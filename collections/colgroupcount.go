package collections

import (
	"math"
	"math/bits"
	"sort"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// GROUP BY one column, answered from the columns.
//
// The ungrouped count already reads a column and never decodes a record:
//
//	count(*) where NumJobStarts < 10                                        38ms
//	NumJobStarts, count(*) where NumJobStarts < 10 GROUP BY NumJobStarts   8112ms
//
// 210x apart on the same predicate over the same 631,983 matching records, with the group column HOT. The
// grouped form declined every columnar path -- an index answers only the unconstrained case and only for a
// categorically indexed column, the count fast path is gated on there being no grouping, and the general
// columnar aggregate has no attribute to aggregate for COUNT(*) -- and fell through to projecting every
// matching record one at a time.
//
// But a GROUP BY on one numeric column IS a histogram of that column, and the count scan is already walking
// it. This does the same narrowing passes and then reads the group column for the survivors, bucketing them
// instead of only counting them. MIN/MAX/SUM/AVG/COUNT(attr) per group come from reading their columns in
// the same pass, since the records have already been narrowed.
//
// Memory is one entry per distinct group value, which is what the scanning aggregator this replaces also
// builds -- so a high-cardinality group column is no worse here, just faster.
//
// A STRING group column takes the same route, reading the group value through the per-block string
// dictionary instead of a numeric slot; see colgroupstr.go for why that is cheap. That case was the same
// 200x apart from the numeric one (21.1 s against 90 ms on a 726k-record window) purely for the column's
// type.

// GroupCount is one group of a GROUP BY: the group column's value and how many records held it.
type GroupCount struct {
	Value classad.Value
	Count int
}

// GroupStats is one group of a GROUP BY with value aggregates: the group column's value, how many records
// fell in the group, and one NumStats per requested aggregate attribute, in the order requested.
type GroupStats struct {
	Value classad.Value
	Count int
	Stats []NumStats
}

// GroupCountConstraint parses a constraint string and, if the shape is columnar-eligible, answers
// the per-value counts of groupAttr over the matching records (see GroupCountQuery). ok=false ⇒
// use the normal grouping path.
func (c *Collection) GroupCountConstraint(constraint, groupAttr string) ([]GroupCount, bool) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, false
	}
	return c.GroupCountQuery(q, groupAttr)
}

// GroupCountQuery answers `SELECT groupAttr, COUNT(*) ... WHERE q GROUP BY groupAttr` from the columnar
// blocks, returning the groups sorted by value. See GroupStatsQuery, of which this is the no-value-
// aggregate case.
func (c *Collection) GroupCountQuery(q *vm.Query, groupAttr string) ([]GroupCount, bool) {
	return countsOf(c.GroupStatsQuery(q, groupAttr, nil))
}

// GroupCountAll is GroupCountQuery with no constraint: the histogram of the whole column.
func (c *Collection) GroupCountAll(groupAttr string) ([]GroupCount, bool) {
	return countsOf(c.GroupStatsAll(groupAttr, nil))
}

// countsOf drops the (absent) aggregate columns from a grouped-stats result, so the count API is the
// no-aggregate case of the one scan rather than a second scan that could drift from it.
func countsOf(gs []GroupStats, ok bool) ([]GroupCount, bool) {
	if !ok {
		return nil, false
	}
	out := make([]GroupCount, len(gs))
	for i, g := range gs {
		out[i] = GroupCount{Value: g.Value, Count: g.Count}
	}
	return out, true
}

// GroupStatsConstraint parses a constraint string and, if the shape is columnar-eligible, answers the
// per-group record count plus a NumStats per aggAttr over the matching records (see GroupStatsQuery).
// ok=false ⇒ use the normal grouping path.
func (c *Collection) GroupStatsConstraint(constraint, groupAttr string, aggAttrs []string) ([]GroupStats, bool) {
	return c.GroupStatsConstraintBucketed(constraint, groupAttr, 0, aggAttrs)
}

// GroupStatsConstraintBucketed is GroupStatsConstraint with the group column floored into buckets of
// bucket (0 = the raw value). See GroupStatsQueryBucketed.
func (c *Collection) GroupStatsConstraintBucketed(constraint, groupAttr string, bucket int64, aggAttrs []string) ([]GroupStats, bool) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, false
	}
	return c.GroupStatsQueryBucketed(q, groupAttr, bucket, aggAttrs)
}

