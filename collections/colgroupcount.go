package collections

import (
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

// GroupStats is one group of a GROUP BY with value aggregates: the group columns' values (one per
// requested group column, in request order), how many records fell in the group, and one NumStats
// per requested aggregate attribute, in the order requested.
type GroupStats struct {
	Values []classad.Value
	Count  int
	Stats  []NumStats
	// CountOf is one record count per requested filtered COUNT(*) (see GroupStatsConstraintAggs),
	// in the order requested: how many of the group's records passed that filter. It is nil for a
	// query that asked for none, which is every query that predates per-aggregate FILTER.
	//
	// It is not derivable from Stats: a NumStats counts the records whose COLUMN held a number,
	// and a filtered COUNT(*) counts the records that passed the filter whatever they carry.
	CountOf []int
}

// GroupSpec is one GROUP BY column: the attribute, optionally floored into fixed-width buckets.
// BucketWidth == 0 groups by the raw value; BucketWidth > 0 groups by floor(v/width)*width, and a
// record whose value is not a finite number leaves the series. It mirrors db.GroupCol, which is the
// shape the aggregate engine above carries, so a caller translating between the two moves fields
// rather than reinterpreting them.
type GroupSpec struct {
	Attr        string
	BucketWidth int64
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
		// A single group column, so a group has exactly one component; a caller asking for counts
		// has no shape for more than one anyway.
		out[i] = GroupCount{Value: g.Values[0], Count: g.Count}
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
	return c.GroupStatsConstraintCols(constraint, []GroupSpec{{Attr: groupAttr, BucketWidth: bucket}}, aggAttrs)
}

// GroupStatsConstraintCols is GroupStatsConstraintBucketed over SEVERAL group columns: the group is
// the tuple of their (individually bucketed) values. See GroupStatsQueryCols.
func (c *Collection) GroupStatsConstraintCols(constraint string, groups []GroupSpec, aggAttrs []string) ([]GroupStats, bool) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, false
	}
	return c.GroupStatsQueryCols(q, groups, aggAttrs)
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
	return c.GroupStatsQueryCols(q, []GroupSpec{{Attr: groupAttr, BucketWidth: bucket}}, aggAttrs)
}

// GroupStatsQueryCols is GroupStatsQueryBucketed over SEVERAL group columns: a record's group is the
// TUPLE of its group values, each bucketed on its own width, and the whole tuple identifies one
// group. "Per owner per day" -- one raw string column beside one bucketed time column -- is the
// shape this exists for, and it is the shape that fell to a record scan when only one column was
// served.
//
// Each component keeps exactly the semantics the single-column form gives it, including the ones
// that drop a record: a bucketed component that is not a finite number has no bucket, and the
// RECORD then leaves the series, because the reference drops a row as soon as any one of its bucket
// keys fails to render. So one unbucketable component is enough to drop a record that every other
// component could have grouped.
//
// ok=false for more than MaxGroupCols columns. The key holds a fixed number of components and
// truncating past it would merge groups that differ only in a dropped component, so the query goes
// back to the scan instead.
func (c *Collection) GroupStatsQueryCols(q *vm.Query, groups []GroupSpec, aggAttrs []string) ([]GroupStats, bool) {
	return c.groupStatsQuery(q, groups, aggAttrs, nil, nil, false)
}

// GroupAggCol is one aggregate column of a grouped columnar query: the numeric attribute it reads
// and the per-aggregate FILTER deciding which records feed it ("" = every record the WHERE clause
// kept). It mirrors the (Arg, Filter) pair db.AggSpec carries, so a caller translating between the
// two moves fields rather than reinterpreting them.
//
// The SAME attribute under two different filters is two columns here, each with its own
// accumulator. Deduplicating aggregates is the caller's job and its key has to include the filter:
// MIN(x) beside MAX(x) still reads x once, but MIN(x) beside MIN(x) FILTER (...) cannot.
type GroupAggCol struct {
	Attr   string
	Filter string
}

// GroupStatsConstraintAggs is GroupStatsConstraintCols where each aggregate column may carry a
// per-aggregate FILTER, and countFilters asks additionally for a filtered RECORD COUNT per group --
// SQL's `COUNT(*) FILTER (WHERE ...)`, which reads no column at all.
//
// GroupStats.Stats stays aligned with aggCols and holds only the records that passed that column's
// filter; GroupStats.CountOf is aligned with countFilters. A filter narrows an aggregate and never
// a group: a group whose every record fails every filter is still returned, with zero counts and
// empty stats, which is what SQL specifies.
//
// ok=false when any filter is a shape the columns cannot answer (see compileGroupFilters), and the
// caller then scans. That is a decline of the WHOLE query: half a columnar result cannot be
// stitched to half a scan.
func (c *Collection) GroupStatsConstraintAggs(constraint string, groups []GroupSpec, aggCols []GroupAggCol,
	countFilters []string) ([]GroupStats, bool) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, false
	}
	attrs, filters := splitAggCols(aggCols)
	return c.groupStatsQuery(q, groups, attrs, filters, countFilters, false)
}

