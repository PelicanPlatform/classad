package collections

import (
	"encoding/binary"
	"math"
	"sort"
	"strings"
)

// A segment's byte-weighted value profile: what retention needs that a zone map cannot say.
//
// A zone map bounds a segment's values ([min,max]); it cannot say HOW MUCH of the segment
// lies below a given value. Retention needs exactly that. Dropping whole segments oldest-
// first works only while append order tracks the values being retained against, and in a
// hub/spoke archive it does not: a spoke replaying a year of backfill appends records far
// older than the ones already in the oldest segment, so a size-driven pass evicts recent
// data and keeps the backfill.
//
// The profile is an equi-DEPTH quantile vector: sort the segment's records by the attribute,
// walk them accumulating stored bytes, and record the value at every bucketBytes boundary.
// Equi-depth rather than equi-width because the value range is not known in advance -- an
// equi-width histogram over unix seconds degenerates the moment a spoke imports a decade.
//
// breaks[k] is the LARGEST value among the first (k+1) buckets, so every one of those
// (k+1)*bucketBytes bytes has a value <= breaks[k], and the vector is non-decreasing. A
// cutoff T therefore frees AT LEAST bucketBytes for each breaks[k] < T -- a lower bound,
// because the final partial bucket is not represented. A lower bound is the direction that
// matters: a pass that overestimates what it freed stops while still over its ceiling.
//
// Bytes are STORED bytes (recTotalLen), post-codec, because those are what retention frees.
// They are not what a rewrite recovers exactly -- survivors recompress against a different
// dictionary -- so a pass budgets and re-measures rather than trusting the projection.
type segProfile struct {
	attr         string    // the attribute profiled; a profile for another attribute is not usable
	bucketBytes  int64     // stored bytes per breakpoint
	breaks       []float64 // non-decreasing; breaks[k] bounds the first (k+1) buckets
	totalBytes   int64     // stored bytes walked, including unknownBytes
	unknownBytes int64     // bytes of records whose value could not be bounded (see below)
	valueMin     float64   // over the records that HAD a bounded value
	valueMax     float64
	haveValues   bool  // false: no record had a bounded value; min/max/breaks are meaningless
	recvMin      int64 // unix seconds the segment was sealed; 0 = not recorded
	recvMax      int64
}

// UNKNOWN BYTES. computeSegZones drops an attribute's zone entirely if any record holds an
// expression for it, because a range that cannot bound every record must not prune. A
// profile cannot take that route: absence would fall straight back to the positional policy
// this exists to replace. So unbounded records are COUNTED instead, and the eviction policy
// never frees them by value -- they are reclaimable only by the positional fallback, which
// is what stops unknown-valued data from pinning an archive forever.

const (
	segProfileMagic   = 0x53505246 // "SPRF"
	segProfileVersion = 1
	// defaultProfileBucketBytes is the quantile resolution: ~8 breakpoints against the 8 MiB
	// default segment, ~100 bytes of metadata. Finer buys a tighter cutoff and costs bytes in
	// every sidecar; this is the agreed default rather than a tuned one.
	defaultProfileBucketBytes = 1 << 20
	// profileBucketsPerSegment is how many breakpoints a segment should offer. It is what
	// makes a TRIM possible at all: a segment with one breakpoint is all-or-nothing to the
	// cutoff search, so it can only ever be dropped whole, and a store whose records are
	// interleaved (a hub tailing several spokes) has no other kind of segment. Deriving the
	// default bucket from the segment keeps that resolution at any segment size -- and at the
	// default 8 MiB segment it lands exactly on defaultProfileBucketBytes.
	profileBucketsPerSegment = 8
)

// profileAcc accumulates (value, bytes) pairs during a segment walk. It holds one pair per
// record -- an 8 MiB segment of ~1 KB records is ~8k pairs, ~128 KB transient, sorted once
// at seal -- rather than streaming, because equi-depth buckets need the values in order and
// the walk delivers them in append order.
type profileAcc struct {
	attr    string
	pairs   []valueBytes
	unknown int64
	total   int64
}

type valueBytes struct {
	v float64
	n int64
}

// add records one record's contribution. ok=false means the record's value could not be
// bounded (an expression, a sealed/encrypted value, a non-numeric, or absent).
func (p *profileAcc) add(v float64, n int64, ok bool) {
	p.total += n
	if !ok {
		p.unknown += n
		return
	}
	p.pairs = append(p.pairs, valueBytes{v: v, n: n})
}

