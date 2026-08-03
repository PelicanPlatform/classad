package collections

import (
	"math"
	"math/bits"
	"sort"
)

// Per-index segment statistics. Each segment's index (segIndex) carries, per
// indexed attribute, a compact immutable summary of the values in its indexed
// prefix [0, upto). The summary is computed once at buildSegIndex time from the
// EXACT postings (the postings map already holds every distinct value and its
// exact record count for the prefix), so nothing is estimated that could be
// known cheaply and exactly, and there is no second scan.
//
// Two query uses, matching the two families of fields:
//
//   - Segment SKIP (correctness-critical: must never drop a real match). A probe
//     that provably has no candidate in the prefix lets the query skip the whole
//     indexed prefix and only full-scan the un-indexed tail. Driven by min/max
//     (numeric range/equality) and the bloom filter (categorical membership),
//     each guarded by the exception count (records whose value is present but not
//     the indexed literal type still re-verify, so they forbid a skip).
//
//   - Selectivity ORDERING (heuristic: affects speed, never results). When a
//     query has several indexed probes, applying the most selective first shrinks
//     the roaring intersection fastest. Driven by the undefined fraction, the
//     top-N heavy hitters, and the distinct-value count (NDV).
//
// The bloom filter and HyperLogLog are the compact, postings-free primitives:
// today the live segment keeps its full postings so membership/NDV could be read
// exactly, but the sketches are what a future immutable (postings-dropped) or
// cross-segment-merged summary is built from. NDV is also kept exact per segment
// (free from len(post)); the HLL is the mergeable form for a pool-wide estimate.

const (
	// topNMax bounds the heavy-hitter list kept per attribute (memory-bounded per
	// the design: a handful of the most frequent values drives equality selectivity).
	topNMax = 10

	// hllPrecision sets the HyperLogLog register count to 2^p. p=10 -> 1024
	// one-byte registers (1 KiB/attr/segment), ~3% standard error — a modest budget
	// that still merges across segments for a pool-wide distinct-count estimate.
	hllPrecision = 10
	hllRegisters = 1 << hllPrecision

	// bloomBitsPerKey / bloomMaxBits size the categorical membership filter: ~10
	// bits/distinct-value targets a ~1% false-positive rate (a false positive only
	// forgoes a skip, never drops a match), capped so a high-cardinality attribute
	// cannot blow the memory budget.
	bloomBitsPerKey = 10
	bloomMaxBits    = 1 << 16 // 64 Kib = 8 KiB/attr/segment ceiling
)

// topEntry is one heavy hitter: a value's hash-independent key and its exact
// record count in the indexed prefix. For categorical attributes fkey is unused
// and skey holds the folded string; for value attributes skey is empty and fkey
// holds the number.
type topEntry struct {
	skey  string
	fkey  float64
	count uint32
}

// segStats summarizes one attribute's values in a segment's indexed prefix. All
// fields are written once at build and then read-only, so query readers share it
// lock-free alongside the immutable segIndex.
type segStats struct {
	covered uint64 // records in the prefix that carry this attribute at all (indexable + exceptional)
	exc     uint64 // of covered, those present but not the indexed literal type (forbid skip; re-verified)
	ndv     uint64 // exact distinct indexable values in the prefix (== len(post))

	// Numeric (value index) range. hasRange is false for a categorical attribute or
	// a value attribute with no indexable numeric record.
	min, max float64
	hasRange bool

	top   []topEntry   // up to topNMax heavy hitters, descending by count
	bloom *bloomFilter // categorical membership; nil for a value index
	hll   *hyperLogLog // mergeable distinct-count sketch

	// hist is the equi-depth histogram for a value index's numeric distribution, giving
	// skew-aware range selectivity instead of uniform [min,max] interpolation. nil for a
	// categorical attribute, an empty value index, or a pre-v9 sidecar (falls back to the
	// [min,max] model).
	hist *valHistogram
}

// histMaxBuckets bounds the equi-depth histogram kept per value-index attribute per segment:
// at most this many (value-quantile, cumulative-count) points. 64 gives skew-accurate range
// selectivity at ~1 KiB/attr/segment, on the order of the HLL's footprint.
const histMaxBuckets = 64

