package collections

import (
	"math/bits"

	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Columnar TOP-K threshold.
//
// ORDER BY <numeric attr> {DESC|ASC} LIMIT k was served by projecting EVERY matching row into
// classad.Values and heaping them -- a million allocations to keep one, while MAX(attr) over the
// same rows reduces raw column values with none. This is the first half of closing that gap: the
// same columnar scan the numeric aggregate uses (block pruning, MVCC visibility, cold-tail escape,
// the active-segment row fallback), but instead of MIN/MAX it keeps the best k VALUES and returns
// the k-th (the threshold). The caller then re-runs the projection with `attr >= threshold`
// (desc) / `attr <= threshold` (asc) appended, which the columns and zone maps narrow to ~k rows,
// and projects only those -- so the million-Value pass never happens.
//
// It reads only floats; it never touches a record, exactly as the aggregate does not.

// topKKeep holds the current best k order values: the largest k when desc, the smallest k when asc.
// worst() is the value a better one evicts, so once full its value is the k-th best -- the cutoff.
type topKKeep struct {
	vals []float64
	k    int
	desc bool
	seen int // numeric order values offered (== matching rows with a numeric order attr)
}

func (t *topKKeep) offer(v float64) {
	t.seen++
	if len(t.vals) < t.k {
		t.vals = append(t.vals, v)
		return
	}
	wi := t.worstIdx()
	if (t.desc && v > t.vals[wi]) || (!t.desc && v < t.vals[wi]) {
		t.vals[wi] = v
	}
}

// worstIdx is the index of the kept value a better one would replace: the smallest when keeping the
// largest k (desc), the largest when keeping the smallest k (asc). O(k) per offer, which is why this
// is for small k; k is a LIMIT.
func (t *topKKeep) worstIdx() int {
	wi := 0
	for i := 1; i < len(t.vals); i++ {
		if (t.desc && t.vals[i] < t.vals[wi]) || (!t.desc && t.vals[i] > t.vals[wi]) {
			wi = i
		}
	}
	return wi
}

// threshold returns the cutoff and whether the keep is full. When full a row belongs to the top k
// (plus ties) iff its order value is >= cutoff (desc) or <= cutoff (asc).
func (t *topKKeep) threshold() (float64, bool) {
	if len(t.vals) < t.k {
		return 0, false
	}
	return t.vals[t.worstIdx()], true
}

// TopKOrderThreshold computes the cutoff order value for ORDER BY orderAttr {DESC|ASC} LIMIT k over
// the records matching q: the k-th largest (desc) or k-th smallest (asc). It returns
// (threshold, seen, ok): seen is the number of matching records with a numeric order value, and ok
// reports whether the columnar path could serve the request at all (same gate as NumStatsQuery:
// accelerator on, orderAttr a numeric schema field, q a conjunction of scalar numeric comparisons).
//
// The threshold is meaningful only when seen > k (the keep filled); at seen <= k there are k or
// fewer rows and the caller should just fetch them directly.
//
// stats (may be nil) records the cutoff scan's work for EXPLAIN ANALYZE: which records were read
// from columns vs reassembled by the active-segment fallback, and how many matched -- the same
// ScanStats a projected scan reports, so the top-K path is no longer invisible to the diagnostic.
func (c *Collection) TopKOrderThreshold(q *vm.Query, orderAttr string, desc bool, k int, stats *ScanStats) (threshold float64, seen int, ok bool) {
	if k <= 0 {
		return 0, 0, false
	}
	st := c.schemaScan.Load()
	if st == nil || c.intern == nil {
		return 0, 0, false
	}
	id, has := c.intern.LookupID(orderAttr)
	if !has {
		return 0, 0, false
	}
	idx, has := st.schema.byID[id]
	if !has || !numericKind(st.schema.fields[idx].kind) {
		return 0, 0, false
	}
	var preds []fieldPred
	if q != nil {
		var pok bool
		preds, pok = c.numPredsCT(q, st.schema)
		if !pok {
			return 0, 0, false // not a conjunction of scalar numeric comparisons
		}
	}

	// A predicate on the ORDER attribute itself is applied in the value pass (the value is read
	// anyway); predicates on other fields narrow first -- mirrors schemaScanStatsMulti.
	aggID := id
	var selfPreds, otherPreds []fieldPred
	for _, p := range preds {
		if p.fieldID == aggID {
			selfPreds = append(selfPreds, p)
		} else {
			otherPreds = append(otherPreds, p)
		}
	}
	selfOK := func(v float64) bool {
		for _, p := range selfPreds {
			if !p.eval(v) {
				return false
			}
		}
		return true
	}
	lookups := make([]fieldLookup, len(otherPreds))
	for i, p := range otherPreds {
		lookups[i] = c.attrLookup(p.fieldID)
	}
	aggLookup := c.attrLookup(aggID)

	tk := &topKKeep{k: k, desc: desc}
	var cand candSet
	var sc predScratch
	ct := predsNeedCT(preds)
	zp := predZoneProbes(preds)
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		pruned := 0
		for _, w := range wins {
			if ct && !w.ctFree {
				releaseWindows(wins) // a folded predicate is exact only in a proven window
				return 0, 0, false
			}
			// No record here can match, so none offers an order value; see schemaScanCountMulti.
			if c.segZoneExcludes(w, zp, ct) {
				pruned++
				continue
			}
			cs := w.seg.colblk.Load()
			aggIdx, allIdxs, rok := resolveStatsFields(cs, aggID, preds)
			if !rok {
				if !c.bruteTopK(w, s0, aggLookup, otherPreds, lookups, selfOK, tk, stats) {
					releaseWindows(wins)
					return 0, 0, false
				}
				continue
			}
			otherIdxs := make([]int, 0, len(otherPreds))
			for i, p := range preds {
				if p.fieldID != aggID {
					otherIdxs = append(otherIdxs, allIdxs[i])
				}
			}
			base := 0
			for _, blk := range cs.blocks {
				if !blockMayMatch(blk, allIdxs, preds) {
					base += blk.n
					continue
				}
				// A column that will not read (an unreadable cold stream): this block's records are
				// read from the arena instead of left out of the cutoff, and counted as reassembled.
				col, colOK := newNumCol(blk, aggIdx, aggID, st.cache)
				if !colOK || !blk.numColsReadable(otherIdxs, st.cache) {
					if !c.topKBlockRows(w, s0, cs, base, blk.n, aggLookup, otherPreds, lookups, selfOK, tk, stats) {
						releaseWindows(wins)
						return 0, 0, false
					}
					base += blk.n
					continue
				}
				if len(otherPreds) == 0 {
					for j := 0; j < blk.n; j++ {
						gk := base + j
						if gk >= cs.offsLen() {
							break
						}
						o := cs.offAt(gk)
						if !w.visible(o, s0) {
							continue
						}
						stats.visit()
						stats.columnDecided() // the filter is answered from the columns
						nv, cell := col.cell(j, st.cache)
						if cell == cellExpr {
							releaseWindows(wins)
							return 0, 0, false
						}
						if cell == cellNumber && selfOK(nv.f) {
							tk.offer(nv.f)
							stats.matched()
						}
					}
					base += blk.n
					continue
				}
				sc.prepare(blk.n)
				cand.setVisible(cs, w, s0, base, blk.n)
				for i := range otherPreds {
					if cand.live == 0 {
						break
					}
					if !narrowByField(blk, otherIdxs[i], otherPreds[i], st.cache, &cand, &sc) {
						releaseWindows(wins)
						return 0, 0, false
					}
				}
				for wi, word := range cand.w {
					for word != 0 {
						j := wi*64 + bits.TrailingZeros64(word)
						word &= word - 1
						stats.visit()
						stats.columnDecided()
						nv, cell := col.cell(j, st.cache)
						if cell == cellExpr {
							releaseWindows(wins)
							return 0, 0, false
						}
						if cell == cellNumber && selfOK(nv.f) {
							tk.offer(nv.f)
							stats.matched()
						}
					}
				}
				base += blk.n
			}
		}
		stats.segments(len(wins), pruned)
		releaseWindows(wins)
	}
	th, _ := tk.threshold()
	return th, tk.seen, true
}

