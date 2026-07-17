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
