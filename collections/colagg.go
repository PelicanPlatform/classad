package collections

import (
	"math"

	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Numeric aggregates over the columnar accelerator.
//
// The block already stores each hot numeric column contiguously and its scan hands back values,
// so counting was never the limit -- it was just the only consumer written. MIN/MAX/SUM/AVG are
// the same pass with a different accumulator, over the same per-block schema resolution, MVCC
// visibility and cold-tail escape handling.

// NumStats is one columnar pass's worth of aggregate inputs for a single numeric attribute, from
// which MIN, MAX, SUM, AVG and COUNT(attr) all follow.
//
// N counts the records whose value for the attribute is a number -- which is COUNT(attr)'s
// definition, and the divisor for AVG. Records where the attribute is absent or non-numeric
// contribute to none of these fields. Min/Max are meaningless when N == 0.
type NumStats struct {
	N   int     `json:"n"`
	Sum float64 `json:"sum"`
	Min float64 `json:"min"`
	Max float64 `json:"max"`
	// IntSum accumulates the INTEGER contributions in int64, exactly as the reference SUM does
	// -- a float64 accumulator loses precision past 2^53, so an all-integer sum rendered from
	// Sum would disagree with the scanning aggregator on large values.
	IntSum int64 `json:"intSum,omitempty"`
	// AnyReal records whether any contributing value was a REAL rather than an integer, which is
	// exactly the reference's promotion rule: SUM stays an integer unless a real appears, and
	// MIN/MAX keep their element's type. Without it the same query would print differently
	// depending on whether the accelerator was on.
	AnyReal bool `json:"anyReal,omitempty"`
	// AnyBool records that a boolean turned up in the column (only possible as an escaped value,
	// since a bool is not a numeric schema field). The reference coerces booleans to 1/0 and has
	// a further quirk for a lone boolean element; rather than reproduce that, a caller declines
	// and lets the scan answer. Pathological data, exact answer.
	AnyBool bool `json:"anyBool,omitempty"`
}

// NumStatsQuery computes the numeric aggregate inputs for attr over the records matching q, using
// the columnar scan. A nil q means every record.
//
// It returns ok=false when the columnar path cannot serve the request -- the accelerator is off,
// attr is not a numeric field of the current schema, or q is not a conjunction of scalar numeric
// comparisons ON attr ITSELF. That last restriction is the same one CountQuery has: a predicate
// over a different field would need a second column read per record, which this pass does not do.
// A caller that gets false scans instead.
func (c *Collection) NumStatsQuery(q *vm.Query, attr string) (NumStats, bool) {
	st := c.schemaScan.Load()
	if st == nil {
		return NumStats{}, false
	}
	if c.intern == nil {
		return NumStats{}, false
	}
	id, ok := c.intern.LookupID(attr)
	if !ok {
		return NumStats{}, false
	}
	idx, ok := st.schema.byID[id]
	if !ok || !numericKind(st.schema.fields[idx].kind) {
		return NumStats{}, false
	}
	// The predicate may constrain ANY numeric fields, not just the aggregated one. Requiring it to
	// constrain the aggregated attribute meant `max(ProcId) where JobStatus == 4` -- the shape a
	// dashboard asks -- fell to a record scan while `max(ProcId) where ProcId >= 5` did not.
	var preds []fieldPred
	if q != nil {
		var ok bool
		preds, ok = c.numPredsOnFields(q, st.schema)
		if !ok {
			return NumStats{}, false // not a conjunction of scalar comparisons on numeric fields
		}
	}
	// Record the read so the hot tier learns about it. The tier holds the top-N numeric fields by
	// query read demand, uncompressed; a cold field's column group has to be decompressed. But the
	// accelerated paths recorded NO demand, so a column that is only ever aggregated -- never
	// filtered on through a path that does record -- could not earn a slot, and serving the query
	// faster made it invisible to the mechanism meant to make it faster still. The feedback loop
	// was open.
	reads := make([]string, 0, len(preds)+1)
	reads = append(reads, attr)
	for _, p := range preds {
		if name, ok := c.schemaFieldName(p.fieldID); ok && name != attr {
			reads = append(reads, name)
		}
	}
	c.demand.recordReads(reads)

	return c.schemaScanStatsMulti(id, preds, st.cache), true
}

// numericKind reports whether a schema field's kind is one the numeric column scan can read.
func numericKind(k adKind) bool { return k == akInt || k == akReal }

// colVal is one value the column scan produced: f for comparisons and real arithmetic, i for the
// exact integer accumulation SUM needs, and kind to drive the reference's type promotion.
type colVal struct {
	f    float64
	i    int64
	kind adKind
}

// numPredOnField analyzes q as a conjunction of scalar numeric comparisons against a SINGLE
// numeric schema field, returning that field's intern id and the combined test.
//
// ok=false for anything else: a non-native query, no probes at all, a probe on an attribute the
// schema does not carry as a numeric field, probes spanning two fields, or a probe that is not a
// scalar comparison (present/absent/in/isnt, or a non-numeric operand).
func (c *Collection) numPredOnField(q *vm.Query, s *adSchema) (uint32, func(float64) bool, bool) {
	// ExactProbes, not Probes: this path answers from the probes and never re-verifies a record
	// against the query, so an over-approximation would silently over-count. Probes omits any
	// conjunct that is not `Attr OP literal`, and such a conjunct can still be NATIVE -- e.g.
	// `ProcId >= 5 && ClusterId != ProcId` -- so Native() alone is not the guarantee it looks like.
	probes, exact := q.ExactProbes()
	if !q.Native() || !exact {
		return 0, nil, false
	}
	fieldIdx := -1
	var fieldID uint32
	var cmps []func(float64) bool
	for _, p := range probes {
		id, ok := c.intern.LookupID(p.Attr)
		if !ok {
			return 0, nil, false
		}
		idx, ok := s.byID[id]
		if !ok || !numericKind(s.fields[idx].kind) {
			return 0, nil, false
		}
		if fieldIdx == -1 {
			fieldIdx, fieldID = idx, id
		} else if idx != fieldIdx {
			return 0, nil, false // more than one field: not this fast path
		}
		up, ok := valUsable(id, p)
		if !ok || len(up.fvals) != 1 {
			return 0, nil, false // present/absent/in/isnt or non-numeric: not a scalar comparison
		}
		cmp, ok := numCmp(up.op, up.fvals[0])
		if !ok {
			return 0, nil, false
		}
		cmps = append(cmps, cmp)
	}
	return fieldID, func(v float64) bool {
		for _, cmp := range cmps {
			if !cmp(v) {
				return false
			}
		}
		return true
	}, true
}

// scanNumValues calls fn with every live record's numeric value for fieldID: from each sealed
// segment's columnar block where that segment's OWN schema carries fieldID as a numeric field
// (hot column: no decode; cold column: one cached decode), and by row scan where it does not --
// including the active segment, which has no block. Records where the value is absent or
// non-numeric are skipped, so fn's call count is COUNT(attr).
//
// Resolving per block is what lets segments sealed under different schemas coexist: a schema
// change never rewrites existing segments, they simply fall back for attributes their schema
// lacks. It also decides the value conversion, because a REAL field's fixed slot holds
// math.Float64bits and scanInt returns those bits as an int64 -- reading them as an integer
// would silently produce astronomically wrong values.
func (c *Collection) scanNumValues(fieldID uint32, bc *blockCache, fn func(colVal)) {
	// The row fallback reads records straight from the arena, so it must honor the collection's
	// wire mode: look the field up by inline name on a persistent store, by interned id in memory.
	lookup := func(a wire.Ad) ([]byte, bool) { return a.Lookup(fieldID) }
	if c.inline {
		if name, ok := c.intern.Name(fieldID); ok {
			lookup = func(a wire.Ad) ([]byte, bool) { return a.LookupByName(name) }
		}
	}
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			cs := w.seg.colblk.Load()
			// Every row group in a segment is built under the same schema, so the field resolves
			// once per segment rather than once per group.
			bidx, isReal := -1, false
			if sch := cs.schema(); sch != nil {
				if idx, ok := sch.byID[fieldID]; ok {
					switch sch.fields[idx].kind {
					case akInt:
						bidx = idx
					case akReal:
						bidx, isReal = idx, true
					}
				}
			}
			if bidx < 0 {
				bruteNumValues(c, w, s0, lookup, fn)
				continue
			}
			// Walk the segment's row groups in record order, tracking each group's base so a
			// group-local record index maps back into the segment-wide offs array.
			base := 0
			for _, blk := range cs.blocks {
				blk.scanInt(bidx, bc, func(k int, present bool, v int64) {
					gk := base + k
					if gk >= len(cs.offs) {
						return // truncated offs: cannot establish visibility, so do not count it
					}
					o := cs.offs[gk]
					if !(recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0) {
						return // not visible at this snapshot
					}
					if present {
						if isReal {
							f := math.Float64frombits(uint64(v))
							fn(colVal{f: f, kind: akReal})
						} else {
							fn(colVal{f: float64(v), i: v, kind: akInt})
						}
						return
					}
					// Escaped: read only fieldID from the cold tail, not the whole record. (A full
					// reconstruct dominated the scan when a numeric attr has a value tail.)
					// The cold tail carries the wire node, so its kind is read from the node
					// itself rather than assumed from this block's schema field. With row groups
					// this decompresses only the escaped record's OWN group's tail.
					if nv, ok := blk.escapedNumVal(k, fieldID, bc); ok {
						fn(nv)
					}
				})
				base += blk.n
			}
		}
		releaseWindows(wins)
	}
}

// bruteNumValues is the row-scan fallback: walk a window's visible records, read the numeric
// field from the wire ad, and hand over each value found.
func bruteNumValues(c *Collection, w segWindow, s0 uint64, lookup func(wire.Ad) ([]byte, bool), fn func(colVal)) {
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
				if node, ok := lookup(wire.Ad(ww)); ok {
					if nv, ok := nodeColVal(node); ok {
						fn(nv)
					}
				}
			}
		}
		off += int(total)
	}
}

// nodeColVal reads a wire node as a numeric value, keeping the kind the reference's aggregates
// dispatch on. A boolean coerces to 1/0 as the reference does, and is reported as akBool so a
// caller can decline rather than approximate the reference's boolean quirks.
func nodeColVal(node []byte) (colVal, bool) {
	switch k, lit := nodeKind(node); k {
	case akInt:
		return colVal{f: float64(lit.Int), i: lit.Int, kind: akInt}, true
	case akReal:
		return colVal{f: lit.Real, kind: akReal}, true
	case akBool:
		var i int64
		if lit.Bool {
			i = 1
		}
		return colVal{f: float64(i), i: i, kind: akBool}, true
	}
	return colVal{}, false
}