// valHistogram is an equi-depth (equi-height) histogram over a value index's numeric
// distribution, built exactly from the segment's postings. It replaces the uniform [min,max]
// assumption for range selectivity: linear interpolation badly mis-estimates skewed data
// (e.g. a few huge-Memory nodes stretch max, so `Memory > 4096` estimates ~99% when the truth
// is ~40%), while the histogram's buckets concentrate where the data is dense. lo is the
// smallest value (bucket 0's lower edge); bound[i] is the largest value in bucket i
// (ascending); cum[i] is the count of indexable records with value <= bound[i] (ascending;
// cum[last] == total indexable).
type valHistogram struct {
	lo    float64
	bound []float64
	cum   []uint64
}

// buildValHistogram builds an equi-depth histogram from a value index's ascending distinct
// keys and their exact per-key record counts. Returns nil for an empty distribution. With
// fewer distinct values than buckets it degenerates to one bucket per value -- an exact CDF.
// A single key whose count exceeds the per-bucket target simply forms one heavy bucket (a
// value cannot be split), and buckets resume at ~target records after it.
func buildValHistogram(sortedKeys []float64, counts []uint64, indexable uint64) *valHistogram {
	if len(sortedKeys) == 0 || indexable == 0 {
		return nil
	}
	target := float64(indexable) / float64(histMaxBuckets)
	if target < 1 {
		target = 1
	}
	h := &valHistogram{lo: sortedKeys[0], bound: make([]float64, 0, histMaxBuckets), cum: make([]uint64, 0, histMaxBuckets)}
	var running uint64
	edge := target
	last := len(sortedKeys) - 1
	for i, k := range sortedKeys {
		running += counts[i]
		if float64(running) >= edge || i == last {
			h.bound = append(h.bound, k)
			h.cum = append(h.cum, running)
			edge = float64(running) + target
		}
	}
	return h
}

// cdf estimates the fraction of indexable records with value <= t, by piecewise-linear
// interpolation of the cumulative counts across the bucket the threshold falls in.
func (h *valHistogram) cdf(t float64) float64 {
	n := len(h.bound)
	total := float64(h.cum[n-1])
	if total <= 0 {
		return 0
	}
	if t < h.lo {
		return 0
	}
	if t >= h.bound[n-1] {
		return 1
	}
	i := sort.Search(n, func(j int) bool { return h.bound[j] >= t }) // first bucket top >= t
	loB, cumPrev := h.lo, 0.0
	if i > 0 {
		loB, cumPrev = h.bound[i-1], float64(h.cum[i-1])
	}
	// Interpolate within the bucket. A zero-span bucket only occurs at bucket 0 when the low
	// value equals the first boundary; leaving frac at 0 keeps cdf(min)=0, so `>= min` reads
	// ~100% (matching the previous uniform model) rather than treating the min as a point mass
	// -- estRange conflates `>`/`>=`, so a point mass at t would mis-score the inclusive side.
	frac := 0.0
	if span := h.bound[i] - loB; span > 0 {
		frac = (t - loB) / span
	}
	return (cumPrev + frac*(float64(h.cum[i])-cumPrev)) / total
}

// estRange returns the estimated fraction of indexable records passing op against t.
func (h *valHistogram) estRange(op string, t float64) float64 {
	f := h.cdf(t)
	switch op {
	case "<", "<=":
		return math.Max(0, math.Min(1, f))
	case ">", ">=":
		return math.Max(0, math.Min(1, 1-f))
	}
	return 1
}

// avgTailCount estimates the record count of a value that is NOT one of the kept
// heavy hitters: the records not covered by the top-N spread evenly over the
// remaining distinct values. Used to estimate equality selectivity for a probe
// value absent from the top-N.
func (s *segStats) avgTailCount() float64 {
	tailNDV := int(s.ndv) - len(s.top)
	if tailNDV <= 0 {
		return 1
	}
	var topSum uint64
	for _, e := range s.top {
		topSum += uint64(e.count)
	}
	indexable := s.covered - s.exc
	if topSum >= indexable {
		return 1
	}
	return math.Max(1, float64(indexable-topSum)/float64(tailNDV))
}

