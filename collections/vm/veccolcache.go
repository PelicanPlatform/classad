package vm

import "github.com/PelicanPlatform/classad/ast"

// LOADING A COLUMN ONCE PER BATCH INSTEAD OF ONCE PER MENTION.
//
// `A > 4 && A < 10` names the same column twice and, until this, decompressed it twice. So did
// `A + A`, and so did every conjunction that constrains one attribute from both sides -- which is
// what a range predicate is. A load is a DECOMPRESSION: roughly 78% of this store's read CPU is
// zstd, and nothing else the executor does is in that league.
//
// A COPY IS THE PRICE, and whether it is worth paying depends on what the load COSTS -- which is not
// the same for every column, and is the thing a first guess here got wrong.
//
// "A load is a decompression" is false on this path. The block is decompressed once by the block
// cache, and a load extracts one column from the decompressed block. For a fitted-width numeric
// column with no escapes that extraction is a SLICE of bytes the block already holds, so loading it
// twice costs a bounds check and loading it once costs a copy -- the cache would be a pure loss. For
// a string column it is a dictionary parse or a positional walk over every preceding field, which is
// where the time actually is: measured at -38% on `Owner == "user3" || Owner == "user10"`.
//
// So the first load decides. A column that comes back in RAW form is cheap to re-read and is not
// cached; anything else is. The decision is per batch rather than per program, because whether a
// column is raw is a property of the block -- the same attribute is raw in a block with no escapes
// and materialized in one with them.
//
// ONLY COLUMNS MENTIONED MORE THAN ONCE are cached, decided at compile time. A column named once --
// the common case by far -- loads straight onto the stack exactly as before, with no cache lookup
// and no copy. Caching those too would add a copy to the path that has nothing to gain.
//
// The sharing is keyed by NAME AND SCOPE, not by reference index: the compiler appends a fresh
// refInfo per mention, so `A` twice is two indices naming one column.

// colSlots assigns each ref index a cache slot, or -1 when the column is mentioned once and should
// not be cached. It returns the per-ref slots and how many slots there are.
//
// CurrentTime never gets a slot. The vector executor declines it outright -- a column reads an
// absent CurrentTime as undefined where the evaluator reads the clock -- and giving it a slot would
// only put a value in a cache nothing may read.
func colSlots(refs []refInfo) ([]int32, int) {
	type key struct {
		name  string
		scope ast.AttributeScope
	}
	count := map[key]int{}
	for _, r := range refs {
		if r.curTime {
			continue
		}
		count[key{r.node.NormalizedName(), r.scope}]++
	}
	slotOf := map[key]int32{}
	n := int32(0)
	out := make([]int32, len(refs))
	for i, r := range refs {
		out[i] = -1
		if r.curTime {
			continue
		}
		k := key{r.node.NormalizedName(), r.scope}
		if count[k] < 2 {
			continue
		}
		s, ok := slotOf[k]
		if !ok {
			s, slotOf[k] = n, n
			n++
		}
		out[i] = s
	}
	return out, int(n)
}

// loadShared returns the column for slot, loading it once per batch.
//
// The cached vector is never handed out: callers get a copy, because a kernel would write over it
// and the next mention would read the result of the first one's arithmetic.
func (s *VecScratch) loadShared(src ColumnSource, r refInfo, slot int32, nSlots int) (*Vec, bool) {
	for len(s.colCache) < nSlots {
		s.colCache = append(s.colCache, nil)
	}
	if s.colValid[slot] {
		return s.colCache[slot], true
	}
	for len(s.colCheap) < nSlots {
		s.colCheap = append(s.colCheap, false)
	}
	v := s.colCache[slot]
	if v == nil || v.n != s.n || cap(v.I) < s.n {
		v = newVec(s.n)
		s.colCache[slot] = v
	}
	v.n = s.n
	v.I, v.S, v.St = v.I[:s.n], v.S[:s.n], v.St[:s.n]
	v.Hi, v.Lo = v.Hi[:maskWords(s.n)], v.Lo[:maskWords(s.n)]
	v.Mask, v.Const, v.Raw, v.Dict = false, false, nil, nil
	if !src.LoadColumn(r.name, r.scope, v) {
		return nil, false
	}
	if v.Raw != nil {
		// A slice of the block's own bytes. Re-reading it is a bounds check, so there is nothing for
		// a cache to save and the copy would be the only cost. Later mentions load it directly.
		s.colCheap[slot] = true
	} else {
		s.colValid[slot] = true
	}
	return v, true
}

// colIsCheap reports whether this batch found the column cheap enough to re-read.
func (s *VecScratch) colIsCheap(slot int32) bool {
	return int(slot) < len(s.colCheap) && s.colCheap[slot]
}

// resetColCache marks every slot unloaded. The vectors survive so a scan reuses them across blocks;
// the flags do not, because a column belongs to one block.
func (s *VecScratch) resetColCache(nSlots int) {
	for len(s.colValid) < nSlots {
		s.colValid = append(s.colValid, false)
	}
	for i := range s.colValid {
		s.colValid[i] = false
	}
	for i := range s.colCheap {
		s.colCheap[i] = false
	}
}