// finish sorts the accumulated pairs and lays out the equi-depth buckets. Returns nil when
// nothing was walked at all, which every reader takes as "nothing known".
func (p *profileAcc) finish(bucketBytes int64, recvMin, recvMax int64) *segProfile {
	if p == nil || p.total == 0 {
		return nil
	}
	if bucketBytes <= 0 {
		bucketBytes = defaultProfileBucketBytes
	}
	out := &segProfile{
		attr: p.attr, bucketBytes: bucketBytes,
		totalBytes: p.total, unknownBytes: p.unknown,
		recvMin: recvMin, recvMax: recvMax,
	}
	if len(p.pairs) == 0 {
		return out // every record unbounded: counted, never freed by value
	}
	sort.Slice(p.pairs, func(i, j int) bool { return p.pairs[i].v < p.pairs[j].v })
	out.valueMin, out.valueMax = p.pairs[0].v, p.pairs[len(p.pairs)-1].v
	out.haveValues = true

	// A segment holding less than one bucket would close no bucket at all and so offer the
	// cutoff search nothing -- it could only ever be taken whole. Narrow the bucket to what
	// the segment actually holds instead, so it closes exactly one.
	//
	// The bucket size is per-segment for this reason, and every reader already takes it from
	// the profile rather than from configuration, so a narrowed one is credited at its real
	// size. Widening it would be the dangerous direction: a breakpoint credited with more
	// bytes than the segment holds makes a pass believe it freed space it did not.
	var known int64
	for _, pr := range p.pairs {
		known += pr.n
	}
	if known > 0 && known < bucketBytes {
		bucketBytes = known
		out.bucketBytes = known
	}

	// Emit a breakpoint each time the running total crosses a bucket boundary. The value
	// recorded is the one that crossed it, which is the largest in the closed bucket
	// because the walk is in ascending order.
	var run int64
	next := bucketBytes
	for _, pr := range p.pairs {
		run += pr.n
		for run >= next {
			out.breaks = append(out.breaks, pr.v)
			next += bucketBytes
		}
	}
	return out
}

// bytesBelow reports a LOWER BOUND on the stored bytes in the segment whose value is < t,
// and whether the whole segment is below t. The bound counts only closed buckets, so it
// understates by up to bucketBytes plus the final partial bucket.
func (sp *segProfile) bytesBelow(t float64) (bytes int64, whole bool) {
	if sp == nil || !sp.haveValues {
		return 0, false
	}
	// Whole-segment: every bounded value is below t AND nothing is unbounded. A segment
	// holding unknown bytes is never wholly freed by value.
	if sp.valueMax < t && sp.unknownBytes == 0 {
		return sp.totalBytes, true
	}
	n := sort.Search(len(sp.breaks), func(i int) bool { return sp.breaks[i] >= t })
	return int64(n) * sp.bucketBytes, false
}

// buildProfileBlob encodes a profile for the sidecar's profile section. Returns nil for a
// nil profile, which writes no section at all.
func buildProfileBlob(sp *segProfile) []byte {
	if sp == nil {
		return nil
	}
	b := make([]byte, 0, 64+len(sp.attr)+8*len(sp.breaks))
	b = binary.LittleEndian.AppendUint32(b, segProfileMagic)
	b = binary.LittleEndian.AppendUint16(b, segProfileVersion)
	var flags uint16
	if sp.haveValues {
		flags |= 1
	}
	b = binary.LittleEndian.AppendUint16(b, flags)
	b = binary.LittleEndian.AppendUint64(b, uint64(sp.bucketBytes))
	b = binary.LittleEndian.AppendUint64(b, uint64(sp.totalBytes))
	b = binary.LittleEndian.AppendUint64(b, uint64(sp.unknownBytes))
	b = binary.LittleEndian.AppendUint64(b, math.Float64bits(sp.valueMin))
	b = binary.LittleEndian.AppendUint64(b, math.Float64bits(sp.valueMax))
	b = binary.LittleEndian.AppendUint64(b, uint64(sp.recvMin))
	b = binary.LittleEndian.AppendUint64(b, uint64(sp.recvMax))
	b = binary.LittleEndian.AppendUint16(b, uint16(len(sp.attr)))
	b = append(b, sp.attr...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(sp.breaks)))
	for _, v := range sp.breaks {
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(v))
	}
	return b
}

