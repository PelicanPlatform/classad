package collections

import (
	"math"
	"slices"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// Append-only recompression. The mutable store recompresses (RetrainDict) and repacks
// (Rewrite) via compaction -- a live-copy that supersedes old versions, renumbers
// segments, and rebuilds the key directory. None of that is legal for an append log,
// where every record is permanently live and order is meaningful. Instead each sealed
// segment is *resealed in place*: its records are decoded, re-encoded with the current
// hot set, recompressed under a target codec, and written to a fresh same-id segment
// file, preserving append order (seq, key, and time markers). The new segment is swapped
// for the old under the shard lock; the old file is unlinked once in-flight scans drop
// their pins (the standard retire/reap path). Indexes are rebuilt afterward by Reindex.

// resealAppendOnly rebuilds every segment of an append-only collection under targetCodec
// and the current hot set, preserving append order, then rebuilds indexes. It is the
// shared engine for append-only RetrainDict (targetCodec = the new-dictionary codec) and
// Rewrite (targetCodec = the current codec, to apply a hot-set change or repack). The
// caller holds maintMu.
func (c *Collection) resealAppendOnly(targetCodec Codec) {
	for _, sh := range c.shards {
		// Retire the active segment from the write path so it becomes immutable and is
		// resealed too; the next write allocates a fresh active segment under the current
		// codec. Snapshot the segments to reseal under the same lock.
		sh.mu.Lock()
		var srcs []*segment
		for _, s := range sh.segs {
			if s != nil {
				srcs = append(srcs, s)
			}
		}
		sh.act = nil
		sh.mu.Unlock()

		var toReap []*segment
		for _, src := range srcs {
			newseg := c.resealOneSegment(sh, src, targetCodec)
			if newseg == nil {
				continue // reseal failed: leave the original in place (best-effort)
			}
			sh.mu.Lock()
			// Swap only if the source still occupies its slot (append-only segments are
			// not otherwise mutated, but stay defensive against a concurrent reseal).
			if int(src.id) < len(sh.segs) && sh.segs[src.id] == src {
				sh.segs[src.id] = newseg
				if src.retire() {
					toReap = append(toReap, src)
				}
			} else {
				// Slot moved under us: drop the freshly built segment's file rather than
				// leak it.
				newseg.retire()
				newseg.reapAndHook() //nolint:errcheck // best effort: the segment is already out of service, so a failed munmap or unlink leaks a file rather than corrupting one
			}
			sh.mu.Unlock()
		}
		for _, seg := range toReap {
			seg.reapAndHook() //nolint:errcheck // munmap + unlink the old segment file, off-lock
		}
	}
	// Rebuild the per-segment indexes (and seal sidecars) over the fresh segments, and
	// drop any dictionary no live segment references anymore.
	c.reindexAfterCompaction()
	c.pruneDicts()
}

// InternSealed transcodes every sealed, still-inline segment of a persistent append-only
// collection to interned form under the current codec, swapping each in and rebuilding its
// indexes. It is the eager complement to interning-at-compaction (resealAppendOnly): an archive
// that never retrains still gets the density/decode win as soon as segments seal. Idempotent --
// already-interned segments and the active write target are skipped, so re-calling is cheap and
// it composes with a later RetrainDict. Append-only only: a mutable segment's records can be
// superseded in place after seal, which this off-lock transcode would race (there interning rides
// compaction, which reconciles supersedes in a final critical section). Holds maintMu, so it
// serializes against Compact/RetrainDict/Rotate/Rewrite. Called opt-in from the Archive.Append
// eager-seal hook (Options.InternAtSeal) and available as a manual maintenance pass.
func (c *Collection) InternSealed() {
	if !c.inline || !c.appendOnly() {
		return // interning-at-seal is an append-only, persistent-collection operation
	}
	c.maintMu.Lock()
	defer c.maintMu.Unlock()
	c.internSealedLocked()
}

// internSkippedDictUnusable counts segments interning passed over because their dictionary is
// unusable, once per segment.
var internSkippedDictUnusable atomic.Int64

// InternSkippedDictUnusable reports how many segments, since the process started, InternSealed (and
// RetrainDict, which runs it) has passed over because their attribute dictionary is unusable
// (SegDictsUnusable): their records are interned, so there is nothing to transcode, and none of them
// can be read to rewrite. Each segment counts once, however many passes skip it.
func InternSkippedDictUnusable() int64 { return internSkippedDictUnusable.Load() }

// internSealedLocked is InternSealed's body, for callers already holding maintMu (RetrainDict
// runs it so a retrain still interns whatever has sealed since the last one -- a one-way,
// once-per-segment transition, unlike recompression, which would otherwise repeat over the
// whole archive on every retrain).
func (c *Collection) internSealedLocked() {
	if !c.inline || !c.appendOnly() {
		return
	}
	codec := c.currentCodec()
	swapped := false
	for _, sh := range c.shards {
		sh.mu.Lock()
		act := sh.act
		var srcs []*segment
		for _, s := range sh.segs {
			if s == nil || s == act || s.used == 0 || s.dict.Load() != nil {
				continue
			}
			// No published dictionary does not make a segment inline: one whose dictionary failed
			// its checksum, or was not recovered, is interned with nothing to resolve its ids. None
			// of its records decode, so the transcode fails on the first one, and it used to be
			// retried on every pass.
			if s.dictUnusable.Load() {
				if s.internSkipCounted.CompareAndSwap(false, true) {
					internSkippedDictUnusable.Add(1)
				}
				continue
			}
			srcs = append(srcs, s) // sealed + still inline: a transcode candidate
		}
		sh.mu.Unlock()

		var toReap []*segment
		for _, src := range srcs {
			newseg := c.resealOneSegment(sh, src, codec) // interns (c.inline); reads src off-lock
			if newseg == nil {
				continue // transcode failed: leave the inline original in place (best-effort)
			}
			sh.mu.Lock()
			// Swap only if the source still occupies its slot and is not the (new) active
			// target -- defensive against a concurrent reseal/rotate.
			if int(src.id) < len(sh.segs) && sh.segs[src.id] == src && sh.act != src {
				sh.segs[src.id] = newseg
				swapped = true
				if src.retire() {
					toReap = append(toReap, src)
				}
			} else {
				newseg.retire()
				newseg.reapAndHook() //nolint:errcheck // slot moved under us: drop the fresh file, don't leak it
			}
			sh.mu.Unlock()
		}
		for _, seg := range toReap {
			seg.reapAndHook() //nolint:errcheck // munmap + unlink the old inline file, off-lock
		}
	}
	if swapped {
		c.reindexAfterCompaction() // rebuild sidecars over the interned segments (takes reindexMu)
		c.pruneDicts()
	}
}

// resealEntry is one record staged for reseal: a data record (its re-encoded, recompressed
// stored bytes and key) or a time-checkpoint marker (marker=true, millis set).
type resealEntry struct {
	seq    uint64
	key    []byte
	stored []byte
	marker bool
	millis uint64
}

// resealOneSegment builds a new segment holding src's records re-encoded with the current
// hot set and recompressed under targetCodec, preserving order. It returns the new segment
// (with the same logical id as src) or nil on any error. The source is immutable (sealed
// or retired from the write path), so it is read without the shard lock.
func (c *Collection) resealOneSegment(sh *shard, src *segment, targetCodec Codec) *segment {
	return c.resealSegments(sh, []*segment{src}, targetCodec)
}

// resealSegments builds ONE new segment holding every record of srcs, in order, re-encoded
// under targetCodec and the current hot set. With a single source it is a reseal in place;
// with several adjacent ones it is a merge, which is how an archive keeps its segment count
// (and therefore its mmap count) bounded as it grows.
//
// The new segment takes srcs[0]'s id, so it keeps the lowest source's position: segment order
// is append order, and on reopen ids are re-derived from file order, so the merged file must
// sort where its oldest input did. Record seq, key, and time markers are carried across
// unchanged, which is what keeps watch cursors (a commit-sequence vector, not a physical
// position) valid over a merge.
//
// Returns nil on any failure, leaving the caller to keep the originals -- merging is
// best-effort maintenance, never a correctness dependency.
func (c *Collection) resealSegments(sh *shard, srcs []*segment, targetCodec Codec) *segment {
	return c.resealSegmentsAs(sh, srcs, targetCodec, "seg")
}

// resealSegmentsAs is resealSegments writing its output under an explicit file-name prefix,
// so a merge can stage the result where recovery will not see it until it is renamed.
func (c *Collection) resealSegmentsAs(sh *shard, srcs []*segment, targetCodec Codec, prefix string) *segment {
	out, _ := c.resealSegmentsFiltered(sh, srcs, targetCodec, prefix, nil)
	return out
}

// resealSegmentsFiltered is resealSegmentsAs with an optional record filter: keep decides,
// from the decoded ad, whether a record is carried into the output. It returns how many
// records it left behind.
//
// A filtered reseal is how retention trims a segment that holds data on both sides of an
// eviction cutoff -- the segment cannot be dropped whole, so the part below the cutoff is
// rewritten away. It goes through the same staging and swap as a merge, because the hard part
// is not the rewrite but being crash-safe about the replacement.
//
// Unlike an unfiltered reseal, the output does NOT hold its sources' records, so it does not
// inherit their zone map or profile: both are recomputed from what was actually written. A
// profile inherited across a filter would report bytes the output no longer holds, and a pass
// reading it would believe it could free space that is already gone.
func (c *Collection) resealSegmentsFiltered(sh *shard, srcs []*segment, targetCodec Codec, prefix string, keep *resealFilter) (*segment, int) {
	filtered := 0
	live := srcs[:0:0]
	for _, s := range srcs {
		if s != nil && s.used > 0 {
			live = append(live, s)
		}
	}
	srcs = live
	if len(srcs) == 0 {
		return nil, 0
	}
	for _, s := range srcs {
		if !verifyForRewrite(s) {
			return nil, 0 // damaged framing: leave the sources as they are
		}
	}
	// A persistent collection reseals INTERNED: records carry segment-local ids against a
	// per-segment table + a hot header, and the segment gets a dictionary appended at the end
	// (see segdict.go / the interning design). Encryption at rest composes -- encodeInterned seals
	// the designated values while interning the rest. Only an in-memory collection (already
	// global-interned, c.inline false) reseals via c.encodeAd. The two-pass build means the table
	// is fully populated before the dict is sized, so the segment is sized EXACTLY -- no dict
	// reserve/waste (unlike the streaming compaction transcode).
	intern := c.inline
	var table *wire.InternTable
	hot := map[uint32]struct{}{}
	lastLen := 0
	refreshHot := func() { // add local ids of hot attr names now in the table (case-preserving)
		if table.Len() == lastLen {
			return
		}
		for name := range c.currentHotNames() {
			if id, ok := table.LookupID(name); ok {
				hot[id] = struct{}{}
			}
		}
		lastLen = table.Len()
	}
	if intern {
		table = wire.NewInternTable()
	}
	// Pass 1: decode + re-encode + recompress every record, computing the exact size the
	// new segment needs so it fits in a single segment (append order is 1:1).
	var entries []resealEntry
	total, dropped := 0, 0
	var wireBuf []byte
	for _, src := range srcs {
		for off := 0; off < src.used; {
			o := uint32(off)
			if end, ok := src.damaged().skip(o); ok {
				off = int(end)
				continue
			}
			rl := recTotalLen(src.data, o)
			if rl == 0 {
				break
			}
			if recIsMarker(src.data, o) {
				// The source's dictionary and columnar records are not carried: the output gets
				// a dictionary of its own and is written whole. Carried as markers, each became a
				// sequence-0 time checkpoint holding its body's first bytes as a time, one more on
				// every reseal of the segment.
				if !recIsTimeMarker(src.data, o) {
					off += int(rl)
					continue
				}
				seq := recSeq(src.data, o)
				millis := recMarkerMillis(src.data, o)
				entries = append(entries, resealEntry{seq: seq, marker: true, millis: millis})
				total += recordLen(0, 8)
				off += int(rl)
				continue
			}
			seq := recSeq(src.data, o)
			// A reseal DECODES each record and encodes it afresh, which a delta cannot survive:
			// it holds only what one write changed, and re-encoding its fragment produces
			// something a reader believes is a whole ad. An append-only shard never writes one
			// (a delta needs a per-key current version, which an append log has no notion of).
			//
			// A LIVE one refuses the reseal rather than rewrite the data wrongly. A SUPERSEDED one
			// is dropped, as compaction drops it: nothing reads it once the key's current version
			// is a whole record, and the caller that reaches here with a mutable segment (the
			// sealed-attribute migration) establishes that for the whole shard first. Refusing it
			// instead refused nearly every segment of a delta store, since a sealed segment holds
			// the superseded fragments its collapse left behind.
			if recIsDelta(src.data, o) {
				if recSuperseded(src.data, o) == seqMax {
					return nil, 0
				}
				resealDroppedDeltas.Add(1)
				dropped++
				off += int(rl)
				continue
			}
			key := append([]byte(nil), recKey(src.data, o)...)
			// The FULL ad: a columnarized source holds only part of each record, and the rest
			// lives in a columnar payload that is not carried across. Reading it whole here
			// de-columnarizes into the rebuilt segment, which a later maintenance pass can
			// columnarize again -- losing an accelerator is fine, losing attributes is not.
			wireBytes, err := c.recordWireIn(src, src.data, o, nil)
			if err != nil {
				return nil, 0
			}
			if keep != nil && !keep.keeps(wire.Ad(wireBytes), src.dict.Load(), sh.zoneInline) {
				filtered++
				off += int(rl)
				continue
			}
			ad, err := c.decodeWireDict(src.dict.Load(), wireBytes) // honor the source's own encoding
			if err != nil {
				return nil, 0
			}
			// A value that will not seal refuses the reseal, as a record that will not decode does:
			// the segment is left as it is rather than rebuilt with that value in the clear.
			var stored []byte
			if intern {
				if wireBuf, err = c.encodeInterned(wireBuf[:0], ad, table, hot); err != nil {
					return nil, 0
				}
				refreshHot()
				stored = targetCodec.Compress(nil, wireBuf)
			} else {
				raw, err := c.encodeAd(ad)
				if err != nil {
					return nil, 0
				}
				stored = targetCodec.Compress(nil, raw)
			}
			entries = append(entries, resealEntry{seq: seq, key: key, stored: stored})
			total += recordLen(len(key), len(stored))
			off += int(rl)
		}
	}
	// Nothing to write is a failure, except for an interned segment whose every record was a dropped
	// delta -- the state a collapse leaves a segment in. That one is rebuilt holding only its
	// dictionary; refusing it left the segment, and whatever it held, in place for good.
	if total == 0 && (dropped == 0 || !intern) {
		return nil, 0
	}
	// The dict (interned only) is a trailing record; size the segment to include it exactly.
	var dictBytes []byte
	if intern {
		dictBytes = appendSegDict(nil, table.Names())
		total += recordLen(0, len(dictBytes))
	}

	// Allocate a new same-id segment sized to the recompressed data (rounded up by the
	// allocator). sh.alloc names the file by the codec's dictionary id; a nil alloc
	// (in-memory collection) yields a RAM segment.
	size := recAlign(total)
	var newseg *segment
	if sh.alloc == nil {
		newseg = newSegment(srcs[0].id, size, targetCodec)
		newseg.pinReap = sh.sealRAM
	} else {
		s, err := sh.allocNamed(srcs[0].id, size, targetCodec, prefix)
		if err != nil {
			return nil, 0
		}
		newseg = s
	}

	// Pass 2: append the staged records in order.
	for _, e := range entries {
		if e.marker {
			if !newseg.appendMarker(e.seq, e.millis) {
				return nil, 0
			}
			continue
		}
		if _, ok := newseg.append(e.seq, noLoc, e.key, e.stored); !ok {
			return nil, 0
		}
	}
	// Interned: append the dictionary as a trailing record and publish the segment's dict so its
	// records resolve their segment-local ids.
	if intern {
		doff, ok := newseg.appendDict(dictBytes)
		if !ok {
			return nil, 0
		}
		newseg.dict.Store(&segDictHandle{data: newseg.data, base: doff + recKeyOff + 4, rec: doff})
	}
	// The zone map, before the segment is published: a reader prunes on it, retention ages
	// segments out by it, and the sidecar the post-swap reindex writes persists it -- so a
	// segment published without one is scanned in full and never aged out, until a reopen
	// recomputes it from every record.
	if keep == nil {
		newseg.zones = c.rewriteZones(sh, srcs, newseg)
		// The retention profile travels with the zone map, and for the same reason: a rebuilt
		// segment published without one is never aged out by value, only by position.
		newseg.profile = c.rewriteProfile(sh, srcs, newseg)
	} else {
		// A filtered output holds fewer records than its source, so neither the union of the
		// sources' zones nor the merge of their profiles describes it. Both are computed from
		// the segment that was actually written.
		newseg.zones, newseg.profile = c.zonesAndProfileFor(sh, newseg)
	}
	// Persistent segments: flush the written extent so recovery sees a complete segment.
	if newseg.persistent {
		newseg.synced = newseg.used
		if err := newseg.flush(); err != nil {
			return nil, 0
		}
	}
	return newseg, filtered
}

// rewriteProfile returns the retention profile for out, a segment resealSegmentsAs rebuilt from
// srcs. out holds exactly the records of srcs with the same values, so its distribution is
// theirs combined -- which mergeProfiles assembles from their breakpoint vectors rather than by
// re-reading records, since not re-reading them is what a merge is for.
//
// A source without a usable profile (never profiled, or profiled over a different attribute)
// leaves the merge nothing to assemble from, so the profile is computed from out's records
// instead, as rewriteZones does for a source without a zone map. It is never assembled from
// the sources that DO have one: a profile that accounts for only some of a segment's records
// would under-report what it holds above a cutoff, and that is the error that drops records
// which should have been kept. The recompute costs one decode of the output, and it is paid
// here because nothing later rebuilds a missing profile -- without it a merge that absorbed
// one unprofiled segment would be reclaimable only by position for good.
func (c *Collection) rewriteProfile(sh *shard, srcs []*segment, out *segment) *segProfile {
	attr := sh.profileAttr()
	if attr == "" {
		return nil
	}
	sh.mu.RLock()
	ps := make([]*segProfile, len(srcs))
	for i, s := range srcs {
		ps[i] = s.profile
	}
	bucket := sh.profileBucketBytes()
	sh.mu.RUnlock()
	if p := mergeProfiles(ps, bucket, attr); p != nil {
		return p
	}
	_, p := c.zonesAndProfileFor(sh, out)
	return p
}

// rewriteZones returns the zone map for out, a segment resealSegmentsAs rebuilt from srcs, or nil
// when the shard keeps none (it is not append-only, or no attribute is zoned).
//
// out holds exactly the records of srcs, with the same values: a reseal re-encodes each ad (its
// attribute ids become segment-local, its codec may change) but does not change what any value
// is, and both it and the sources' zone computation leave out the same damaged spans. So when
// every source carries a zone map, out's is their union -- per attribute, the widest range,
// full only where every source's was -- over the attributes every source has a zone for; an
// attribute one source has none for (an expression, no number, or zoned after it sealed) has
// none in out either. The maps are keyed by this process's ids, not the records' encoding, so
// the segment-local ids out's records use do not enter into it. A source without a zone map has
// it recomputed from out's records instead.
func (c *Collection) rewriteZones(sh *shard, srcs []*segment, out *segment) map[uint32]zoneRange {
	sh.mu.RLock()
	attrs, inline, appendOnly := sh.zoneAttrs, sh.zoneInline, sh.appendOnly
	zs := make([]map[uint32]zoneRange, len(srcs))
	for i, s := range srcs {
		zs[i] = s.zones // replaced, never mutated, under the shard lock (rezoneSealed)
	}
	sh.mu.RUnlock()
	if !appendOnly || len(attrs) == 0 {
		return nil
	}
	if slices.ContainsFunc(zs, func(z map[uint32]zoneRange) bool { return z == nil }) {
		return computeSegZones(c, out, out.used, attrs, inline, sh.sealsName)
	}
	// A source zoned before an attribute was sealed carries a range of its clear values; the rewrite
	// seals them, so the output keeps no zone for it (see computeSegZones).
	var sealedIDs map[uint32]struct{}
	if sh.sealsName != nil {
		for _, za := range attrs {
			if sh.sealsName(za.name) {
				if sealedIDs == nil {
					sealedIDs = map[uint32]struct{}{}
				}
				sealedIDs[za.id] = struct{}{}
			}
		}
	}
	merged := make(map[uint32]zoneRange, len(zs[0]))
	for id, z := range zs[0] {
		if _, sealed := sealedIDs[id]; sealed {
			continue
		}
		ok := true
		for _, other := range zs[1:] {
			o, has := other[id]
			if !has {
				ok = false
				break
			}
			z.Min, z.Max, z.Full = math.Min(z.Min, o.Min), math.Max(z.Max, o.Max), z.Full && o.Full
		}
		if ok {
			merged[id] = z
		}
	}
	return merged
}

// resealFilter decides which records a filtered reseal carries. It reads the attribute
// through the same lookup and classification the zone map and the retention profile use
// (lookupZoneNode + zoneValue), so a trim and the profile that chose it cannot disagree
// about what a record's value is.
type resealFilter struct {
	attr   zoneAttr
	cutoff float64
}

// keeps reports whether a record survives the trim. A record whose value is BELOW the
// cutoff is dropped; everything else is kept, and that deliberately includes records whose
// value cannot be bounded -- an expression, a sealed value, a non-numeric, or the attribute
// missing entirely. Those are the profile's "unknown bytes", which it never offers up for
// eviction, so the trim must not remove them either: nothing has established that they are
// below the cutoff, and a dropped record cannot be recovered to find out.
func (f *resealFilter) keeps(ad wire.Ad, dict *segDictHandle, inline bool) bool {
	node, ok := lookupZoneNode(ad, f.attr, dict, inline)
	if !ok {
		return true
	}
	v, cell := zoneValue(node)
	if cell != zoneNumber {
		return true
	}
	return v >= f.cutoff
}

// zonesAndProfileFor computes a rebuilt segment's zone map and retention profile from the
// segment itself. It is the path a FILTERED reseal takes: its output does not hold its
// sources' records, so nothing about the sources describes it.
func (c *Collection) zonesAndProfileFor(sh *shard, out *segment) (map[uint32]zoneRange, *segProfile) {
	sh.mu.RLock()
	attrs, inline, appendOnly := sh.zoneAttrs, sh.zoneInline, sh.appendOnly
	bucket := sh.profileBucketBytes()
	sh.mu.RUnlock()
	if !appendOnly || len(attrs) == 0 {
		return nil, nil
	}
	var prof *profileAcc
	if attr := sh.profileAttr(); attr != "" {
		prof = &profileAcc{attr: attr}
	}
	zones := computeSegZonesProfiled(c, out, out.used, attrs, inline, sh.sealsName, prof)
	if prof == nil {
		return zones, nil
	}
	now := int64(nowMillis() / 1000)
	return zones, prof.finish(bucket, now, now)
}