// finishValStats fills the numeric-range, top-N, NDV and HLL fields from a value
// index's completed postings. Called once at the end of buildSegIndex.
func (vp *valPostings) finishStats() {
	s := &vp.stats
	s.exc = vp.exc.GetCardinality()
	s.ndv = uint64(len(vp.post))
	s.hll = newHLL()
	var indexable uint64
	first := true
	entries := make([]topEntry, 0, len(vp.post))
	for k, bm := range vp.post {
		c := bm.GetCardinality()
		indexable += c
		if first || k < s.min {
			s.min = k
		}
		if first || k > s.max {
			s.max = k
		}
		first = false
		s.hll.addHash(hashFloat(k))
		entries = append(entries, topEntry{fkey: k, count: uint32(c)})
	}
	if !first {
		s.hasRange = true
	}
	s.covered = indexable + s.exc
	s.top = topEntries(entries)
	// Sorted keys for range boundary search (probeOffsets `<`,`<=`,`>`,`>=`).
	vp.sortedKeys = make([]float64, 0, len(vp.post))
	for k := range vp.post {
		vp.sortedKeys = append(vp.sortedKeys, k)
	}
	sort.Float64s(vp.sortedKeys)
	// Equi-depth histogram over the exact (value, count) distribution, for skew-aware range
	// selectivity (replaces uniform [min,max] interpolation in estRange).
	counts := make([]uint64, len(vp.sortedKeys))
	for i, k := range vp.sortedKeys {
		counts[i] = vp.post[k].GetCardinality()
	}
	s.hist = buildValHistogram(vp.sortedKeys, counts, indexable)
}

// finishStats fills the categorical top-N, NDV, bloom and HLL fields from a
// categorical index's completed postings. Called once at the end of buildSegIndex.
func (cp *catPostings) finishStats() {
	s := &cp.stats
	s.exc = cp.exc.GetCardinality()
	s.ndv = uint64(len(cp.post))
	s.hll = newHLL()
	s.bloom = newBloom(len(cp.post))
	var indexable uint64
	entries := make([]topEntry, 0, len(cp.post))
	for k, bm := range cp.post {
		c := bm.GetCardinality()
		indexable += c
		h := hashString(k)
		s.hll.addHash(h)
		s.bloom.addHash(h)
		entries = append(entries, topEntry{skey: k, count: uint32(c)})
	}
	s.covered = indexable + s.exc
	s.top = topEntries(entries)
}

// topEntries returns the topNMax highest-count entries, descending by count
// (value as a deterministic tie-break so a rebuild is reproducible).
func topEntries(entries []topEntry) []topEntry {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		if entries[i].skey != entries[j].skey {
			return entries[i].skey < entries[j].skey
		}
		return entries[i].fkey < entries[j].fkey
	})
	if len(entries) > topNMax {
		entries = entries[:topNMax]
	}
	return entries
}

// sketchBytes is the resident memory of this attribute's per-segment sketches: the
// categorical bloom filter (uint64 words) and the HyperLogLog register array (one byte
// per register). These live alongside the postings but are NOT counted by the posting
// sizeBytes; IndexSizes reports them as a separate SketchBytes column so the operator
// sees the full index memory rather than only the roaring bitmaps.
func (s *segStats) sketchBytes() int64 {
	var n int64
	if s.bloom != nil {
		n += int64(len(s.bloom.bits)) * 8
	}
	if s.hll != nil {
		n += int64(len(s.hll.reg))
	}
	return n
}

// --- bloom filter (categorical membership) -------------------------------------

// bloomFilter is a compact Bloom filter over 64-bit value hashes. Built once from
// a categorical index's key set, then read-only. A miss ("definitely absent")
// lets a query skip a segment prefix for an equality/in probe; a hit may be a
// false positive (only forgoing a skip, never dropping a match).
type bloomFilter struct {
	bits []uint64
	m    uint32 // number of bits (a power of two, so index = hash & (m-1))
	k    uint32 // number of hash probes
}

