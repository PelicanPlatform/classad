package collections

import (
	"math"

	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// The hand-written column scan, generalized from one attribute to N.
//
// numPredOnField serves a conjunction of scalar comparisons on a SINGLE numeric field, so
// `RequestMemory > 4096` is columnar and `RequestMemory > 4096 && RequestCpus >= 4` is not -- the
// second falls all the way back to decoding whole ads, which is a cliff rather than a slope. Nothing
// about reading two columns instead of one requires the general evaluator: the shape is still
// `Attr OP literal`, just repeated.
//
// COLUMN-AT-A-TIME, not record-at-a-time. Each field is a separate pass over the block, narrowing a
// candidate set, so every pass is a tight strided read of one column rather than a walk that hops
// between columns per record. A selective first predicate makes every later pass cheaper, since
// excluded records are skipped, and a pass that empties the candidate set ends the block.
//
// SEMANTICS. Counting matches means counting records where the constraint is TRUE. For a conjunction
// that is: every comparison TRUE. A missing or non-numeric value makes its comparison UNDEFINED, and
// a conjunction with an UNDEFINED and no FALSE is UNDEFINED -- not TRUE -- so such a record is not
// counted. Dropping a record whose value is absent is therefore correct for every combination, which
// is what lets each pass simply narrow the candidate set.

// fieldPred is one field's combined test: the conjunction of every probe on that field.
//
// tests keeps each probe's (op, values) alongside the compiled eval, because a block's zone can rule
// the block out from the operator and bound alone -- and an eval closure cannot be reasoned about.
type fieldPred struct {
	fieldID uint32
	eval    func(float64) bool
	tests   []zoneTest
}

// zoneTest is one comparison in the form the zone map reasons about (see zoneMayMatch).
type zoneTest struct {
	op   string
	vals []float64
}

// numPredsOnFields analyzes q as a conjunction of scalar numeric comparisons over ANY number of
// numeric schema fields, returning one combined predicate per distinct field.
//
// ok=false for anything else: a non-native query, probes that do not exactly cover it (see
// ExactProbes -- this path answers from the probes and never re-verifies), an attribute the schema
// does not carry as a numeric field, or a probe that is not a scalar comparison.
func (c *Collection) numPredsOnFields(q *vm.Query, s *adSchema) ([]fieldPred, bool) {
	probes, exact := q.ExactProbes()
	if !q.Native() || !exact {
		return nil, false
	}
	order := make([]uint32, 0, len(probes))
	cmps := make(map[uint32][]func(float64) bool, len(probes))
	tests := make(map[uint32][]zoneTest, len(probes))
	for _, p := range probes {
		id, ok := c.intern.LookupID(p.Attr)
		if !ok {
			return nil, false
		}
		idx, ok := s.byID[id]
		if !ok || !numericKind(s.fields[idx].kind) {
			return nil, false
		}
		up, ok := valUsable(id, p)
		if !ok || len(up.fvals) != 1 {
			return nil, false // present/absent/in/isnt or non-numeric: not a scalar comparison
		}
		cmp, ok := numCmp(up.op, up.fvals[0])
		if !ok {
			return nil, false
		}
		if _, seen := cmps[id]; !seen {
			order = append(order, id)
		}
		cmps[id] = append(cmps[id], cmp)
		tests[id] = append(tests[id], zoneTest{op: up.op, vals: up.fvals})
	}
	if len(order) == 0 {
		return nil, false
	}
	preds := make([]fieldPred, 0, len(order))
	for _, id := range order {
		list := cmps[id]
		if len(list) == 1 {
			preds = append(preds, fieldPred{fieldID: id, eval: list[0], tests: tests[id]})
			continue
		}
		preds = append(preds, fieldPred{fieldID: id, eval: func(v float64) bool {
			for _, cmp := range list {
				if !cmp(v) {
					return false
				}
			}
			return true
		}, tests: tests[id]})
	}
	return preds, true
}

// schemaScanCountMulti counts live records satisfying every predicate, one column pass per field.
//
// A segment whose own schema lacks any of the fields, and the active segment (no block), are counted
// by a row walk that reads only the predicated attributes -- not by decoding the ad. So a schema
// change never costs more than the segments it actually affects.
func (c *Collection) schemaScanCountMulti(preds []fieldPred, bc *blockCache) int {
	lookups := make([]func(wire.Ad) ([]byte, bool), len(preds))
	for i, p := range preds {
		id := p.fieldID
		lookups[i] = func(a wire.Ad) ([]byte, bool) { return a.Lookup(id) }
		if c.inline {
			if name, ok := c.intern.Name(id); ok {
				lookups[i] = func(a wire.Ad) ([]byte, bool) { return a.LookupByName(name) }
			}
		}
	}
	count := 0
	var keep []bool
	var scratch []int64
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			cs := w.seg.colblk.Load()
			idxs, resolved := resolveFields(cs, preds)
			if !resolved {
				count += bruteCountMulti(c, w, s0, preds, lookups)
				continue
			}
			base := 0
			for _, blk := range cs.blocks {
				// Skip the whole block when its own column ranges rule it out -- no visibility
				// walk, no column reads, no cold-tail decompression. This is where row groups pay
				// off a second time: the finer the block, the more a selective predicate can skip.
				if !blockMayMatch(blk, idxs, preds) {
					base += blk.n
					continue
				}
				if cap(keep) < blk.n {
					keep = make([]bool, blk.n)
					scratch = make([]int64, blk.n)
				}
				keep, scratch = keep[:blk.n], scratch[:blk.n]
				// Pass 0 establishes visibility, so later passes never touch a record no
				// snapshot can see.
				live := 0
				for k := 0; k < blk.n; k++ {
					gk := base + k
					vis := gk < cs.offsLen()
					if vis {
						o := cs.offAt(gk)
						vis = recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0
					}
					keep[k] = vis
					if vis {
						live++
					}
				}
				for i := range preds {
					if live == 0 {
						break // nothing left in this block; later columns would be read for nothing
					}
					live = narrowByField(blk, idxs[i], preds[i], bc, keep, scratch)
				}
				count += live
				base += blk.n
			}
		}
		releaseWindows(wins)
	}
	return count
}