// bruteTopK is the row fallback for a window whose schema lacks the order/predicate fields, and for
// the active segment: walk its visible records, test the predicates, and offer the order value --
// reading only those attributes from the wire, never a full decode. Mirrors bruteStatsMulti.
func (c *Collection) bruteTopK(w segWindow, s0 uint64, aggLookup fieldLookup,
	preds []fieldPred, lookups []fieldLookup, selfOK func(float64) bool, tk *topKKeep, stats *ScanStats) bool {
	var buf []byte
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
		if !recIsMarker(w.data, o) && w.visible(o, s0) {
			if ww, err := c.wire(recRef{w: w, off: o, dict: w.dict()}, buf, sealedUnopened); err == nil {
				buf = ww
				stats.visit()
				stats.reassembled() // no columnar block here: the record was rebuilt to test it
				ad := wire.Ad(ww)
				match := true
				for i := range preds {
					node, found := lookups[i](w.dict(), ad)
					if !found {
						match = false
						break
					}
					if isExprNode(node) {
						return false
					}
					nv, isNum := nodeColVal(node)
					if !isNum || !preds[i].eval(nv.f) {
						match = false
						break
					}
				}
				if match {
					if node, found := aggLookup(w.dict(), ad); found {
						if isExprNode(node) {
							return false
						}
						if nv, isNum := nodeColVal(node); isNum && selfOK(nv.f) {
							tk.offer(nv.f)
							stats.matched()
						}
					}
				}
			}
		}
		off += int(total)
	}
	return true
}

// topKBlockRows is the row fallback for one columnar block whose columns will not read: its visible
// records, read from the arena (see eachBlockRecord). Kept out of the column loop so that loop's
// code is the same whether or not a block ever falls back.
func (c *Collection) topKBlockRows(w segWindow, s0 uint64, cs *colSegment, base, n int,
	aggLookup fieldLookup, preds []fieldPred, lookups []fieldLookup,
	selfOK func(float64) bool, tk *topKKeep, stats *ScanStats) bool {
	return c.eachBlockRecord(w, s0, cs, base, n, func(ad wire.Ad) bool {
		return topKFromAd(w.dict(), ad, aggLookup, preds, lookups, selfOK, tk, stats)
	})
}

// topKFromAd is the row fallback's step for one record: test the predicates and offer the order value,
// counting the record as visited and reassembled (it was rebuilt to test it). ok=false when a value
// involved is an expression. bruteTopK takes the same step inline, for the reason statsFromAd gives.
func topKFromAd(d *segDictHandle, ad wire.Ad, aggLookup fieldLookup, preds []fieldPred,
	lookups []fieldLookup, selfOK func(float64) bool, tk *topKKeep, stats *ScanStats) bool {
	stats.visit()
	stats.reassembled()
	match, ok := wirePredsMatch(d, ad, preds, lookups)
	if !ok {
		return false
	}
	if !match {
		return true
	}
	node, found := aggLookup(d, ad)
	if !found {
		return true
	}
	if isExprNode(node) {
		return false
	}
	if nv, isNum := nodeColVal(node); isNum && selfOK(nv.f) {
		tk.offer(nv.f)
		stats.matched()
	}
	return true
}