// newBloom sizes a filter for n distinct keys at ~bloomBitsPerKey bits/key,
// rounded up to a power of two and capped at bloomMaxBits.
func newBloom(n int) *bloomFilter {
	targetBits := n * bloomBitsPerKey
	if targetBits < 64 {
		targetBits = 64
	}
	if targetBits > bloomMaxBits {
		targetBits = bloomMaxBits
	}
	m := uint32(1) << bits.Len(uint(targetBits-1)) // next power of two >= targetBits
	// k = round(m/n * ln2), clamped to [1, 8].
	k := uint32(1)
	if n > 0 {
		k = uint32(math.Round(float64(m) / float64(n) * math.Ln2))
	}
	if k < 1 {
		k = 1
	}
	if k > 8 {
		k = 8
	}
	return &bloomFilter{bits: make([]uint64, m/64), m: m, k: k}
}

// addHash sets the k bit positions for a value's 64-bit hash using double hashing
// (two 32-bit halves), the standard Kirsch–Mitzenmacher construction.
func (b *bloomFilter) addHash(h uint64) {
	h1, h2 := uint32(h), uint32(h>>32)
	for i := uint32(0); i < b.k; i++ {
		p := (h1 + i*h2) & (b.m - 1)
		b.bits[p>>6] |= 1 << (p & 63)
	}
}

// mayContain reports whether the hash might be present (false = definitely absent).
func (b *bloomFilter) mayContain(h uint64) bool {
	h1, h2 := uint32(h), uint32(h>>32)
	for i := uint32(0); i < b.k; i++ {
		p := (h1 + i*h2) & (b.m - 1)
		if b.bits[p>>6]&(1<<(p&63)) == 0 {
			return false
		}
	}
	return true
}

// --- HyperLogLog (mergeable distinct-count) ------------------------------------

// hyperLogLog estimates the number of distinct values from their hashes with a
// bounded, mergeable register array. Kept alongside the exact per-segment NDV so a
// planner can merge segment sketches into a pool-wide distinct-count estimate
// (union of segments) without double-counting shared values.
type hyperLogLog struct {
	reg []uint8 // hllRegisters registers, each the max leading-zero run + 1 seen for its bucket
}

func newHLL() *hyperLogLog { return &hyperLogLog{reg: make([]uint8, hllRegisters)} }

// addHash folds one value's 64-bit hash into the sketch: the top hllPrecision bits
// pick the register, the rest's leading-zero run sizes the estimate.
func (h *hyperLogLog) addHash(x uint64) {
	idx := x >> (64 - hllPrecision)
	w := (x << hllPrecision) | (1 << (hllPrecision - 1)) // guard bit bounds the zero run
	rank := uint8(bits.LeadingZeros64(w)) + 1
	if rank > h.reg[idx] {
		h.reg[idx] = rank
	}
}

// merge folds another sketch into this one (register-wise max). Both must share
// the precision (they do — it is a package constant).
func (h *hyperLogLog) merge(o *hyperLogLog) {
	for i := range h.reg {
		if o.reg[i] > h.reg[i] {
			h.reg[i] = o.reg[i]
		}
	}
}

// estimate returns the approximate distinct count (raw HLL with small-range
// linear-counting correction).
func (h *hyperLogLog) estimate() float64 {
	m := float64(hllRegisters)
	var sum float64
	var zeros int
	for _, r := range h.reg {
		sum += 1.0 / float64(uint64(1)<<r)
		if r == 0 {
			zeros++
		}
	}
	est := hllAlpha * m * m / sum
	if est <= 2.5*m && zeros > 0 { // small-range: linear counting is more accurate
		return m * math.Log(m/float64(zeros))
	}
	return est
}

// hllAlpha is the bias-correction constant for m = 1024 registers.
const hllAlpha = 0.7213 / (1 + 1.079/hllRegisters)

// --- value hashing -------------------------------------------------------------

// hashString is a 64-bit FNV-1a hash of a folded categorical key, finalized with
// an avalanche so ALL bits are well mixed. The finalizer matters: the HLL and
// bloom take the register index from the TOP bits, and raw FNV-1a mixes its high
// bits poorly for short similar keys ("v0".."v9"), collapsing them into the same
// bucket. Kept local (not hash/fnv) so both sketches share one cheap alloc-free hash.
func hashString(s string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return mix64(h)
}

// hashFloat hashes a numeric index key through the same avalanche so adjacent
// integers land in different HLL buckets.
func hashFloat(f float64) uint64 { return mix64(math.Float64bits(f)) }

// mix64 is the splitmix64 finalizer: a bijective avalanche that spreads every
// input bit across all 64 output bits.
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}