// parseProfileBlob decodes a profile section. A section that is absent, truncated, of an
// unknown version, or whose breakpoints are not non-decreasing yields nil -- "nothing
// known" -- rather than a partial profile. A profile that understates what a segment holds
// makes eviction free less than it computed and stop over its ceiling; one that overstates
// drops records that should have been kept. Neither is detectable after the fact, so a
// section that is not exactly right is not used at all.
func parseProfileBlob(b []byte, wantAttr string) *segProfile {
	const fixed = 4 + 2 + 2 + 8*7 + 2
	if len(b) < fixed || binary.LittleEndian.Uint32(b) != segProfileMagic {
		return nil
	}
	if binary.LittleEndian.Uint16(b[4:]) != segProfileVersion {
		return nil
	}
	sp := &segProfile{
		haveValues:   binary.LittleEndian.Uint16(b[6:])&1 != 0,
		bucketBytes:  int64(binary.LittleEndian.Uint64(b[8:])),
		totalBytes:   int64(binary.LittleEndian.Uint64(b[16:])),
		unknownBytes: int64(binary.LittleEndian.Uint64(b[24:])),
		valueMin:     math.Float64frombits(binary.LittleEndian.Uint64(b[32:])),
		valueMax:     math.Float64frombits(binary.LittleEndian.Uint64(b[40:])),
		recvMin:      int64(binary.LittleEndian.Uint64(b[48:])),
		recvMax:      int64(binary.LittleEndian.Uint64(b[56:])),
	}
	nameLen := int(binary.LittleEndian.Uint16(b[64:]))
	if sp.bucketBytes <= 0 || sp.totalBytes < 0 || sp.unknownBytes < 0 ||
		sp.unknownBytes > sp.totalBytes || len(b) < fixed+nameLen+4 {
		return nil
	}
	sp.attr = string(b[fixed : fixed+nameLen])
	// A profile over a DIFFERENT attribute is not merely useless, it is wrong: its values
	// are in another space entirely. The caller asks for the attribute it is evicting on.
	if wantAttr != "" && !strings.EqualFold(sp.attr, wantAttr) {
		return nil
	}
	at := fixed + nameLen
	n := int(binary.LittleEndian.Uint32(b[at:]))
	at += 4
	if n < 0 || len(b) < at+8*n {
		return nil
	}
	sp.breaks = make([]float64, n)
	for i := 0; i < n; i++ {
		sp.breaks[i] = math.Float64frombits(binary.LittleEndian.Uint64(b[at+8*i:]))
		if i > 0 && sp.breaks[i] < sp.breaks[i-1] {
			return nil // not non-decreasing: the bound the policy rests on does not hold
		}
		// Every breakpoint is a value the segment holds, so it lies within the recorded
		// span. Checked per entry rather than only pairwise: a one-entry vector has no
		// neighbour to be out of order with, and would otherwise accept anything.
		if sp.breaks[i] < sp.valueMin || sp.breaks[i] > sp.valueMax {
			return nil
		}
	}
	if n > 0 && !sp.haveValues {
		return nil // breakpoints without values: internally inconsistent
	}
	return sp
}

// mergeProfiles combines the profiles of the segments a merge or reseal consumed into one for
// the segment it produced. The output holds exactly the input records, so its distribution is
// theirs added together.
//
// It works from the breakpoint vectors rather than the records, because the records are what a
// merge exists to avoid re-reading. Each breakpoint stands for one bucket of data whose values
// are at or below it, so the bucket is re-entered at that value; the bytes past the last
// breakpoint (a segment's final partial bucket) are entered at the segment's own maximum. Both
// place the data at the HIGHEST value it could have, which keeps the result a lower bound on
// what sits below any cutoff -- the direction that makes a pass free at least what it computed.
//
// Returns nil unless every input is a profile of attr: a merged segment with no profile is
// read as "not orderable by value", which the positional rule still reclaims, whereas one
// assembled from profiles of different attributes would order on values from two spaces.
//
// Without this, a merge collapses its inputs' distributions to a single [min,max] -- which is
// the same wart that forces rotation to run BEFORE merging, since a merged segment otherwise
// takes the newest value it absorbed and ages as though all of it were that new.
func mergeProfiles(ps []*segProfile, bucketBytes int64, attr string) *segProfile {
	if len(ps) == 0 || bucketBytes <= 0 {
		return nil
	}
	acc := &profileAcc{attr: attr}
	var recvMin, recvMax int64
	trueMin, trueMax := math.Inf(1), math.Inf(-1)
	for _, p := range ps {
		if p == nil || !strings.EqualFold(p.attr, attr) {
			return nil
		}
		for _, v := range p.breaks {
			acc.add(v, p.bucketBytes, true)
		}
		// The tail past the last closed bucket, entered at the segment's own maximum.
		known := p.totalBytes - p.unknownBytes
		if tail := known - int64(len(p.breaks))*p.bucketBytes; tail > 0 && p.haveValues {
			acc.add(p.valueMax, tail, true)
		}
		if p.unknownBytes > 0 {
			acc.add(0, p.unknownBytes, false)
		}
		if p.haveValues {
			trueMin = math.Min(trueMin, p.valueMin)
			trueMax = math.Max(trueMax, p.valueMax)
		}
		if p.recvMin != 0 && (recvMin == 0 || p.recvMin < recvMin) {
			recvMin = p.recvMin
		}
		if p.recvMax > recvMax {
			recvMax = p.recvMax
		}
	}
	out := acc.finish(bucketBytes, recvMin, recvMax)
	if out == nil {
		return nil
	}
	// The reconstruction above re-enters each bucket at the value that CLOSED it, so the
	// smallest value in the output is the first breakpoint, not the smallest value any input
	// held. The span is reported, so carry the real one rather than the reconstruction's --
	// it costs two comparisons and keeps the profile's own account of itself true.
	if out.haveValues && !math.IsInf(trueMin, 1) {
		out.valueMin, out.valueMax = trueMin, trueMax
	}
	return out
}