// blockMayMatch reports whether blk could hold a record satisfying every predicate.
func blockMayMatch(blk *columnarBlock, idxs []int, preds []fieldPred) bool {
	for i := range preds {
		if !blk.mayMatch(idxs[i], preds[i].tests) {
			return false
		}
	}
	return true
}

// resolveFields maps each predicate's field to its index in this SEGMENT's schema. Segments sealed
// under different schemas coexist, so a field present in the current schema may be absent here.
func resolveFields(cs *colSegment, preds []fieldPred) ([]int, bool) {
	sch := cs.schema()
	if sch == nil {
		return nil, false
	}
	idxs := make([]int, len(preds))
	for i, p := range preds {
		idx, ok := sch.byID[p.fieldID]
		if !ok || !numericKind(sch.fields[idx].kind) {
			return nil, false
		}
		idxs[i] = idx
	}
	return idxs, true
}

// narrowByField clears keep[k] for every still-candidate record failing this field's predicate, and
// returns how many survive. A real field's slot holds math.Float64bits, so the raw value is
// converted by kind -- reading a real as an integer would silently produce astronomical values.
func narrowByField(blk *columnarBlock, idx int, p fieldPred, bc *blockCache, keep []bool, scratch []int64) int {
	isReal := blk.schema.fields[idx].kind == akReal
	live := 0
	// Batch form: one tight typed load of the whole column, then one predicate loop. Escaped records
	// are read from the cold tail as they come up, so the batch load is unconditional -- gating it on
	// an escape-free block made it fire almost never (see loadIntBatch).
	if len(scratch) >= blk.n && blk.loadIntBatch(idx, bc, scratch) {
		escFree := blk.escapeFree(idx)
		for k := 0; k < blk.n; k++ {
			if !keep[k] {
				continue
			}
			var v float64
			if !escFree && testBit(blk.escapeAt(k), idx) {
				// The value is not in the column: missing, wrong kind, or too wide for the slot.
				nv, ok := blk.escapedNumVal(k, p.fieldID, bc)
				if !ok {
					keep[k] = false
					continue
				}
				v = nv.f
			} else if isReal {
				v = math.Float64frombits(uint64(scratch[k]))
			} else {
				v = float64(scratch[k])
			}
			if !p.eval(v) {
				keep[k] = false
				continue
			}
			live++
		}
		return live
	}
	_ = blk.scanInt(idx, bc, func(k int, present bool, v int64) {
		if !keep[k] {
			return
		}
		if !present {
			// Escaped: read just this field from the cold tail. Absent or non-numeric leaves the
			// comparison UNDEFINED, so the record cannot satisfy the conjunction.
			nv, ok := blk.escapedNumVal(k, p.fieldID, bc)
			if !ok || !p.eval(nv.f) {
				keep[k] = false
				return
			}
			live++
			return
		}
		f := float64(v)
		if isReal {
			f = math.Float64frombits(uint64(v))
		}
		if !p.eval(f) {
			keep[k] = false
			return
		}
		live++
	})
	return live
}

// bruteCountMulti is the row fallback: walk a window's visible records and test the predicated
// attributes from their wire nodes, reading only those attributes rather than decoding the ad.
func bruteCountMulti(c *Collection, w segWindow, s0 uint64, preds []fieldPred, lookups []func(wire.Ad) ([]byte, bool)) int {
	count := 0
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
				ok := true
				for i := range preds {
					node, found := lookups[i](wire.Ad(ww))
					if !found {
						ok = false
						break
					}
					nv, isNum := nodeColVal(node)
					if !isNum || !preds[i].eval(nv.f) {
						ok = false
						break
					}
				}
				if ok {
					count++
				}
			}
		}
		off += int(total)
	}
	return count
}
