package collections

import (
	"math"
	"math/bits"
	"sync/atomic"

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
//
// ints is the same conjunction lowered to integer comparisons, for the plane kernels, and intOK says
// whether every test lowered. It is derived ONCE per query rather than per block: the lowering depends
// only on the operator and the literal, and the thing that does vary per block -- whether the column
// is an integer one narrow enough for the two forms to agree -- is a separate check at the block (see
// intLowerable).
type fieldPred struct {
	fieldID uint32
	eval    func(float64) bool
	tests   []zoneTest
	ints    []intTest
	intOK   bool
	// ct marks a predicate from a query's CurrentTime-folded form (numPredsCT): it holds only for
	// records without CurrentTime, so a scan using it must answer from proven windows alone.
	ct bool
	// probes are the query conjuncts this predicate was built from, which is the form a SEGMENT's
	// zone map reasons about (zoneExcludes). Block zones cover only the fields a segment's own schema
	// carries, so a segment sealed before the field joined the schema has no block zone for it and
	// only its segment zone can rule it out.
	probes []vm.Probe
}

// segZonePrunes counts the segments a column aggregate skipped on the segment zone map, so a test
// can tell a pruned segment from one that was read and happened to contribute nothing.
var segZonePrunes atomic.Int64

// predZoneProbes collects the probes behind preds, for zoneExcludes. Every one is a required
// conjunct of the query preds came from, so any of them ruling a segment out is sound.
func predZoneProbes(preds []fieldPred) []vm.Probe {
	var out []vm.Probe
	for _, p := range preds {
		out = append(out, p.probes...)
	}
	return out
}

// segZoneExcludes is zoneExcludes for the column aggregates: it reports whether w's segment zone map
// proves no record in it satisfies probes, counting the prune. ct is whether the probes were computed
// with CurrentTime folded (see zoneExcludes); such probes prune only a window proven free of it.
func (c *Collection) segZoneExcludes(w segWindow, probes []vm.Probe, ct bool) bool {
	if len(probes) == 0 || !c.zoneExcludes(w, probes, ct) {
		return false
	}
	segZonePrunes.Add(1)
	return true
}

// numPredsCT is numPredsOnFields that also serves a query reading CurrentTime, through its folded
// form (vm.Query.Folded), when every segment is proven free of CurrentTime. The predicates it returns
// then carry ct, and a scan using them declines on meeting a window that is not proven (predsNeedCT):
// a write may land between this check and the scan's snapshot.
func (c *Collection) numPredsCT(q *vm.Query, s *adSchema) ([]fieldPred, bool) {
	if preds, ok := c.numPredsOnFields(q, s); ok {
		return preds, true
	}
	qf := c.foldFor(q)
	if qf == nil || !c.ctAllProven() {
		return nil, false
	}
	preds, ok := c.numPredsOnFields(qf, s)
	if !ok {
		return nil, false
	}
	for i := range preds {
		preds[i].ct = true
	}
	return preds, true
}

// predsNeedCT reports whether preds came from a CurrentTime-folded query (see numPredsCT).
func predsNeedCT(preds []fieldPred) bool {
	for _, p := range preds {
		if p.ct {
			return true
		}
	}
	return false
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
	srcs := make(map[uint32][]vm.Probe, len(probes))
	for _, p := range probes {
		id, ok := c.intern.LookupID(p.Attr)
		if !ok {
			return nil, false
		}
		idx, ok := s.byID[id]
		if !ok || !numericKind(s.fields[idx].kind) {
			return nil, false
		}
		// numPredProbe, not valUsable: this path compares each STORED value, so it can honour a
		// negated ordering probe exactly (see numPredProbe) where an index lookup cannot.
		up, ok := numPredProbe(id, p)
		if !ok || len(up.fvals) != 1 {
			return nil, false // present/absent/in/isnt or non-numeric: not a scalar comparison
		}
		if up.inexact || p.Op == "is" {
			// The comparison below is on float64, which cannot tell an integer past 2^53 from
			// its neighbors (see floatKeyOp), nor an integer from the real of equal value that
			// =?= must reject -- valUsable plans =?= as ==.
			return nil, false
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
		srcs[id] = append(srcs[id], p)
	}
	if len(order) == 0 {
		return nil, false
	}
	preds := make([]fieldPred, 0, len(order))
	for _, id := range order {
		list := cmps[id]
		p := fieldPred{fieldID: id, eval: list[0], tests: tests[id], probes: srcs[id]}
		if len(list) > 1 {
			p.eval = func(v float64) bool {
				for _, cmp := range list {
					if !cmp(v) {
						return false
					}
				}
				return true
			}
		}
		p.ints, p.intOK = lowerIntTests(p.tests)
		preds = append(preds, p)
	}
	return preds, true
}

// schemaScanCountMulti counts live records satisfying every predicate, one column pass per field.
//
// A segment whose own schema lacks any of the fields, and the active segment (no block), are counted
// by a row walk that reads only the predicated attributes -- not by decoding the ad. So a schema
// change never costs more than the segments it actually affects.
func (c *Collection) schemaScanCountMulti(preds []fieldPred, bc *blockCache) (int, bool) {
	lookups := make([]fieldLookup, len(preds))
	for i, p := range preds {
		lookups[i] = c.attrLookup(p.fieldID)
	}
	count := 0
	var cand candSet
	var sc predScratch
	ct := predsNeedCT(preds)
	zp := predZoneProbes(preds)
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			if ct && !w.ctFree {
				releaseWindows(wins)
				return 0, false
			}
			// The segment zone first: it is what rules out a segment whose own schema lacks a
			// predicated field, where the block zones below have nothing to say and the row walk
			// would read every record.
			if c.segZoneExcludes(w, zp, ct) {
				continue
			}
			cs := w.seg.colblk.Load()
			idxs, resolved := resolveFields(cs, preds)
			if !resolved {
				n, ok := bruteCountMulti(c, w, s0, preds, lookups)
				if !ok {
					releaseWindows(wins)
					return 0, false
				}
				count += n
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
				// A predicated column that will not read (an unreadable cold stream): count this
				// block's records from the arena rather than let the narrowing guess at them.
				if !blk.numColsReadable(idxs, bc) {
					if !c.eachBlockRecord(w, s0, cs, base, blk.n, func(ad wire.Ad) bool {
						match, ok := wirePredsMatch(w.dict(), ad, preds, lookups)
						if match {
							count++
						}
						return ok
					}) {
						releaseWindows(wins)
						return 0, false
					}
					base += blk.n
					continue
				}
				sc.prepare(blk.n)
				// Pass 0 establishes visibility, so later passes never touch a record no
				// snapshot can see.
				cand.setVisible(cs, w, s0, base, blk.n)
				for i := range preds {
					if cand.live == 0 {
						break // nothing left in this block; later columns would be read for nothing
					}
					if !narrowByField(blk, idxs[i], preds[i], bc, &cand, &sc) {
						releaseWindows(wins)
						return 0, false
					}
				}
				count += cand.live
				base += blk.n
			}
		}
		releaseWindows(wins)
	}
	return count, true
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

// narrowByField removes from cand every still-candidate record failing this field's predicate.
//
// Two forms. The PLANE form compares the whole column in one kernel call and intersects the result
// with the candidate set as word arithmetic, with no per-record read or write of the set at all; it
// serves an integer column whose comparisons lower into the integer domain (see lowerIntTests and
// intLowerable). The RECORD form is the original loop, kept for a real column -- whose slot holds
// math.Float64bits, so reading it as an integer would silently produce astronomical values -- for a
// column too wide for the two to agree, and for a literal that does not lower.
//
// ok=false when a candidate's value is one the columns cannot evaluate (cellExpr), or the column will
// not read at all: the caller gives the query back to the row path, since treating either as failing
// (or passing) the predicate is a wrong answer.
func narrowByField(blk *columnarBlock, idx int, p fieldPred, bc *blockCache, cand *candSet,
	sc *predScratch) bool {
	f := blk.schema.fields[idx]
	if p.intOK && intLowerable(f) {
		if plane, built := comparePlane(blk, idx, f, p.ints, bc, sc); built {
			if blk.escapeFree(idx) {
				cand.and(plane)
				return true
			}
			return fixEscapedPlane(blk, idx, p, bc, cand, plane)
		}
	}
	return narrowByRecord(blk, idx, f, p, bc, cand, sc)
}

// intLowerable reports whether a column's values and the float64s the predicate was compiled against
// order identically, which is what lets the integer kernels answer for it.
//
// Six bytes is the cutoff: nothing a six-byte slot holds reaches 2^48, well inside the integers
// float64 identifies individually, so float64(v) orders exactly as v does and the two forms cannot
// disagree about any literal at all. An eight-byte column can hold two values that share a float64,
// and there the agreement depends on where the LITERAL falls -- numPredsOnFields happens to bound it
// to (-2^53, 2^53), which is enough, but resting a correctness argument on a filter somewhere else
// is how the argument stops holding when a second caller appears. Eight bytes is the fitted width of
// last resort, so those columns keep the float64 loop and the question does not arise.
func intLowerable(f adField) bool { return f.kind == akInt && f.width <= 6 }

// fixEscapedPlane repairs the plane bits of the candidates whose value is not in the column, then
// intersects it. An escape means the value is MISSING or did not fit the slot, so the slot's bytes
// are undefined and the kernel's answer for that record is meaningless.
//
// It walks the CANDIDATES rather than the block, so a block whose set is already narrow pays for the
// records still in play and not for the ones an earlier pass removed. A non-candidate's plane bit is
// left as the kernel wrote it, garbage included, because the intersection below clears it anyway.
func fixEscapedPlane(blk *columnarBlock, idx int, p fieldPred, bc *blockCache, cand *candSet,
	plane []uint64) bool {
	for wi, word := range cand.w {
		for word != 0 {
			k := wi*64 + bits.TrailingZeros64(word)
			word &= word - 1
			if !testBit(blk.escapeAt(k), idx) {
				continue
			}
			pass := false
			// The value is not in the column: missing, wrong kind, or too wide for the slot. Absent
			// or non-numeric leaves the comparison UNDEFINED, so the record cannot satisfy the
			// conjunction.
			if nv, cell := blk.escapedNumCell(k, p.fieldID, bc); cell == cellExpr {
				return false
			} else if cell == cellNumber {
				pass = p.eval(nv.f)
			}
			if pass {
				plane[wi] |= 1 << uint(k&63)
			} else {
				plane[wi] &^= 1 << uint(k&63)
			}
		}
	}
	cand.and(plane)
	return true
}

// narrowByRecord is the per-record form: one tight typed load of the whole column, then the predicate
// closure over the candidates.
//
// It visits the SET BITS of the candidate set rather than every record, so a pass after a selective
// one costs what is left rather than what the block holds -- which the []bool it replaces could not
// do, since finding the survivors there meant reading a byte per record either way.
//
// Escaped records are read from the cold tail as they come up, so the batch load is unconditional --
// gating it on an escape-free block made it fire almost never (see loadIntBatch).
func narrowByRecord(blk *columnarBlock, idx int, f adField, p fieldPred, bc *blockCache,
	cand *candSet, sc *predScratch) bool {
	isReal := f.kind == akReal
	if !blk.loadIntBatch(idx, bc, sc.ints) {
		return narrowByScan(blk, idx, isReal, p, bc, cand)
	}
	escFree := blk.escapeFree(idx)
	live := 0
	for wi, word := range cand.w {
		keep := word
		for word != 0 {
			k := wi*64 + bits.TrailingZeros64(word)
			word &= word - 1
			var v float64
			if !escFree && testBit(blk.escapeAt(k), idx) {
				nv, cell := blk.escapedNumCell(k, p.fieldID, bc)
				if cell == cellExpr {
					return false
				}
				if cell != cellNumber {
					keep &^= 1 << uint(k&63)
					continue
				}
				v = nv.f
			} else if isReal {
				v = math.Float64frombits(uint64(sc.ints[k]))
			} else {
				v = float64(sc.ints[k])
			}
			if !p.eval(v) {
				keep &^= 1 << uint(k&63)
			}
		}
		cand.w[wi] = keep
		live += bits.OnesCount64(keep)
	}
	cand.live = live
	return true
}

// narrowByScan is the last resort: the column would not batch-load, so it is walked through scanInt's
// callback, which is what every form here replaced and is kept only for the blocks that need it.
func narrowByScan(blk *columnarBlock, idx int, isReal bool, p fieldPred, bc *blockCache,
	cand *candSet) bool {
	ok := true
	err := blk.scanInt(idx, bc, func(k int, present bool, v int64) {
		if !ok || !cand.test(k) {
			return
		}
		f := float64(v)
		if !present {
			// Escaped: read just this field from the cold tail. Absent or non-numeric leaves the
			// comparison UNDEFINED, so the record cannot satisfy the conjunction.
			nv, cell := blk.escapedNumCell(k, p.fieldID, bc)
			if cell == cellExpr {
				ok = false
				return
			}
			if cell != cellNumber {
				cand.clear(k)
				return
			}
			f = nv.f
		} else if isReal {
			f = math.Float64frombits(uint64(v))
		}
		if !p.eval(f) {
			cand.clear(k)
		}
	})
	// A column that will not read tested nothing, so every candidate would otherwise pass a
	// predicate it was never compared against.
	return ok && err == nil
}

// bruteCountMulti is the row fallback: walk a window's visible records and test the predicated
// attributes from their wire nodes, reading only those attributes rather than decoding the ad.
//
// ok=false when a predicated attribute holds an expression: comparing it needs evaluating it, which
// only the general scan does, and skipping it would undercount.
func bruteCountMulti(c *Collection, w segWindow, s0 uint64, preds []fieldPred, lookups []fieldLookup) (int, bool) {
	count := 0
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
				match, ok := wirePredsMatch(w.dict(), wire.Ad(ww), preds, lookups)
				if !ok {
					return 0, false
				}
				if match {
					count++
				}
			}
		}
		off += int(total)
	}
	return count, true
}