// GroupStatsConstraintAggsRedacted is GroupStatsConstraintAggs for a reader NOT entitled to sealed
// values: a record whose constraint the columns cannot decide is matched with no key, so a sealed
// value is undefined to it. Group and aggregate values need nothing: a sealed one is never a column
// and a cell holding one declines the whole query, which then goes back to the (keyless) scan.
//
// The match-all form, GroupStatsAllAggs, serves both readers as it is: it evaluates no constraint and
// reads nothing but columns.
func (c *Collection) GroupStatsConstraintAggsRedacted(constraint string, groups []GroupSpec, aggCols []GroupAggCol,
	countFilters []string) ([]GroupStats, bool) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, false
	}
	attrs, filters := splitAggCols(aggCols)
	return c.groupStatsQuery(q, groups, attrs, filters, countFilters, true)
}

// GroupStatsAllAggs is GroupStatsConstraintAggs over every record, for a constraint the caller has
// established is match-all. See GroupStatsAllCols.
func (c *Collection) GroupStatsAllAggs(groups []GroupSpec, aggCols []GroupAggCol,
	countFilters []string) ([]GroupStats, bool) {
	attrs, filters := splitAggCols(aggCols)
	return c.groupStatsAll(groups, attrs, filters, countFilters)
}

// splitAggCols splits aggregate columns into the parallel attribute and filter lists the internals
// carry, which keeps the attribute list the one shape every tier below already resolves.
func splitAggCols(aggCols []GroupAggCol) ([]string, []string) {
	if len(aggCols) == 0 {
		return nil, nil
	}
	attrs := make([]string, len(aggCols))
	filters := make([]string, len(aggCols))
	for i, a := range aggCols {
		attrs[i], filters[i] = a.Attr, a.Filter
	}
	return attrs, filters
}