// GroupStatsQuery answers `SELECT groupAttr, COUNT(*), <aggregates over aggAttrs> ... WHERE q GROUP BY
// groupAttr` from the columnar blocks, returning the groups sorted by value. Each group carries a
// NumStats per aggAttr, from which the caller renders MIN/MAX/SUM/AVG/COUNT(attr) -- the same inputs
// NumStatsQuery returns for the ungrouped case, so the rendering is shared and cannot diverge.
//
// ok=false when the shape is not columnar-servable -- no accelerator, a non-native constraint, a predicate
// that is not a conjunction of scalar numeric comparisons, a group attribute the schema does not carry as
// a numeric or string field, or an aggregate attribute it does not carry as a numeric one -- and the
// caller then scans as before.
//
// It also declines mid-flight if a matching record's GROUP value is one only EVALUATION can produce: an
// expression, a list, a nested ad. Such a record still forms a group -- the row path emits a group per
// distinct rendered value -- so dropping it would return a result silently missing groups, and the query
// goes back to the scan that can render it. Every SCALAR shape is now served instead of declined: a
// number, a string, a boolean, and `undefined` for a record whose attribute is absent, each rendering as
// the scan renders it.
//
// An absent AGGREGATE value is different and is simply skipped: MIN/SUM/COUNT(attr) over a set that does
// not include the record is what the reference computes for a record whose attribute is undefined. Only
// the group column decides which rows exist.
//
// Segments whose own schema lacks a field fall back to a row walk that reads only the attributes involved,
// so a schema change costs those segments and not the query.
func (c *Collection) GroupStatsQuery(q *vm.Query, groupAttr string, aggAttrs []string) ([]GroupStats, bool) {
	return c.GroupStatsQueryBucketed(q, groupAttr, 0, aggAttrs)
}

// GroupStatsQueryBucketed is GroupStatsQuery grouping by the group column FLOORED into buckets of
// bucket (0 = the raw value, i.e. GroupStatsQuery). This is the shape a time series asks -- "per day",
// not "per second".
//
// Only a NUMBER can be floored, so a record whose group value is a string, a boolean, undefined or
// error is DROPPED from a bucketed result rather than declining it: that is what the scanning
// aggregator does with one (db.bucketKeyText runs number(v), which rejects all four). The unbucketed
// form groups all of them, because there a column read can render them.
//
// A bucketed GROUP BY on a STRING column therefore has no groups at all, which is the scan's answer too.
func (c *Collection) GroupStatsQueryBucketed(q *vm.Query, groupAttr string, bucket int64, aggAttrs []string) ([]GroupStats, bool) {
	st := c.schemaScan.Load()
	if st == nil || c.intern == nil || q == nil {
		return nil, false
	}
	groupID, aggIDs, ok := c.resolveGroupAttrs(st, groupAttr, aggAttrs)
	if !ok {
		return nil, false
	}
	// First tier: a conjunction of numeric comparisons against literals, narrowed column by column with
	// zone-map pruning. Fastest, and the shape a history dashboard asks most.
	if preds, pok := c.numPredsCT(q, st.schema); pok {
		if gs, served := c.groupStats(groupID, aggIDs, bucket, preds, st); served || !predsNeedCT(preds) {
			return gs, served
		}
		// A window turned out unproven: the per-window tier below still serves the query.
	}
	// Second tier: evaluate the query itself against the columns -- a column at a time where the
	// expression allows it, per record where it does not. This serves any NATIVE query, so a string
	// comparison, an attribute-to-attribute comparison, arithmetic or a disjunction groups from the
	// columns instead of falling to a record scan, matching what the ungrouped count already served.
	//
	// Skipped when an index could prune the row path instead, for the reason CountQuery skips it: this
	// evaluates EVERY visible record, so against a selective indexed constraint the scan wins.
	if c.indexCanPrune(q) {
		return nil, false
	}
	acc, ok := c.vecGroupStats(q, groupID, aggIDs, bucket, st)
	if !ok {
		return nil, false
	}
	return c.shapeGroupStats(acc, groupID, aggIDs)
}

// GroupStatsAll is GroupStatsQuery with no constraint: the whole column's histogram plus aggregates. The
// caller is asserting the query matches every record, which only it can know (see db.IsMatchAll) -- there
// is no predicate here to check that against.
//
// This is not reachable through GroupStatsQuery: an unconstrained query has no probes to analyze, so the
// predicate analysis declines it. And the index path that answers other unconstrained grouped counts only
// covers CATEGORICALLY indexed attributes, so `GROUP BY <numeric>` with no WHERE fell to a record scan the
// same way the constrained form did.
func (c *Collection) GroupStatsAll(groupAttr string, aggAttrs []string) ([]GroupStats, bool) {
	return c.GroupStatsAllBucketed(groupAttr, 0, aggAttrs)
}

