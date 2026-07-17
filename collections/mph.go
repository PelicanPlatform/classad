package collections

import "math/bits"

// Minimal perfect hash (BBHash) over an immutable, distinct key set, for O(1) equality on
// the mmap index tier. On a cold, high-cardinality sidecar an equality probe otherwise
// binary-searches the sorted key blob -- O(log n) comparisons, each a potential page fault
// into a scattered part of the mapping. The MPH resolves a member to its slot in ~1 probe.
//
// BBHash is a cascade of bit arrays ("levels"): at each level a key hashes to a bit; a bit
// that exactly one still-unplaced key lands on is "assigned" (set), and colliding keys fall
// through to the next level. A key's slot is the rank (count of set bits before its bit)
// across the levels, giving a dense [0, nAssigned) numbering. Lookups are a bit test plus a
// popcount-rank, so the serialized form probes zero-copy over the mmap.
//
// SAFETY: the MPH is a pure fast path, never authoritative. A member always resolves to its
// own slot; a non-member may resolve to some slot (a false hit) -- so the caller MUST verify
// the key stored at that slot and, on any miss (unassigned member, verify mismatch,
// non-member), fall back to the sorted-run binary search. A build bug can therefore only
// cost a redundant binary search, never a wrong answer.

const (
	mphGamma          = 2 // bits per still-unplaced key at each level (load factor 1/gamma)
	mphMaxLevels      = 7 // after this, any leftover keys are simply unassigned (caller falls back)
	mphRankBlockWords = 8 // one cumulative-popcount checkpoint per 512 bits
	mphSeed           = 0x9e3779b97f4a7c15
)

// mphLevel is one BBHash level: a bit array (m bits, m a multiple of 64), a per-block
// cumulative popcount index for O(1) rank, and the slot base (set bits in all prior levels).
type mphLevel struct {
	bits []uint64
	m    uint32
	rank []uint32 // rank[i] = popcount of bits[0 : i*mphRankBlockWords]
	base uint32
}

type mph struct {
	levels    []mphLevel
	nAssigned uint32
}

// mphLevelHash derives a level's hash from the key's base hash, decorrelating levels.
func mphLevelHash(h uint64, level int) uint64 {
	return mix64(h ^ (mphSeed * uint64(level+1)))
}

// buildMPH constructs an MPH over keys (which must be distinct). It returns the structure
// and slots, where slots[i] is key i's assigned slot or -1 if it fell through all levels
// unassigned (the caller resolves those via the sorted run). nAssigned == len(keys) unless
// some keys are unassigned.
func buildMPH(keys []string) (*mph, []int32) {
	n := len(keys)
	slots := make([]int32, n)
	for i := range slots {
		slots[i] = -1
	}

	m := &mph{}
	// hashes[i] is the base hash of the key still at index i in `remaining`.
	remaining := make([]int, n)
	for i := range remaining {
		remaining[i] = i
	}
	var slotBase uint32
	for level := 0; level < mphMaxLevels && len(remaining) > 0; level++ {
		mbits := uint32(len(remaining) * mphGamma)
		mbits = (mbits + 63) &^ 63 // round up to a whole word
		if mbits == 0 {
			mbits = 64
		}
		// Count collisions per bit (saturating at 2), then keep only singletons.
		count := make([]uint8, mbits)
		pos := make([]uint32, len(remaining))
		for i, ki := range remaining {
			p := uint32(mphLevelHash(hashString(keys[ki]), level) % uint64(mbits))
			pos[i] = p
			if count[p] < 2 {
				count[p]++
			}
		}
		levelBits := make([]uint64, mbits/64)
		next := remaining[:0:0] // fresh slice; do not alias remaining while reading it
		for i, ki := range remaining {
			if count[pos[i]] == 1 {
				levelBits[pos[i]>>6] |= 1 << (pos[i] & 63)
			} else {
				next = append(next, ki)
			}
		}
		rank, set := buildRankIndex(levelBits)
		// Assign slots for the singletons of this level, in bit order.
		for i, ki := range remaining {
			if count[pos[i]] == 1 {
				slots[ki] = int32(slotBase + rankAt(levelBits, rank, pos[i]))
			}
		}
		m.levels = append(m.levels, mphLevel{bits: levelBits, m: mbits, rank: rank, base: slotBase})
		slotBase += set
		remaining = next
	}
	m.nAssigned = slotBase
	return m, slots
}

