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
// ok=false when q is not native, or when a matching record's group value is one only evaluation can
// produce -- the same give-up condition as the predicate-analysis path, for the same reason (see
// GroupStatsQuery).
func (c *Collection) vecGroupStats(q *vm.Query, groupIDs []uint32, aggIDs []uint32, buckets []int64,
	st *schemaScanState) (*groupAccum, bool) {
	if q == nil || !q.Native() {
		return nil, false
	}
	acc := newGroupAccum(buckets)
	sides := c.newEvalSides(q)
	cs := &colScope{bc: st.cache, c: c}
	resolver := cs.resolve
	src := &blockVecSource{c: c, bc: st.cache}
	scratch := vecScratchPool.Get().(*vm.VecScratch)
	defer func() {
		scratch.Release()
		vecScratchPool.Put(scratch)
	}()
	groupLookups := make([]func(a wire.Ad) ([]byte, bool), 0, len(groupIDs))
	for _, id := range groupIDs {
		groupLookups = append(groupLookups, c.attrLookup(id))
	}
	aggLookups := make([]func(a wire.Ad) ([]byte, bool), 0, len(aggIDs))
	for _, id := range aggIDs {
		aggLookups = append(aggLookups, c.attrLookup(id))
	}

	var live []uint64
	var split vecSplit
	defer func() { lastGroupSplit.Store(&split) }()
	var cols groupCols
	aggCols := make([]numCol, len(aggIDs))
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			// No matching record, so no group: pruning is as sound here as for the count.
			if sides.excludes(c, w) {
				split.prunedSegs++
				continue
			}
			// The CurrentTime-folded form where the window is proven free of CurrentTime.
			sd := sides.at(w)
			q, m, fallbackM := sd.q, sd.m, sd.rm
			seg := w.seg.colblk.Load()
			groupIdxs, aggIdxs, ok := []int(nil), []int(nil), false
			if seg != nil && seg.schema() != nil {
				groupIdxs, aggIdxs, _, ok = resolveGroupFields(seg, groupIDs, aggIDs, nil)
			}
			if !ok {
				// No columnar block, or this segment's schema does not carry the group/aggregate
				// columns: evaluate and read this window's records the ordinary way.
				split.rowWindows++
				if !c.rowGroupWindow(w, s0, fallbackM, groupLookups, aggLookups, acc) {
					releaseWindows(wins)
					return nil, false
				}
				continue
			}
			pruneIdxs, prunePreds := sd.tests(c, seg.schema())
			strEq := sd.stringEq(c, seg.schema())
			base := 0
			for bi, blk := range seg.blocks {
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
				colOK := cols.setBlock(blk, groupIdxs, groupIDs, st.cache)
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
					if w.visible(o, s0) {
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
					cs.setGroups(seg, bi)
					if !c.groupBlockScoped(cs, resolver, m, fallbackM, w, seg, blk, base, s0,
						&cols, aggCols, st.cache, acc) {
						releaseWindows(wins)
						return nil, false
					}
					base += blk.n
					continue
				}
				src.blk = blk
				src.setGroups(seg, bi)
				vec, ok := q.VecEval(src, blk.n, scratch)
				if !ok {
					// This block only. Every other block still vectorizes.
					split.scopeBlocks++
					cs.setBlock(blk)
					cs.setGroups(seg, bi)
					if !c.groupBlockScoped(cs, resolver, m, fallbackM, w, seg, blk, base, s0,
						&cols, aggCols, st.cache, acc) {
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
					if !addGroupFromColumns(k, &cols, aggCols, st.cache, acc) {
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
// into acc. ok=false when the group value is one only evaluation can produce, which cannot be turned
// into a group here.
func addGroupFromColumns(k int, cols *groupCols, aggCols []numCol, bc *blockCache, acc *groupAccum) bool {
	g, ok := cols.accFor(k, bc, acc, len(aggCols))
	if !ok {
		return false
	}
	if g == nil {
		return true // in no bucket, which the scan drops too
	}
	for i := range aggCols {
		// An absent or non-numeric aggregate value contributes nothing, which is what the reference
		// computes for it -- unlike the group value, which decides whether the row exists at all. An
		// EXPRESSION has a value this cannot compute, so it gives the query back.
		av, cell := aggCols[i].cell(k, bc)
		if cell == cellExpr {
			return false
		}
		if cell == cellNumber {
			g.stats[i].add(av)
		}
	}
	return true
}

// groupBlockScoped is countBlockScoped accumulating groups: one block's visible matching records found
// with the per-record resolver, for a block the vector executor declined or one too heavily superseded to
// be worth vectorizing.
func (c *Collection) groupBlockScoped(cs *colScope, resolver func(name string, scope ast.AttributeScope) classad.Value,
	m *vm.Matcher, fallbackM *rowMatch, w segWindow, seg *colSegment, blk *columnarBlock, base int, s0 uint64,
	cols *groupCols, aggCols []numCol, bc *blockCache, acc *groupAccum) bool {
	for k := 0; k < blk.n; k++ {
		gk := base + k
		if gk >= seg.offsLen() {
			break
		}
		o := seg.offAt(gk)
		if !w.visible(o, s0) {
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
		if !addGroupFromColumns(k, cols, aggCols, bc, acc) {
			return false
		}
	}
	return true
}

// rowGroupWindow is rowEvalWindow accumulating groups: for a window with no columnar block (the active
// segment, or one the accelerator has not reached) or one whose schema lacks a column involved, evaluate
// each visible record the ordinary way and read the group and aggregate values from the record itself.
func (c *Collection) rowGroupWindow(w segWindow, s0 uint64, m *rowMatch,
	groupLookups []func(a wire.Ad) ([]byte, bool), aggLookups []func(a wire.Ad) ([]byte, bool),
	acc *groupAccum) bool {
	var comps [maxGroupCols]groupComp
	for off := 0; off < w.used; {
		o := uint32(off)
		if end, ok := w.dmg.skip(o); ok {
			off = int(end)
			continue
		}
		total := recTotalLen(w.data, o)
		if total == 0 {
			break
		}
		if recIsMarker(w.data, o) || !w.visible(o, s0) {
			off += int(total)
			continue
		}
		if ww, matched := m.match(w, o); matched {
			// Matched, so this record forms a group: read its values from the wire form, the same
			// lookups the predicate-analysis path's row fallback uses, so group identity does not
			// depend on which tier answered.
			if !addGroupFromAd(wire.Ad(ww), groupLookups, aggLookups, acc, &comps) {
				return false
			}
		}
		off += int(total)
	}
	return true
}

// addGroupFromAd is addGroupFromColumns reading from a wire ad instead of a block's columns.
//
// comps is the caller's scratch, held across the window rather than declared here. Declaring it was
// a maxGroupCols-wide array zeroed once per RECORD on this path, which made the cap a per-record
// cost at every arity -- a query grouping by one column paid for the widest key any query could
// ask, and raising the cap raised that. Every slot the accumulator reads is comps[:len(groupLookups)]
// and the loop below writes all of them, so reusing one array cannot carry a component over from
// the previous record.
func addGroupFromAd(ad wire.Ad, groupLookups []func(a wire.Ad) ([]byte, bool),
	aggLookups []func(a wire.Ad) ([]byte, bool), acc *groupAccum,
	comps *[maxGroupCols]groupComp) bool {
	for i := range groupLookups {
		node, found := groupLookups[i](ad)
		comp, ok := adGroupComp(node, found, acc)
		if !ok {
			return false
		}
		comps[i] = comp
	}
	g, _ := acc.accFor(comps, len(aggLookups))
	if g == nil {
		return true // in no bucket, which the scan drops too
	}
	for i := range aggLookups {
		if an, found := aggLookups[i](ad); found {
			if isExprNode(an) {
				return false
			}
			if av, isNum := nodeColVal(an); isNum {
				g.stats[i].add(av)
			}
		}
	}
	return true
}