// GroupStatsAllBucketed is GroupStatsAll with the group column floored into buckets of bucket
// (0 = the raw value). See GroupStatsQueryBucketed for what bucketing changes.
func (c *Collection) GroupStatsAllBucketed(groupAttr string, bucket int64, aggAttrs []string) ([]GroupStats, bool) {
	st := c.schemaScan.Load()
	if st == nil || c.intern == nil {
		return nil, false
	}
	groupID, aggIDs, ok := c.resolveGroupAttrs(st, groupAttr, aggAttrs)
	if !ok {
		return nil, false
	}
	return c.groupStats(groupID, aggIDs, bucket, nil, st)
}

// resolveGroupAttrs interns the group and aggregate attribute names and checks each is carried by the
// collection-wide schema as a field this can read. A name repeated in aggAttrs resolves to the same id,
// which costs one extra read per record and keeps the result positional -- callers ask for MIN(x) and
// MAX(x).
//
// The GROUP column may be numeric or string; an AGGREGATE column must be numeric. That asymmetry is the
// shapes themselves: MIN/SUM/AVG of a string is not something the columns compute, while grouping by one
// only needs its identity.
func (c *Collection) resolveGroupAttrs(st *schemaScanState, groupAttr string, aggAttrs []string) (uint32, []uint32, bool) {
	field := func(name string, want func(adKind) bool) (uint32, bool) {
		id, ok := c.intern.LookupID(name)
		if !ok {
			return 0, false
		}
		idx, ok := st.schema.byID[id]
		if !ok || !want(st.schema.fields[idx].kind) {
			return 0, false
		}
		return id, true
	}
	groupID, ok := field(groupAttr, groupableKind)
	if !ok {
		return 0, nil, false
	}
	var aggIDs []uint32
	for _, a := range aggAttrs {
		id, ok := field(a, numericKind)
		if !ok {
			return 0, nil, false
		}
		aggIDs = append(aggIDs, id)
	}
	return groupID, aggIDs, true
}

// groupStats runs the scan and shapes its result: the part GroupStatsQuery and GroupStatsAll share once
// the columns and the predicate set are resolved. A nil preds means every visible record survives.
func (c *Collection) groupStats(groupID uint32, aggIDs []uint32, bucket int64, preds []fieldPred, st *schemaScanState) ([]GroupStats, bool) {
	acc, ok := c.schemaScanGroupStats(groupID, aggIDs, bucket, preds, st.cache)
	if !ok {
		return nil, false
	}
	return c.shapeGroupStats(acc, groupID, aggIDs)
}

// shapeGroupStats turns an accumulator map into the sorted result and records the read demand. Shared by
// every tier, so the shape and order of the answer cannot depend on which one produced it.
func (c *Collection) shapeGroupStats(ga *groupAccum, groupID uint32, aggIDs []uint32) ([]GroupStats, bool) {
	out := make([]GroupStats, 0, len(ga.acc))
	for raw, a := range ga.acc {
		g := GroupStats{Value: raw.value(), Count: a.n}
		if len(aggIDs) > 0 {
			g.Stats = make([]NumStats, len(aggIDs))
			for i := range a.stats {
				g.Stats[i] = a.stats[i].result()
			}
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return groupLess(out[i].Value, out[j].Value) })
	// Reading a column is demand for it, the same as any other query that touches one.
	read := make([]string, 0, 1+len(aggIDs))
	for _, id := range append([]uint32{groupID}, aggIDs...) {
		if name, ok := c.schemaFieldName(id); ok {
			read = append(read, name)
		}
	}
	c.demand.recordReads(read)
	return out, true
}

// groupKey identifies one group: the raw slot value for a number or a boolean, the text for a string,
// and the kind, so an int and a real that share a bit pattern do not collide and each renders as the
// type it was stored as.
//
// The string field costs one header per GROUP, not per record -- the map holds one entry per distinct
// value either way -- so carrying it here is cheaper than the alternative of a second side table keyed
// by something narrower. Keeping one key type also keeps the four tiers, groupLess and shapeGroupStats
// on one definition of group identity, which is the property that stops them disagreeing.
type groupKey struct {
	raw  int64
	s    string
	kind groupValKind
}

// groupKeyOf turns a numeric column value into a map key. A boolean reaches this only from a cold tail
// (a numeric column's slot holds numbers), and keeps its kind so it renders "true"/"false" as the scan
// does rather than as the 1/0 the aggregates coerce it to.
func groupKeyOf(nv colVal) groupKey {
	switch nv.kind {
	case akReal:
		return groupKey{raw: int64(math.Float64bits(nv.f)), kind: gkReal}
	case akBool:
		return groupKey{raw: nv.i, kind: gkBool}
	}
	return groupKey{raw: nv.i, kind: gkInt}
}

