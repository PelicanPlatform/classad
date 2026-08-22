package collections

import (
	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// GROUP BY under a predicate the hand-written column scan cannot lower.
//
// GroupStatsQuery's predicate analysis serves a conjunction of numeric comparisons against literals --
// the shape a history dashboard asks most, and the one that was reported slow. Anything else declined and
// went back to a record scan, even though the UNGROUPED count had already moved past that limit: since
// the vector executor landed, `count(*) where Owner == "x"`, `... where A > B`, and `... where A || B`
// are all columnar. Grouping was not, so the same predicate cost 38 ms ungrouped and 8 s grouped for a
// reason that had stopped being true.
//
// This is VectorEvalCount's loop with the count replaced by group accumulation. Every tier it falls
// through -- a window with no columnar block, a mostly-superseded block, a block the executor declines --
// gets a grouped analogue here, because a tier that silently contributed no groups would return a wrong
// answer rather than a slow one.
//
// One asymmetry with the count path is deliberate: reading the GROUP and aggregate columns still goes
// through the block's columns even when the PREDICATE fell back to per-record evaluation. The fallback is
// about evaluating the expression, not about which record k is, so the group value is read the same way
// in every tier and group identity cannot depend on which tier answered.

// vecGroupStats accumulates per-group stats for every visible record matching q, evaluating q against the
// columns (a column at a time where the expression allows it, per record where it does not).
//
// ok=false when q is not native, or when a matching record's group value is not a scalar number -- the
// same give-up condition as the predicate-analysis path, for the same reason (see GroupStatsQuery).
func (c *Collection) vecGroupStats(q *vm.Query, groupID uint32, aggIDs []uint32,
	st *schemaScanState) (map[groupKey]*groupAcc, bool) {
	if q == nil || !q.Native() {
		return nil, false
	}
	acc := map[groupKey]*groupAcc{}
	m := q.Matcher()
	fallbackM := q.Matcher()
	cs := &colScope{bc: st.cache, c: c}
	resolver := cs.resolve
	src := &blockVecSource{c: c, bc: st.cache}
	scratch := vecScratchPool.Get().(*vm.VecScratch)
	defer func() {
		scratch.Release()
		vecScratchPool.Put(scratch)
	}()
	groupLookup := c.attrLookup(groupID)
	aggLookups := make([]func(a wire.Ad) ([]byte, bool), 0, len(aggIDs))
	for _, id := range aggIDs {
		aggLookups = append(aggLookups, c.attrLookup(id))
	}

	var live []uint64
	var plan prunePlan
	var split vecSplit
	defer func() { lastGroupSplit.Store(&split) }()
	aggCols := make([]numCol, len(aggIDs))
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			seg := w.seg.colblk.Load()
			groupIdx, aggIdxs, ok := 0, []int(nil), false
			if seg != nil && seg.schema() != nil {
				groupIdx, aggIdxs, _, ok = resolveGroupFields(seg, groupID, aggIDs, nil)
			}
			if !ok {
				// No columnar block, or this segment's schema does not carry the group/aggregate
				// columns: evaluate and read this window's records the ordinary way.
				split.rowWindows++
				if !c.rowGroupWindow(w, s0, fallbackM, groupLookup, aggLookups, acc) {
					releaseWindows(wins)
					return nil, false
				}
				continue
			}
			pruneIdxs, prunePreds := plan.tests(c, q, seg.schema())
			strEq := plan.stringEq(c, q, seg.schema())
			base := 0
			for _, blk := range seg.blocks {
				// Pruning is sound for grouping: a block no matching record falls in contributes no
				// groups, so skipping it drops nothing.
				if len(prunePreds) > 0 && !blockMayMatch(blk, pruneIdxs, prunePreds) {
					split.prunedBlocks++
					base += blk.n
					continue
				}
				if len(strEq) > 0 && blk.dictPrunes(strEq, st.cache, &src.dictBuf) {
					split.prunedBlocks++
					base += blk.n
					continue
				}
				col, colOK := newNumCol(blk, groupIdx, groupID, st.cache)
				if !colOK {
					releaseWindows(wins)
					return nil, false
				}
				for i := range aggIDs {
					if aggCols[i], colOK = newNumCol(blk, aggIdxs[i], aggIDs[i], st.cache); !colOK {
						releaseWindows(wins)
						return nil, false
					}
				}
				words := vm.MaskWords(blk.n)
				if cap(live) < words {
					live = make([]uint64, words)
				}
				live = live[:words]
				for i := range live {
					live[i] = 0
				}
				nLive := 0
				for k := 0; k < blk.n; k++ {
					gk := base + k
					if gk >= seg.offsLen() {
						break
					}
					o := seg.offAt(gk)
					if recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0 {
						live[k/64] |= 1 << uint(k%64)
						nLive++
					}
				}
				if nLive == 0 {
					split.emptyBlocks++
					base += blk.n
					continue
				}
				if nLive*minLiveDen < blk.n*minLiveNum {
					split.churnBlocks++
					cs.setBlock(blk)
					if !c.groupBlockScoped(cs, resolver, m, fallbackM, w, seg, blk, base, s0,
						col, aggCols, st.cache, acc) {
						releaseWindows(wins)
						return nil, false
					}
					base += blk.n
					continue
				}
				src.blk = blk
				vec, ok := q.VecEval(src, blk.n, scratch)
				if !ok {
					// This block only. Every other block still vectorizes.
					split.scopeBlocks++
					cs.setBlock(blk)
					if !c.groupBlockScoped(cs, resolver, m, fallbackM, w, seg, blk, base, s0,
						col, aggCols, st.cache, acc) {
						releaseWindows(wins)
						return nil, false
					}
					base += blk.n
					continue
				}
				split.vecBlocks++
				for k := 0; k < blk.n; k++ {
					if live[k/64]&(1<<uint(k%64)) == 0 || !vec.IsTrue(k) {
						continue
					}
					if !addGroupFromColumns(k, col, aggCols, st.cache, acc) {
						releaseWindows(wins)
						return nil, false
					}
				}
				base += blk.n
			}
		}
		releaseWindows(wins)
	}
	return acc, true
}

