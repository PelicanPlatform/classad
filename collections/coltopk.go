package collections

import (
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
		preds, pok = c.numPredsOnFields(q, st.schema)
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
	lookups := make([]func(wire.Ad) ([]byte, bool), len(otherPreds))
	for i, p := range otherPreds {
		lookups[i] = c.attrLookup(p.fieldID)
	}
	aggLookup := c.attrLookup(aggID)

	tk := &topKKeep{k: k, desc: desc}
	var keep []bool
	var scratch []int64
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		stats.segments(len(wins), 0) // the cutoff scan filters on non-order fields, so no zone prune
		for _, w := range wins {
			cs := w.seg.colblk.Load()
			aggIdx, allIdxs, rok := resolveStatsFields(cs, aggID, preds)
			if !rok {
				c.bruteTopK(w, s0, aggLookup, otherPreds, lookups, selfOK, tk, stats)
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
				col, colOK := newNumCol(blk, aggIdx, aggID, st.cache)
				if !colOK {
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
						if !(recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0) {
							continue
						}
						stats.visit()
						stats.columnDecided() // the filter is answered from the columns
						if nv, vok := col.at(j, st.cache); vok && selfOK(nv.f) {
							tk.offer(nv.f)
							stats.matched()
						}
					}
					base += blk.n
					continue
				}
				if cap(keep) < blk.n {
					keep = make([]bool, blk.n)
					scratch = make([]int64, blk.n)
				}
				keep, scratch = keep[:blk.n], scratch[:blk.n]
				live := 0
				for j := 0; j < blk.n; j++ {
					gk := base + j
					vis := gk < cs.offsLen()
					if vis {
						o := cs.offAt(gk)
						vis = recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0
					}
					keep[j] = vis
					if vis {
						live++
					}
				}
				for i := range otherPreds {
					if live == 0 {
						break
					}
					live = narrowByField(blk, otherIdxs[i], otherPreds[i], st.cache, keep, scratch)
				}
				if live > 0 {
					for j := 0; j < blk.n; j++ {
						if !keep[j] {
							continue
						}
						stats.visit()
						stats.columnDecided()
						if nv, vok := col.at(j, st.cache); vok && selfOK(nv.f) {
							tk.offer(nv.f)
							stats.matched()
						}
					}
				}
				base += blk.n
			}
		}
		releaseWindows(wins)
	}
	th, _ := tk.threshold()
	return th, tk.seen, true
}

// bruteTopK is the row fallback for a window whose schema lacks the order/predicate fields, and for
// the active segment: walk its visible records, test the predicates, and offer the order value --
// reading only those attributes from the wire, never a full decode. Mirrors bruteStatsMulti.
func (c *Collection) bruteTopK(w segWindow, s0 uint64, aggLookup func(wire.Ad) ([]byte, bool),
	preds []fieldPred, lookups []func(wire.Ad) ([]byte, bool), selfOK func(float64) bool, tk *topKKeep, stats *ScanStats) {
	var buf []byte
	for off := 0; off < w.used; {
		o := uint32(off)
		total := recTotalLen(w.data, o)
		if total == 0 {
			break
		}
		if !recIsMarker(w.data, o) && recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0 {
			if ww, err := c.wire(recRef{w: w, off: o, dict: w.dict()}, buf); err == nil {
				buf = ww
				stats.visit()
				stats.reassembled() // no columnar block here: the record was rebuilt to test it
				ad := wire.Ad(ww)
				match := true
				for i := range preds {
					node, found := lookups[i](ad)
					if !found {
						match = false
						break
					}
					nv, isNum := nodeColVal(node)
					if !isNum || !preds[i].eval(nv.f) {
						match = false
						break
					}
				}
				if match {
					if node, found := aggLookup(ad); found {
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
}