func (c *Collection) groupStatsQuery(q *vm.Query, groups []GroupSpec, aggAttrs, colFilters,
	countFilters []string, redact bool) ([]GroupStats, bool) {
	st := c.schemaScan.Load()
	if st == nil || c.intern == nil || q == nil {
		return nil, false
	}
	groupIDs, buckets, aggIDs, ok := c.resolveGroupAttrs(st, groups, aggAttrs)
	if !ok {
		return nil, false
	}
	gf, ok := c.compileGroupFilters(st.schema, colFilters, countFilters)
	if !ok {
		return nil, false
	}
	// First tier: a conjunction of numeric comparisons against literals, narrowed column by column with
	// zone-map pruning. Fastest, and the shape a history dashboard asks most.
	if preds, pok := c.numPredsCT(q, st.schema); pok {
		if gs, served := c.groupStats(groupIDs, aggIDs, buckets, preds, gf, st); served || !predsNeedCT(preds) {
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
	acc, ok := c.vecGroupStats(q, groupIDs, aggIDs, buckets, gf, st, redact)
	if !ok {
		return nil, false
	}
	return c.shapeGroupStats(acc, groupIDs, aggIDs, nil)
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
	return c.GroupStatsAllCols([]GroupSpec{{Attr: groupAttr, BucketWidth: bucket}}, aggAttrs)
}

// GroupStatsAllCols is GroupStatsAllBucketed over SEVERAL group columns. See GroupStatsQueryCols.
func (c *Collection) GroupStatsAllCols(groups []GroupSpec, aggAttrs []string) ([]GroupStats, bool) {
	return c.groupStatsAll(groups, aggAttrs, nil, nil)
}

func (c *Collection) groupStatsAll(groups []GroupSpec, aggAttrs, colFilters,
	countFilters []string) ([]GroupStats, bool) {
	st := c.schemaScan.Load()
	if st == nil || c.intern == nil {
		return nil, false
	}
	groupIDs, buckets, aggIDs, ok := c.resolveGroupAttrs(st, groups, aggAttrs)
	if !ok {
		return nil, false
	}
	gf, ok := c.compileGroupFilters(st.schema, colFilters, countFilters)
	if !ok {
		return nil, false
	}
	return c.groupStats(groupIDs, aggIDs, buckets, nil, gf, st)
}

// resolveGroupAttrs interns the group and aggregate attribute names and checks each is carried by the
// collection-wide schema as a field this can read. A name repeated in aggAttrs resolves to the same id,
// which costs one extra read per record and keeps the result positional -- callers ask for MIN(x) and
// MAX(x).
//
// The GROUP columns may be numeric or string; an AGGREGATE column must be numeric. That asymmetry is
// the shapes themselves: MIN/SUM/AVG of a string is not something the columns compute, while grouping
// by one only needs its identity.
//
// A group column list longer than maxGroupCols declines here, before anything is read: the packed
// key has that many component slots and there is no answer that fits more. A NEGATIVE bucket width
// declines too -- the scan's own bucket test is `width > 0`, so a negative width there means "do not
// bucket", and silently agreeing with that reading rather than refusing a request that cannot mean
// what it says is how the two paths come to disagree.
func (c *Collection) resolveGroupAttrs(st *schemaScanState, groups []GroupSpec, aggAttrs []string) ([]uint32, []int64, []uint32, bool) {
	if len(groups) == 0 || len(groups) > maxGroupCols {
		return nil, nil, nil, false
	}
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
	groupIDs := make([]uint32, len(groups))
	buckets := make([]int64, len(groups))
	for i, g := range groups {
		if g.BucketWidth < 0 {
			return nil, nil, nil, false
		}
		id, ok := field(g.Attr, groupableKind)
		if !ok {
			return nil, nil, nil, false
		}
		groupIDs[i], buckets[i] = id, g.BucketWidth
	}
	var aggIDs []uint32
	for _, a := range aggAttrs {
		id, ok := field(a, numericKind)
		if !ok {
			return nil, nil, nil, false
		}
		aggIDs = append(aggIDs, id)
	}
	return groupIDs, buckets, aggIDs, true
}

// groupStats runs the scan and shapes its result: the part GroupStatsQuery and GroupStatsAll share once
// the columns and the predicate set are resolved. A nil preds means every visible record survives.
func (c *Collection) groupStats(groupIDs []uint32, aggIDs []uint32, buckets []int64, preds []fieldPred,
	gf *groupFilters, st *schemaScanState) ([]GroupStats, bool) {
	acc, ok := c.schemaScanGroupStats(groupIDs, aggIDs, buckets, preds, gf, st.cache)
	if !ok {
		return nil, false
	}
	return c.shapeGroupStats(acc, groupIDs, aggIDs, gf)
}

// shapeGroupStats turns an accumulator map into the sorted result and records the read demand. Shared by
// every tier, so the shape and order of the answer cannot depend on which one produced it.
func (c *Collection) shapeGroupStats(ga *groupAccum, groupIDs []uint32, aggIDs []uint32,
	gf *groupFilters) ([]GroupStats, bool) {
	out := make([]GroupStats, 0, ga.nGroups())
	nCount := 0
	if gf != nil {
		nCount = len(gf.countOf)
	}
	ga.each(func(vals []classad.Value, a *groupAcc) {
		g := GroupStats{Values: vals, Count: a.n}
		if len(aggIDs) > 0 {
			g.Stats = make([]NumStats, len(aggIDs))
			for i := range a.stats {
				g.Stats[i] = a.stats[i].result()
			}
		}
		if nCount > 0 {
			g.CountOf = make([]int, nCount)
			for i, f := range gf.countOf {
				g.CountOf[i] = int(a.rows[f])
			}
		}
		out = append(out, g)
	})
	sort.Slice(out, func(i, j int) bool { return groupTupleLess(out[i].Values, out[j].Values) })
	// Reading a column is demand for it, the same as any other query that touches one.
	read := make([]string, 0, len(groupIDs)+len(aggIDs))
	for _, id := range append(append([]uint32{}, groupIDs...), aggIDs...) {
		if name, ok := c.schemaFieldName(id); ok {
			read = append(read, name)
		}
	}
	c.demand.recordReads(read)
	return out, true
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

// groupTupleLess orders two groups by their component tuples, left to right -- the order a reader
// expects from `GROUP BY a, b`, and the one a two-column result needs for its order to be
// deterministic at all.
func groupTupleLess(a, b []classad.Value) bool {
	for i := range a {
		if i >= len(b) {
			return false
		}
		if groupValueLess(a[i], b[i]) {
			return true
		}
		if groupValueLess(b[i], a[i]) {
			return false
		}
	}
	return len(a) < len(b)
}

// groupValueLess orders one component by value, ints before reals only when they are equal, so ORDER
// BY on the group column gets the order it expects without the caller re-deriving it.
//
// A group set is no longer necessarily all numbers: a string column groups by text, and a record missing
// the attribute groups as `undefined`. Mixed sets are ordered by type first -- numbers, then strings,
// then booleans, then undefined and error -- because there is no meaningful order ACROSS those and an
// arbitrary-but-total one at least keeps the answer deterministic between runs.
func groupValueLess(a, b classad.Value) bool {
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

// schemaScanGroupStats buckets the group column for every record satisfying preds -- one narrowing pass
// per predicate column, then one read of the group column and of each aggregate column for the survivors.
//
// ok=false means a surviving record's GROUP value is one only evaluation can produce, which this cannot
// turn into a group (see GroupStatsQuery); the partial result is then discarded and the caller scans.
func (c *Collection) schemaScanGroupStats(groupIDs []uint32, aggIDs []uint32, buckets []int64, preds []fieldPred,
	gf *groupFilters, bc *blockCache) (*groupAccum, bool) {
	acc := newGroupAccum(buckets)
	acc.nFilt = gf.n()
	lookups := make([]fieldLookup, len(preds))
	for i, p := range preds {
		lookups[i] = c.attrLookup(p.fieldID)
	}
	groupLookups := make([]fieldLookup, len(groupIDs))
	for i, id := range groupIDs {
		groupLookups[i] = c.attrLookup(id)
	}
	aggLookups := make([]fieldLookup, len(aggIDs))
	for i, id := range aggIDs {
		aggLookups[i] = c.attrLookup(id)
	}

	var cand candSet
	var sc predScratch
	var cols groupCols
	aggCols := make([]numCol, len(aggIDs))
	ct := predsNeedCT(preds)
	zp := predZoneProbes(preds)
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			if ct && !w.ctFree {
				releaseWindows(wins) // a folded predicate is exact only in a proven window
				return nil, false
			}
			// No record here can match, so it adds no group; see schemaScanCountMulti.
			if c.segZoneExcludes(w, zp, ct) {
				continue
			}
			cs := w.seg.colblk.Load()
			groupIdxs, aggIdxs, predIdxs, ok := resolveGroupFields(cs, groupIDs, aggIDs, preds)
			// A filter whose field this segment's schema lacks sends the segment down the same
			// row fallback a missing group or predicate column does, rather than evaluating the
			// filter against a column that is not there.
			if ok {
				ok = gf.resolveSeg(cs)
			}
			if !ok {
				if !bruteGroupStats(c, w, s0, groupLookups, aggLookups, preds, lookups, gf, acc) {
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
				colOK := cols.setBlock(blk, groupIdxs, groupIDs, bc)
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
				// The per-aggregate FILTER planes, built AFTER the WHERE narrowing and only for a
				// block that still has survivors. A filter plane costs a pass over its column
				// whatever the block contributes, so building it for a block the WHERE emptied
				// would be a scan whose every bit is then never read. The planes themselves are
				// not intersected with cand: they are consulted only at a record cand already
				// kept, so the intersection would be work with no reader either.
				if cand.live > 0 && !gf.narrowBlock(blk, bc, &sc) {
					releaseWindows(wins)
					return nil, false
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
						if !addGroupFromColumns(k, &cols, aggCols, bc, acc, gf) {
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

// resolveGroupFields maps every group column, every aggregate column and every predicated field into
// this SEGMENT's schema. ok=false when any is absent, since segments sealed under different schemas
// coexist -- and ONE missing group column sends the whole segment to the row fallback, because a
// group is the tuple and a tuple with a component missing is not a group.
func resolveGroupFields(cs *colSegment, groupIDs []uint32, aggIDs []uint32,
	preds []fieldPred) ([]int, []int, []int, bool) {
	sch := cs.schema()
	if sch == nil {
		return nil, nil, nil, false
	}
	// Not resolveStatsFields: that one is for an AGGREGATED field and so insists on a numeric kind,
	// which a string group column is not.
	groupIdxs := make([]int, len(groupIDs))
	for i, id := range groupIDs {
		idx, ok := sch.byID[id]
		if !ok || !groupableKind(sch.fields[idx].kind) {
			return nil, nil, nil, false
		}
		groupIdxs[i] = idx
	}
	predIdxs, ok := resolveFields(cs, preds)
	if !ok {
		return nil, nil, nil, false
	}
	aggIdxs := make([]int, len(aggIDs))
	for i, id := range aggIDs {
		idx, ok := sch.byID[id]
		if !ok || !numericKind(sch.fields[idx].kind) {
			return nil, nil, nil, false
		}
		aggIdxs[i] = idx
	}
	return groupIdxs, aggIdxs, predIdxs, true
}

// bruteGroupStats is the row fallback for a segment whose schema lacks a field involved: walk the visible
// records and read only those attributes, never decoding the whole ad.
//
// It returns false if a matching record's group value is one only evaluation can produce -- the same
// give-up condition as the column path, for the same reason.
func bruteGroupStats(c *Collection, w segWindow, s0 uint64, groupLookups []fieldLookup,
	aggLookups []fieldLookup, preds []fieldPred,
	lookups []fieldLookup, gf *groupFilters, acc *groupAccum) bool {
	var buf []byte
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
				if match && !addGroupFromAd(w.dict(), ad, groupLookups, aggLookups, acc, &comps, gf) {
					return false
				}
			}
		}
		off += int(total)
	}
	return true
}
