package collections

import (
	"math"
	"math/bits"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// MIN/MAX/SUM/AVG/COUNT(attr) with a predicate on OTHER columns.
//
// NumStatsQuery served a predicate only when it constrained the SAME attribute being aggregated
// (numPredOnField, plus a predID == id check), so `max(ProcId)` and `max(ProcId) where ProcId >= 5`
// were columnar while `max(ProcId) where JobStatus == 4` was not -- and that is the shape a dashboard
// asks. COUNT(*) already handles N conjuncts across N columns; this brings the value aggregates to
// the same footing.
//
// The mechanics are the count scan's, with one extra column read: narrow the candidate records with
// one pass per predicate column (skipping blocks their zones rule out), then read the AGGREGATED
// column for the survivors. The aggregated column is deliberately not zone-pruned -- its values are
// the answer, not a filter.

// statsAccum accumulates one aggregate pass, carrying the reference's type-promotion rules: SUM stays
// integral until a real appears, and a boolean is coerced but flagged so a caller can decline rather
// than reproduce the reference's boolean quirks.
type statsAccum struct {
	out    NumStats
	anyInt bool // some integer contributed, so out.IntMin/IntMax hold values
}

func newStatsAccum() statsAccum {
	return statsAccum{out: NumStats{Min: math.Inf(1), Max: math.Inf(-1)}}
}

func (a *statsAccum) add(nv colVal) {
	a.out.N++
	a.out.Sum += nv.f
	switch nv.kind {
	case akInt:
		a.out.IntSum += nv.i
		a.addInt(nv.i)
	case akReal:
		a.out.AnyReal = true
		// NaN loses every comparison, so it slips past both updates below without ever
		// becoming an extreme -- an all-NaN group would report the seeds, +Inf for MIN and
		// -Inf for MAX, as if they were data. Flag it so a MIN/MAX caller can decline (see
		// NumStats.AnyNaN); the extremes below stay exactly as they were for every other
		// column, NaN or not. A real is the ONLY kind that can be NaN: akInt's f is a
		// converted int64 and akBool's is 0 or 1, so an integer column pays nothing for this
		// and a real column pays one compare that is false on every ordinary value.
		if nv.f != nv.f {
			a.out.AnyNaN = true
		}
	case akBool:
		a.out.IntSum += nv.i
		a.out.AnyBool = true
		a.addInt(nv.i)
	}
	if nv.f < a.out.Min {
		a.out.Min = nv.f
	}
	if nv.f > a.out.Max {
		a.out.Max = nv.f
	}
}

func (a *statsAccum) addInt(i int64) {
	if !a.anyInt || i < a.out.IntMin {
		a.out.IntMin = i
	}
	if !a.anyInt || i > a.out.IntMax {
		a.out.IntMax = i
	}
	a.anyInt = true
}

func (a *statsAccum) result() NumStats {
	if a.out.N == 0 {
		a.out.Min, a.out.Max = 0, 0
	}
	return a.out
}

// numCol is a per-block, per-field column reader with everything resolved OUT of the per-record loop:
// the hot/cold split, the byte offset, the width, the signedness and the kind.
//
// Resolving them per record instead means a map lookup (hotFieldOff) for every value -- scanInt hoists
// exactly this out of its loop, and not doing so left an aggregate 1.4x slower than the single-pass
// path it replaced, even after the predicate fusion.
type numCol struct {
	blk      *columnarBlock
	fieldIdx int
	fieldID  uint32
	escBytes int
	region   []byte // the buffer this column lives in: uncompressed hot columns, or the decompressed
	//        cold-numeric region. Both columnar, so one reader serves both.
	start    int // this field's offset within region
	width    int
	unsigned bool
	isReal   bool
}

// newNumCol resolves a column for repeated reads. ok=false when the field is not numeric here, or its
// cold region cannot be decompressed.
func newNumCol(blk *columnarBlock, fieldIdx int, fieldID uint32, bc *blockCache) (numCol, bool) {
	if !blk.hasFieldIdx(fieldIdx) {
		return numCol{}, false
	}
	f := blk.schema.fields[fieldIdx]
	c := numCol{
		blk: blk, fieldIdx: fieldIdx, fieldID: fieldID, escBytes: blk.schema.escBytes,
		width: f.width, unsigned: f.unsigned, isReal: f.kind == akReal,
	}
	// Hot and cold numerics are both columnar, so resolving a column is picking a buffer and an offset;
	// the per-record read below no longer branches on which it was.
	if start, hot := blk.hotColStart(fieldIdx); hot {
		c.region, c.start = blk.hotCol, start
		return c, true
	}
	start, ok := blk.coldFieldStart(fieldIdx)
	if !ok {
		return numCol{}, false
	}
	coldNum, err := bc.stream(blk, kindColdNum)
	if err != nil {
		return numCol{}, false
	}
	c.region, c.start = coldNum, start
	return c, true
}

// at reads record k's value with its ClassAd kind, taking the cold-tail path only when the value
// escaped its slot.
func (c *numCol) at(k int, bc *blockCache) (colVal, bool) {
	v, cell := c.cell(k, bc)
	return v, cell == cellNumber
}

// cell is at saying why there is no value (see numCell). A caller that skips missing values must use
// this and give up on cellExpr: skipping an expression as if it were missing is a wrong answer.
func (c *numCol) cell(k int, bc *blockCache) (colVal, numCell) {
	base := k * c.blk.bitsStride
	if testBit(c.blk.bits[base:base+c.escBytes], c.fieldIdx) {
		return c.blk.escapedNumCell(k, c.fieldID, bc)
	}
	raw := readIntLE(c.region[c.start+k*c.width:], c.width, c.unsigned)
	if c.isReal {
		return colVal{f: math.Float64frombits(uint64(raw)), kind: akReal}, cellNumber
	}
	return colVal{f: float64(raw), i: raw, kind: akInt}, cellNumber
}

// fieldIntAt reads one record's raw slot value for a numeric field: a strided read of the uncompressed
// hot region, or the cold numeric column group (decompressed once, cached).
//
// scanInt does this for every record; an aggregate with a predicate needs it for the survivors only,
// which is the whole point of narrowing first.
func (b *columnarBlock) fieldIntAt(k, fieldIdx int, bc *blockCache) (int64, bool) {
	f := b.schema.fields[fieldIdx]
	if start, hot := b.hotColStart(fieldIdx); hot {
		return readIntLE(b.hotCol[start+k*f.width:], f.width, f.unsigned), true
	}
	start, ok := b.coldFieldStart(fieldIdx)
	if !ok {
		return 0, false
	}
	coldNum, err := bc.stream(b, kindColdNum)
	if err != nil {
		return 0, false
	}
	return readIntLE(coldNum[start+k*f.width:], f.width, f.unsigned), true
}

// schemaScanStatsMulti computes the aggregate inputs for aggID over the records satisfying every
// predicate. preds may be empty (aggregate everything) and may constrain fields other than aggID.
//
// A segment whose own schema lacks the aggregated field or any predicated one, and the active segment,
// fall back to a row walk reading only those attributes -- never a full ad decode.
//
// ok=false when a value involved is an expression, which the columns cannot evaluate.
func (c *Collection) schemaScanStatsMulti(aggID uint32, preds []fieldPred, bc *blockCache) (NumStats, bool) {
	// A predicate on the AGGREGATED attribute is applied during the value pass, not as a narrowing
	// pass of its own: the value is being read anyway, so testing it there costs nothing, while a
	// separate pass reads the same column twice. Skipping that fusion made
	// `max(ProcId) where ProcId >= 5` -- a shape the single-field path already served -- 1.8x slower.
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
	acc := newStatsAccum()
	lookups := make([]fieldLookup, len(otherPreds))
	for i, p := range otherPreds {
		lookups[i] = c.attrLookup(p.fieldID)
	}
	aggLookup := c.attrLookup(aggID)

	var cand candSet
	var sc predScratch
	ct := predsNeedCT(preds)
	zp := predZoneProbes(preds)
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			if ct && !w.ctFree {
				releaseWindows(wins) // a folded predicate is exact only in a proven window
				return NumStats{}, false
			}
			// No record here can match, so none contributes; see schemaScanCountMulti.
			if c.segZoneExcludes(w, zp, ct) {
				continue
			}
			cs := w.seg.colblk.Load()
			aggIdx, allIdxs, ok := resolveStatsFields(cs, aggID, preds)
			if !ok {
				if !bruteStatsMulti(c, w, s0, aggLookup, otherPreds, lookups, selfOK, &acc) {
					releaseWindows(wins)
					return NumStats{}, false
				}
				continue
			}
			// Narrowing uses only the OTHER fields; pruning may use them all, since ruling a block
			// out is sound for any predicate including one on the aggregated column.
			otherIdxs := make([]int, 0, len(otherPreds))
			for i, p := range preds {
				if p.fieldID != aggID {
					otherIdxs = append(otherIdxs, allIdxs[i])
				}
			}
			base := 0
			for _, blk := range cs.blocks {
				// Only the PREDICATE columns can rule a block out; the aggregated column's values
				// are the answer.
				if !blockMayMatch(blk, allIdxs, preds) {
					base += blk.n
					continue
				}
				// The schema carries every field, but a column may still not read (an unreadable
				// cold stream). Answer that block from its records rather than drop them.
				col, colOK := newNumCol(blk, aggIdx, aggID, bc)
				if !colOK || !blk.numColsReadable(otherIdxs, bc) {
					if !c.statsBlockRows(w, s0, cs, base, blk.n, aggLookup, otherPreds, lookups, selfOK, &acc) {
						releaseWindows(wins)
						return NumStats{}, false
					}
					base += blk.n
					continue
				}
				if len(otherPreds) == 0 {
					// One fused pass: visibility, value, and the aggregated column's own predicate.
					for k := 0; k < blk.n; k++ {
						gk := base + k
						if gk >= cs.offsLen() {
							break
						}
						o := cs.offAt(gk)
						if !w.visible(o, s0) {
							continue
						}
						nv, cell := col.cell(k, bc)
						if cell == cellExpr {
							releaseWindows(wins)
							return NumStats{}, false
						}
						if cell == cellNumber && selfOK(nv.f) {
							acc.add(nv)
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
					if !narrowByField(blk, otherIdxs[i], otherPreds[i], bc, &cand, &sc) {
						releaseWindows(wins)
						return NumStats{}, false
					}
				}
				for wi, word := range cand.w {
					for word != 0 {
						k := wi*64 + bits.TrailingZeros64(word)
						word &= word - 1
						nv, cell := col.cell(k, bc)
						if cell == cellExpr {
							releaseWindows(wins)
							return NumStats{}, false
						}
						if cell == cellNumber && selfOK(nv.f) {
							acc.add(nv)
						}
					}
				}
				base += blk.n
			}
		}
		releaseWindows(wins)
	}
	return acc.result(), true
}

// fieldLookup reads one attribute from a record's own wire form, given the dictionary of the segment
// the record came from (nil for a segment without one).
type fieldLookup func(d *segDictHandle, a wire.Ad) ([]byte, bool)

// attrLookup returns the wire lookup for an attribute in the form each record is stored in: by
// interned id in memory; on a persistent store by inline name, or -- in a segment with a dictionary --
// by that segment's local id for the name.
//
// The dictionary case is why it takes one. A row fallback reads records in their stored encoding, and
// an interned segment's records carry local ids, not names: looking one up by name finds nothing, so
// every row-path aggregate over such a segment counted its records as lacking the attribute. That
// stayed hidden while the row path ran only for segments with no block at all, and a dictionary-keyed
// columnarized segment whose own schema lacks a field -- the normal state while a schema change
// converges -- is exactly the case it got wrong.
func (c *Collection) attrLookup(id uint32) fieldLookup {
	if c.inline {
		if name, ok := c.intern.Name(id); ok {
			return func(d *segDictHandle, a wire.Ad) ([]byte, bool) { return c.wireLookupDict(d, a, name) }
		}
	}
	return func(_ *segDictHandle, a wire.Ad) ([]byte, bool) { return a.Lookup(id) }
}

// resolveStatsFields maps the aggregated field and every predicated field into this SEGMENT's schema.
// ok=false when any is absent, since segments sealed under different schemas coexist.
func resolveStatsFields(cs *colSegment, aggID uint32, preds []fieldPred) (int, []int, bool) {
	sch := cs.schema()
	if sch == nil {
		return 0, nil, false
	}
	aggIdx, ok := sch.byID[aggID]
	if !ok || !numericKind(sch.fields[aggIdx].kind) {
		return 0, nil, false
	}
	idxs, ok := resolveFields(cs, preds)
	if !ok {
		return 0, nil, false
	}
	return aggIdx, idxs, true
}

// bruteStatsMulti is the row fallback: walk a window's visible records, test the predicated
// attributes, and accumulate the aggregated one -- reading only those attributes from the wire ad.
func bruteStatsMulti(c *Collection, w segWindow, s0 uint64, aggLookup fieldLookup,
	preds []fieldPred, lookups []fieldLookup, selfOK func(float64) bool, acc *statsAccum) bool {
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
			if ww, err := c.wire(recRef{w: w, off: o, dict: w.dict()}, buf); err == nil {
				buf = ww
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
							acc.add(nv)
						}
					}
				}
			}
		}
		off += int(total)
	}
	return true
}

// statsBlockRows is the row fallback for one columnar block whose columns will not read: its visible
// records, read from the arena (see eachBlockRecord). Kept out of the column loop so that loop's
// code is the same whether or not a block ever falls back.
func (c *Collection) statsBlockRows(w segWindow, s0 uint64, cs *colSegment, base, n int,
	aggLookup fieldLookup, preds []fieldPred, lookups []fieldLookup,
	selfOK func(float64) bool, acc *statsAccum) bool {
	return c.eachBlockRecord(w, s0, cs, base, n, func(ad wire.Ad) bool {
		return statsFromAd(w.dict(), ad, aggLookup, preds, lookups, selfOK, acc)
	})
}

// statsFromAd is the row fallback's step for one record: test the predicated attributes and
// accumulate the aggregated one. ok=false when a value involved is an expression. bruteStatsMulti
// takes the same step inline; routing it through here measured 3-5% slower on the COLUMN pass of
// schemaScanStatsMulti, which does not even call it.
func statsFromAd(d *segDictHandle, ad wire.Ad, aggLookup fieldLookup, preds []fieldPred,
	lookups []fieldLookup, selfOK func(float64) bool, acc *statsAccum) bool {
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
		acc.add(nv)
	}
	return true
}