// number returns the key's numeric value, which is what a bucket floors.
//
// A boolean is deliberately NOT a number here, matching db.bucketKeyText: it runs number(v), which
// rejects a ClassAd boolean, so the scan drops a boolean from a bucketed series rather than
// bucketing it as 0 or 1.
func (k groupKey) number() (float64, bool) {
	switch k.kind {
	case gkInt:
		return float64(k.raw), true
	case gkReal:
		return math.Float64frombits(uint64(k.raw)), true
	}
	return 0, false
}

func (k groupKey) value() classad.Value {
	switch k.kind {
	case gkReal:
		return classad.NewRealValue(math.Float64frombits(uint64(k.raw)))
	case gkString:
		return classad.NewStringValue(k.s)
	case gkBool:
		return classad.NewBoolValue(k.raw != 0)
	case gkUndef:
		return classad.NewUndefinedValue()
	case gkError:
		return classad.NewErrorValue()
	}
	return classad.NewIntValue(k.raw)
}

// groupRank buckets a group value by type for ordering. The buckets are tested in the order a value is
// classified for RENDERING -- undefined and error before bool, bool before string, string before number
// -- so two values that sort into one bucket are exactly the two a caller will render comparably, and
// the order cannot drift from the labels.
func groupRank(v classad.Value) int {
	switch {
	case v.IsUndefined():
		return 3
	case v.IsError():
		return 4
	case v.IsBool():
		return 2
	case v.IsString():
		return 1
	case v.IsInteger(), v.IsReal():
		return 0
	}
	return 5
}

// groupLess orders groups by value, ints before reals only when they are equal, so ORDER BY on the group
// column gets the order it expects without the caller re-deriving it.
//
// A group set is no longer necessarily all numbers: a string column groups by text, and a record missing
// the attribute groups as `undefined`. Mixed sets are ordered by type first -- numbers, then strings,
// then booleans, then undefined and error -- because there is no meaningful order ACROSS those and an
// arbitrary-but-total one at least keeps the answer deterministic between runs.
func groupLess(a, b classad.Value) bool {
	ar, br := groupRank(a), groupRank(b)
	if ar != br {
		return ar < br
	}
	switch ar {
	case 0:
		af, _ := a.NumberValue()
		bf, _ := b.NumberValue()
		// NaN sorts last rather than comparing false against everything, which would leave it
		// anywhere and stop equal values from landing next to each other.
		if an, bn := af != af, bf != bf; an || bn {
			return !an && bn
		}
		return af < bf
	case 1:
		as, _ := a.StringValue()
		bs, _ := b.StringValue()
		// The evaluator's own case-insensitive comparison, which is also the order the block
		// dictionaries are sorted in, with a byte tiebreak so values that fold equal -- distinct
		// groups, since the scan groups by exact text -- still have a stable place.
		if c := classad.CompareStringsFold(as, bs); c != 0 {
			return c < 0
		}
		return as < bs
	case 2:
		ab, _ := a.BoolValue()
		bb, _ := b.BoolValue()
		return !ab && bb
	case 5:
		return a.String() < b.String()
	}
	return false // undefined and error are one group each: nothing to order within
}

// groupAcc is one group's accumulation: the record count plus one aggregate accumulator per requested
// aggregate column.
type groupAcc struct {
	n     int
	stats []statsAccum
}

func newGroupAcc(nAgg int) *groupAcc {
	g := &groupAcc{}
	if nAgg > 0 {
		g.stats = make([]statsAccum, nAgg)
		for i := range g.stats {
			g.stats[i] = newStatsAccum()
		}
	}
	return g
}

