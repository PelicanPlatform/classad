package collections

import (
	"math"

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
type statsAccum struct{ out NumStats }

func newStatsAccum() statsAccum {
	return statsAccum{out: NumStats{Min: math.Inf(1), Max: math.Inf(-1)}}
}

func (a *statsAccum) add(nv colVal) {
	a.out.N++
	a.out.Sum += nv.f
	switch nv.kind {
	case akInt:
		a.out.IntSum += nv.i
	case akReal:
		a.out.AnyReal = true
	case akBool:
		a.out.IntSum += nv.i
		a.out.AnyBool = true
	}
	if nv.f < a.out.Min {
		a.out.Min = nv.f
	}
	if nv.f > a.out.Max {
		a.out.Max = nv.f
	}
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
	blk       *columnarBlock
	fieldIdx  int
	fieldID   uint32
	escBytes  int
	hotOff    int // byte offset within a record's hot region; -1 when the column is cold
	coldStart int
	width     int
	unsigned  bool
	isReal    bool
	coldNum   []byte // decompressed cold-numeric region, nil for a hot column
}

// newNumCol resolves a column for repeated reads. ok=false when the field is not numeric here, or its
// cold region cannot be decompressed.
func newNumCol(blk *columnarBlock, fieldIdx int, fieldID uint32, bc *blockCache) (numCol, bool) {
	f := blk.schema.fields[fieldIdx]
	c := numCol{
		blk: blk, fieldIdx: fieldIdx, fieldID: fieldID, escBytes: blk.schema.escBytes,
		hotOff: -1, width: f.width, unsigned: f.unsigned, isReal: f.kind == akReal,
	}
	if off, hot := blk.hotFieldOff[fieldIdx]; hot {
		c.hotOff = off
		return c, true
	}
	start, ok := blk.coldFieldStart[fieldIdx]
	if !ok {
		return numCol{}, false
	}
	coldNum, err := bc.stream(blk, kindColdNum)
	if err != nil {
		return numCol{}, false
	}
	c.coldStart, c.coldNum = start, coldNum
	return c, true
}

// at reads record k's value with its ClassAd kind, taking the cold-tail path only when the value
// escaped its slot.
func (c *numCol) at(k int, bc *blockCache) (colVal, bool) {
	base := k * c.blk.hotStride
	if testBit(c.blk.hot[base:base+c.escBytes], c.fieldIdx) {
		return c.blk.escapedNumVal(k, c.fieldID, bc)
	}
	var raw int64
	if c.hotOff >= 0 {
		raw = readIntLE(c.blk.hot[base+c.hotOff:], c.width, c.unsigned)
	} else {
		raw = readIntLE(c.coldNum[c.coldStart+k*c.width:], c.width, c.unsigned)
	}
	if c.isReal {
		return colVal{f: math.Float64frombits(uint64(raw)), kind: akReal}, true
	}
	return colVal{f: float64(raw), i: raw, kind: akInt}, true
}

// fieldIntAt reads one record's raw slot value for a numeric field: a strided read of the uncompressed
// hot region, or the cold numeric column group (decompressed once, cached).
//
// scanInt does this for every record; an aggregate with a predicate needs it for the survivors only,
// which is the whole point of narrowing first.
func (b *columnarBlock) fieldIntAt(k, fieldIdx int, bc *blockCache) (int64, bool) {
	f := b.schema.fields[fieldIdx]
	if off, hot := b.hotFieldOff[fieldIdx]; hot {
		return readIntLE(b.hot[k*b.hotStride+off:], f.width, f.unsigned), true
	}
	start, ok := b.coldFieldStart[fieldIdx]
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
func (c *Collection) schemaScanStatsMulti(aggID uint32, preds []fieldPred, bc *blockCache) NumStats {
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
	lookups := make([]func(wire.Ad) ([]byte, bool), len(otherPreds))
	for i, p := range otherPreds {
		lookups[i] = c.attrLookup(p.fieldID)
	}
	aggLookup := c.attrLookup(aggID)

	var keep []bool
	var scratch []int64
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			cs := w.seg.colblk.Load()
			aggIdx, allIdxs, ok := resolveStatsFields(cs, aggID, preds)
			if !ok {
				bruteStatsMulti(w, s0, aggLookup, otherPreds, lookups, selfOK, &acc)
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
				col, colOK := newNumCol(blk, aggIdx, aggID, bc)
				if !colOK {
					base += blk.n
					continue
				}
				if len(otherPreds) == 0 {
					// One fused pass: visibility, value, and the aggregated column's own predicate.
					for k := 0; k < blk.n; k++ {
						gk := base + k
						if gk >= len(cs.offs) {
							break
						}
						o := cs.offs[gk]
						if !(recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0) {
							continue
						}
						if nv, ok := col.at(k, bc); ok && selfOK(nv.f) {
							acc.add(nv)
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
				for k := 0; k < blk.n; k++ {
					gk := base + k
					vis := gk < len(cs.offs)
					if vis {
						o := cs.offs[gk]
						vis = recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0
					}
					keep[k] = vis
					if vis {
						live++
					}
				}
				for i := range otherPreds {
					if live == 0 {
						break
					}
					live = narrowByField(blk, otherIdxs[i], otherPreds[i], bc, keep, scratch)
				}
				if live > 0 {
					for k := 0; k < blk.n; k++ {
						if !keep[k] {
							continue
						}
						if nv, ok := col.at(k, bc); ok && selfOK(nv.f) {
							acc.add(nv)
						}
					}
				}
				base += blk.n
			}
		}
		releaseWindows(wins)
	}
	return acc.result()
}

// attrLookup returns the wire lookup for an attribute in this collection's mode: by interned id in
// memory, by inline name on a persistent store.
func (c *Collection) attrLookup(id uint32) func(wire.Ad) ([]byte, bool) {
	if c.inline {
		if name, ok := c.intern.Name(id); ok {
			return func(a wire.Ad) ([]byte, bool) { return a.LookupByName(name) }
		}
	}
	return func(a wire.Ad) ([]byte, bool) { return a.Lookup(id) }
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
func bruteStatsMulti(w segWindow, s0 uint64, aggLookup func(wire.Ad) ([]byte, bool),
	preds []fieldPred, lookups []func(wire.Ad) ([]byte, bool), selfOK func(float64) bool, acc *statsAccum) {
	var buf []byte
	for off := 0; off < w.used; {
		o := uint32(off)
		total := recTotalLen(w.data, o)
		if total == 0 {
			break
		}
		if !recIsMarker(w.data, o) && recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0 {
			if ww, err := w.codec.Decompress(buf[:0], recAd(w.data, o)); err == nil {
				buf = ww
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
							acc.add(nv)
						}
					}
				}
			}
		}
		off += int(total)
	}
}