// buildRankIndex returns cumulative popcount checkpoints (one per mphRankBlockWords words)
// and the total set-bit count.
func buildRankIndex(words []uint64) (rank []uint32, total uint32) {
	nblocks := (len(words) + mphRankBlockWords - 1) / mphRankBlockWords
	rank = make([]uint32, nblocks+1)
	var acc uint32
	for i, w := range words {
		if i%mphRankBlockWords == 0 {
			rank[i/mphRankBlockWords] = acc
		}
		acc += uint32(bits.OnesCount64(w))
	}
	rank[nblocks] = acc
	return rank, acc
}

// rankAt returns the number of set bits in words[0:p] (p is a bit index), using the
// checkpoint index for O(1) block skip plus a partial popcount.
func rankAt(words []uint64, rank []uint32, p uint32) uint32 {
	wordIdx := p >> 6
	block := wordIdx / mphRankBlockWords
	r := rank[block]
	for w := block * mphRankBlockWords; w < wordIdx; w++ {
		r += uint32(bits.OnesCount64(words[w]))
	}
	r += uint32(bits.OnesCount64(words[wordIdx] & ((1 << (p & 63)) - 1)))
	return r
}

// --- serialization + zero-copy probe (for the mmap sidecar) ---
//
// Layout, appended contiguously into the sidecar so it pages from the one mapping:
//
//	nAssigned u32; nLevels u32;
//	per level: m u32; base u32; nWords u32; nRank u32; bits [nWords]u64; rank [nRank]u32

// appendMPH serializes m (bloomM==0-style empty is represented by nLevels==0).
func appendMPH(b []byte, m *mph) []byte {
	b = appendU32(b, m.nAssigned)
	b = appendU32(b, uint32(len(m.levels)))
	for _, lv := range m.levels {
		b = appendU32(b, lv.m)
		b = appendU32(b, lv.base)
		b = appendU32(b, uint32(len(lv.bits)))
		b = appendU32(b, uint32(len(lv.rank)))
		for _, w := range lv.bits {
			b = appendU64(b, w)
		}
		for _, r := range lv.rank {
			b = appendU32(b, r)
		}
	}
	return b
}

// mphLookupBytes probes a serialized MPH at file offset off directly over the mapping,
// returning the key's slot in [0, nAssigned) if some level resolves it. A returned slot is
// only a candidate: the caller MUST verify the key stored at that slot (a non-member can
// resolve to another key's slot) and fall back to the sorted run on a miss. ok=false means
// no level resolved the key (an unassigned member or a non-member): fall back.
func mphLookupBytes(d []byte, off uint32, key string) (slot uint32, ok bool) {
	nLevels := le32(d, off+4)
	p := off + 8
	h := hashString(key)
	for lvl := 0; lvl < int(nLevels); lvl++ {
		m := le32(d, p)
		base := le32(d, p+4)
		nWords := le32(d, p+8)
		nRank := le32(d, p+12)
		bitsOff := p + 16
		rankOff := bitsOff + nWords*8
		bit := uint32(mphLevelHash(h, lvl) % uint64(m))
		wordIdx := bit >> 6
		if le64(d, bitsOff+wordIdx*8)&(1<<(bit&63)) != 0 {
			block := wordIdx / mphRankBlockWords
			r := le32(d, rankOff+block*4)
			for w := block * mphRankBlockWords; w < wordIdx; w++ {
				r += uint32(bits.OnesCount64(le64(d, bitsOff+w*8)))
			}
			r += uint32(bits.OnesCount64(le64(d, bitsOff+wordIdx*8) & ((1 << (bit & 63)) - 1)))
			return base + r, true
		}
		p = rankOff + nRank*4
	}
	return 0, false
}