// schemaScanGroupStats buckets the group column for every record satisfying preds -- one narrowing pass
// per predicate column, then one read of the group column and of each aggregate column for the survivors.
//
// ok=false means a surviving record's GROUP value is one only evaluation can produce, which this cannot
// turn into a group (see GroupStatsQuery); the partial result is then discarded and the caller scans.
func (c *Collection) schemaScanGroupStats(groupID uint32, aggIDs []uint32, bucket int64, preds []fieldPred,
	bc *blockCache) (*groupAccum, bool) {
	acc := newGroupAccum(bucket)
	lookups := make([]func(wire.Ad) ([]byte, bool), len(preds))
	for i, p := range preds {
		lookups[i] = c.attrLookup(p.fieldID)
	}
	groupLookup := c.attrLookup(groupID)
	aggLookups := make([]func(wire.Ad) ([]byte, bool), len(aggIDs))
	for i, id := range aggIDs {
		aggLookups[i] = c.attrLookup(id)
	}

	var cand candSet
	var sc predScratch
	var gsc groupColScratch
	aggCols := make([]numCol, len(aggIDs))
	ct := predsNeedCT(preds)
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			if ct && !w.ctFree {
				releaseWindows(wins) // a folded predicate is exact only in a proven window
				return nil, false
			}
			cs := w.seg.colblk.Load()
			groupIdx, aggIdxs, predIdxs, ok := resolveGroupFields(cs, groupID, aggIDs, preds)
			if !ok {
				if !bruteGroupStats(c, w, s0, groupLookup, aggLookups, preds, lookups, acc) {
					releaseWindows(wins)
					return nil, false
				}
				continue
			}
			base := 0
			for _, blk := range cs.blocks {
				// Only the PREDICATE columns can rule a block out; the group and aggregate columns'
				// values are the answer.
				if !blockMayMatch(blk, predIdxs, preds) {
					base += blk.n
					continue
				}
				col, colOK := newGroupCol(blk, groupIdx, groupID, bc, &gsc)
				if !colOK {
					// The block's schema carries the field but its column will not read (an
					// unreadable cold stream, an unreadable dictionary). Skipping the block would
					// drop its records from their groups without saying so, so the query goes back
					// to the scan.
					releaseWindows(wins)
					return nil, false
				}
				for i := range aggIDs {
					aggCols[i], colOK = newNumCol(blk, aggIdxs[i], aggIDs[i], bc)
					if !colOK {
						releaseWindows(wins)
						return nil, false
					}
				}
				sc.prepare(blk.n)
				cand.setVisible(cs, w, s0, base, blk.n)
				for i := range preds {
					if cand.live == 0 {
						break
					}
					if !narrowByField(blk, predIdxs[i], preds[i], bc, &cand, &sc) {
						releaseWindows(wins)
						return nil, false
					}
				}
				// One test per 64 records rules out a word of them, and the survivors are
				// reached by their set bits rather than by reading a byte for each of the
				// records an earlier pass already removed.
				for wi, word := range cand.w {
					for word != 0 {
						k := wi*64 + bits.TrailingZeros64(word)
						word &= word - 1
						// Shared with the vector tier, so group identity and the aggregate
						// skip rules cannot depend on which tier answered.
						if !addGroupFromColumns(k, &col, aggCols, bc, acc) {
							releaseWindows(wins)
							return nil, false
						}
					}
				}
				base += blk.n
			}
		}
		releaseWindows(wins)
	}
	return acc, true
}

// resolveGroupFields maps the group column, every aggregate column and every predicated field into this
// SEGMENT's schema. ok=false when any is absent, since segments sealed under different schemas coexist.
func resolveGroupFields(cs *colSegment, groupID uint32, aggIDs []uint32,
	preds []fieldPred) (int, []int, []int, bool) {
	sch := cs.schema()
	if sch == nil {
		return 0, nil, nil, false
	}
	// Not resolveStatsFields: that one is for an AGGREGATED field and so insists on a numeric kind,
	// which a string group column is not.
	groupIdx, ok := sch.byID[groupID]
	if !ok || !groupableKind(sch.fields[groupIdx].kind) {
		return 0, nil, nil, false
	}
	predIdxs, ok := resolveFields(cs, preds)
	if !ok {
		return 0, nil, nil, false
	}
	aggIdxs := make([]int, len(aggIDs))
	for i, id := range aggIDs {
		idx, ok := sch.byID[id]
		if !ok || !numericKind(sch.fields[idx].kind) {
			return 0, nil, nil, false
		}
		aggIdxs[i] = idx
	}
	return groupIdx, aggIdxs, predIdxs, true
}

// bruteGroupStats is the row fallback for a segment whose schema lacks a field involved: walk the visible
// records and read only those attributes, never decoding the whole ad.
//
// It returns false if a matching record's group value is one only evaluation can produce -- the same
// give-up condition as the column path, for the same reason.
func bruteGroupStats(c *Collection, w segWindow, s0 uint64, groupLookup func(wire.Ad) ([]byte, bool),
	aggLookups []func(wire.Ad) ([]byte, bool), preds []fieldPred,
	lookups []func(wire.Ad) ([]byte, bool), acc *groupAccum) bool {
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
					node, found := lookups[i](ad)
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
				if match && !addGroupFromAd(ad, groupLookup, aggLookups, acc) {
					return false
				}
			}
		}
		off += int(total)
	}
	return true
}