// addGroupFromColumns reads record k's group and aggregate values from the block's columns and folds them
// into acc. ok=false when the group value is not a scalar number, which cannot be turned into a group.
func addGroupFromColumns(k int, col numCol, aggCols []numCol, bc *blockCache, acc map[groupKey]*groupAcc) bool {
	nv, ok := col.at(k, bc)
	if !ok || !numericKind(nv.kind) {
		return false
	}
	key := groupKeyOf(nv)
	g := acc[key]
	if g == nil {
		g = newGroupAcc(len(aggCols))
		acc[key] = g
	}
	g.n++
	for i := range aggCols {
		// An absent or non-numeric aggregate value contributes nothing, which is what the reference
		// computes for it -- unlike the group value, which decides whether the row exists at all.
		if av, ok := aggCols[i].at(k, bc); ok {
			g.stats[i].add(av)
		}
	}
	return true
}

// groupBlockScoped is countBlockScoped accumulating groups: one block's visible matching records found
// with the per-record resolver, for a block the vector executor declined or one too heavily superseded to
// be worth vectorizing.
func (c *Collection) groupBlockScoped(cs *colScope, resolver func(name string, scope ast.AttributeScope) classad.Value,
	m, fallbackM *vm.Matcher, w segWindow, seg *colSegment, blk *columnarBlock, base int, s0 uint64,
	col numCol, aggCols []numCol, bc *blockCache, acc map[groupKey]*groupAcc) bool {
	for k := 0; k < blk.n; k++ {
		gk := base + k
		if gk >= seg.offsLen() {
			break
		}
		o := seg.offAt(gk)
		if !(recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0) {
			continue
		}
		cs.k, cs.fellBack = k, false
		v := m.EvalResolved(resolver)
		matched := false
		if cs.fellBack {
			// One record needs the ordinary evaluator; the rest of the block is unaffected.
			matched = c.evalOneRecord(w, o, fallbackM)
		} else {
			matched = isTrueValue(v)
		}
		if !matched {
			continue
		}
		if !addGroupFromColumns(k, col, aggCols, bc, acc) {
			return false
		}
	}
	return true
}

// rowGroupWindow is rowEvalWindow accumulating groups: for a window with no columnar block (the active
// segment, or one the accelerator has not reached) or one whose schema lacks a column involved, evaluate
// each visible record the ordinary way and read the group and aggregate values from the record itself.
func (c *Collection) rowGroupWindow(w segWindow, s0 uint64, m *vm.Matcher,
	groupLookup func(a wire.Ad) ([]byte, bool), aggLookups []func(a wire.Ad) ([]byte, bool),
	acc map[groupKey]*groupAcc) bool {
	for off := 0; off < w.used; {
		o := uint32(off)
		total := recTotalLen(w.data, o)
		if total == 0 {
			break
		}
		if !recIsMarker(w.data, o) && recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0 &&
			c.evalOneRecord(w, o, m) {
			// Matched, so this record forms a group: read its values from the wire form, the same
			// lookups the predicate-analysis path's row fallback uses, so group identity does not
			// depend on which tier answered.
			ww, err := c.wire(recRef{w: w, off: o, dict: w.dict()}, nil)
			if err != nil {
				return false
			}
			if !addGroupFromAd(wire.Ad(ww), groupLookup, aggLookups, acc) {
				return false
			}
		}
		off += int(total)
	}
	return true
}

// addGroupFromAd is addGroupFromColumns reading from a wire ad instead of a block's columns.
func addGroupFromAd(ad wire.Ad, groupLookup func(a wire.Ad) ([]byte, bool),
	aggLookups []func(a wire.Ad) ([]byte, bool), acc map[groupKey]*groupAcc) bool {
	node, found := groupLookup(ad)
	if !found {
		return false
	}
	nv, isNum := nodeColVal(node)
	if !isNum || !numericKind(nv.kind) {
		return false
	}
	key := groupKeyOf(nv)
	g := acc[key]
	if g == nil {
		g = newGroupAcc(len(aggLookups))
		acc[key] = g
	}
	g.n++
	for i := range aggLookups {
		if an, found := aggLookups[i](ad); found {
			if av, isNum := nodeColVal(an); isNum {
				g.stats[i].add(av)
			}
		}
	}
	return true
}
