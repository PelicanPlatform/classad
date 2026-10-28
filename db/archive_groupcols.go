package db

import (
	"strconv"
	"strings"

	"github.com/PelicanPlatform/classad/collections"
)

// A GROUP BY on one or more columns served from the columnar blocks.
//
// aggregateFromIndex answers a grouped COUNT(*) only from the CATEGORICAL index, the constrained-count
// fast path is gated on there being no grouping, and ColumnarAggregate declines any grouping at all. So
// `GROUP BY <numeric>` fell to a record scan however it was written -- the grouped form of a query whose
// ungrouped form took 38 ms took 8.1 s.
//
// The storage layer computes the whole group set in one narrowed pass (collections.GroupStatsQuery). What
// is left here is turning it into rows the way the SCANNING aggregator would have, which is where the two
// can disagree:
//
//   - Group identity. Storage keys a group by each component's value and kind, so an integer 3 and a
//     real 3.0 are distinct keys; the scan path keys by rendered text, where both are "3". Merging on
//     the text reproduces the scan's partition instead of emitting one group as two rows sharing a
//     label. A string group column makes that merge load-bearing rather than a corner case: a record
//     whose attribute is absent groups as UNDEFINED and renders "undefined", which is also what a
//     record holding the literal string "undefined" renders, and the scan puts both in one group.
//
//     That merge is also what makes SEVERAL group columns safe. The storage key is a REFINEMENT of
//     the scan's rendered identity -- equal keys always render alike, never the reverse -- so every
//     way the two could part company shows up here as two keys whose rendered tuples are equal, and
//     this joins the components with the same "\x00" the scan's own AggregateValues joins them with
//     and folds them. A key that could COARSEN the scan's partition would have no such repair, which
//     is why the storage side identifies a string by its exact bytes and never by a folded form.
//   - Value rendering. Each aggregate renders through numAggValue, the same function the ungrouped
//     columnar aggregate uses, so SUM's int64 accumulation, AVG's always-real, MIN/MAX keeping their
//     element's type, and the empty cases all match the reference by construction rather than by a second
//     implementation that could drift.
//
// One thing deliberately NOT reproduced: the exact float rounding of a real-valued SUM or AVG. Float
// addition is not associative, this pass adds a group's values in column order, and the reference adds
// them in scan order, so the two can differ in the last ULP (13084.50000000002 against
// 13084.500000000018). No ordering guarantee is made for an aggregate, so that is a permitted difference
// rather than a wrong answer, and these are served. Integer sums are exact in int64 either way.

// GroupStatsSource is a table that can answer a grouped columnar aggregate: an archive or a mutable
// table. Both back onto the same collections primitive, so both get the same fast path and the same
// row-shaping rather than a copy per table type that could drift.
type GroupStatsSource interface {
	// GroupStatsConstraintAggs answers the grouped stats for rows matching constraint, grouping by
	// the tuple of the given columns, each optionally floored into fixed-width buckets. An
	// aggregate column may carry a per-aggregate FILTER, and countFilters asks for a filtered
	// record count per group.
	GroupStatsConstraintAggs(constraint string, groups []collections.GroupSpec,
		aggCols []collections.GroupAggCol, countFilters []string) ([]collections.GroupStats, bool)
	// GroupStatsAllAggs answers them over every row, for a constraint the caller has established
	// is match-all (the predicate analysis has no predicate to work with there).
	GroupStatsAllAggs(groups []collections.GroupSpec, aggCols []collections.GroupAggCol,
		countFilters []string) ([]collections.GroupStats, bool)
}

// groupedFromColumns answers a GROUP BY from the archive's columns.
func (t *ArchiveTable) groupedFromColumns(constraint string, groupCols []GroupCol, aggs []AggSpec) ([]AggRow, bool) {
	return GroupedFromColumns(t, constraint, groupCols, aggs)
}

// GroupedFromColumns answers a GROUP BY from src's columns, or ok=false to scan.
//
// Served: up to collections.MaxGroupCols group columns -- each numeric or STRING, each raw or floored
// into fixed-width buckets -- and aggregates drawn from COUNT(*), COUNT(attr), MIN, MAX, SUM and AVG
// over NUMERIC attributes the schema carries. A string group value is read through the block's string
// dictionary; an aggregate over a string column is still not this shape.
//
// The bucketed form is what a time series asks ("per day", not "per second"), and it is the form this
// used to decline outright, sending every `GROUP BY time_bucket(t,'1d')` to a record scan while its
// ungrouped sibling was answered from the columns. Measured against a production history archive, the
// two differed by more than two orders of magnitude.
//
// MORE THAN ONE column is the "per owner per day" shape -- one raw categorical column beside one
// bucketed time column -- which a dashboard asks constantly and which this declined outright for the
// same reason the bucketed form once did. The index tier answers the COUNT(*) form of it over a
// categorically indexed column with no WHERE clause, and nothing else: add an aggregate, a
// constraint, or a group column the index does not carry, and it fell to the record scan.
//
// A per-aggregate FILTER is served: `COUNT(*) FILTER (WHERE JobStatus == 2)` beside a plain
// COUNT(*) answers two differently-conditioned questions in one pass, where it used to decline the
// whole query and send a shape the columns nearly answered back to the record scan. The filter is
// lowered to the same column narrowing the WHERE clause is lowered to, so it is served for exactly
// the expressions the WHERE clause is served for -- a conjunction of scalar comparisons against
// literals over numeric schema fields -- and a filter outside that declines the query.
//
// COUNT(DISTINCT) declines: it needs the values, not their aggregate. More than MaxGroupCols
// columns declines: the storage key has that many component slots and a truncated key would merge
// groups the scan keeps apart. An UNGROUPED filtered aggregate declines too, because this whole
// path is the grouped one -- `SELECT COUNT(*) FILTER (WHERE ...)` with no GROUP BY still scans,
// and the ungrouped columnar rung above refuses a filter for the same reason the index rung does.
func GroupedFromColumns(src GroupStatsSource, constraint string, groupCols []GroupCol, aggs []AggSpec) ([]AggRow, bool) {
	if len(groupCols) == 0 || len(groupCols) > collections.MaxGroupCols || len(aggs) == 0 {
		return nil, false
	}
	specs := make([]collections.GroupSpec, len(groupCols))
	for i, g := range groupCols {
		if g.BucketWidth < 0 {
			return nil, false
		}
		specs[i] = collections.GroupSpec{Attr: g.Attr, BucketWidth: g.BucketWidth}
	}
	// Which column each aggregate reads, and where its stats land. Columns are de-duplicated so
	// MIN(x) alongside MAX(x) reads x once -- and the key is (attribute, FILTER), because two
	// aggregates share a column only when they see the same records. MIN(x) beside
	// MIN(x) FILTER (WHERE ...) is two columns with two accumulators.
	var cols []collections.GroupAggCol
	// The filters of aggregates that read no column at all: `COUNT(*) FILTER (WHERE ...)`, whose
	// answer is a record count and not a column's statistics. De-duplicated by filter text, so
	// several counts under one condition narrow one plane.
	var countFilters []string
	slot := make([]int, len(aggs))  // index into cols, or -1
	cslot := make([]int, len(aggs)) // index into countFilters, or -1
	at := map[string]int{}
	cat := map[string]int{}
	for i, a := range aggs {
		slot[i], cslot[i] = -1, -1
		switch a.Func {
		case AggCount:
			if a.Arg == "*" {
				if a.Filter == "" {
					continue // the group's own record count
				}
				j, ok := cat[a.Filter]
				if !ok {
					j = len(countFilters)
					cat[a.Filter] = j
					countFilters = append(countFilters, a.Filter)
				}
				cslot[i] = j
				continue
			}
		case AggMin, AggMax, AggSum, AggAvg:
		default:
			return nil, false
		}
		if a.Arg == "" || a.Arg == "*" {
			return nil, false
		}
		key := a.Arg + "\x00" + a.Filter
		j, ok := at[key]
		if !ok {
			j = len(cols)
			at[key] = j
			cols = append(cols, collections.GroupAggCol{Attr: a.Arg, Filter: a.Filter})
		}
		slot[i] = j
	}

	// Match-all first, using the knowledge only this layer has. The storage layer would also serve
	// `WHERE true` -- its second tier evaluates the query against the columns, and a literal true matches
	// every live record -- but that pays to evaluate an expression per block to learn what this layer
	// already knows, and it cannot serve an EMPTY constraint at all (the parser rejects it).
	var groups []collections.GroupStats
	var ok bool
	if IsMatchAll(constraint) {
		groups, ok = src.GroupStatsAllAggs(specs, cols, countFilters)
	} else {
		groups, ok = src.GroupStatsConstraintAggs(constraint, specs, cols, countFilters)
	}
	if !ok {
		return nil, false
	}
	// A source that answered without filling the filtered counts would have ignored the filters,
	// and reading past the slice it did return would panic rather than say so. GroupStatsSource is
	// exported, so this is the check that turns an implementation that predates filtered counts
	// into a decline.
	for i := range groups {
		if len(groups[i].CountOf) != len(countFilters) {
			return nil, false
		}
	}

	rows := make([]AggRow, 0, len(groups))
	rowAt := make(map[string]int, len(groups))
	merged := make([]*collections.GroupStats, 0, len(groups))
	labels := make([][]string, 0, len(groups))
	scratch := make([]string, len(groupCols))
	for i := range groups {
		g := &groups[i]
		// A boolean in an aggregated column declines, exactly as the ungrouped path does: the reference
		// coerces booleans to 1/0 and has a further quirk for a lone boolean element, and the scan gives
		// the exact answer for data that pathological.
		for _, ns := range g.Stats {
			if ns.AnyBool {
				return nil, false
			}
		}
		// A NaN in a column some MIN/MAX reads declines the same way. The reference's min()/max()
		// seeds from the first contributing element and NaN loses every comparison, so its answer
		// depends on the order records are visited in -- an order this pass does not share (see
		// minMaxDeclinesNaN). The check is per aggregate, not per column, so a SUM or AVG over a
		// NaN-bearing column is still served; and one tripped group declines the whole query,
		// because half a columnar result cannot be stitched to half a scan.
		for i, a := range aggs {
			if slot[i] >= 0 && minMaxDeclinesNaN(a.Func, g.Stats[slot[i]]) {
				return nil, false
			}
		}
		// The rendered tuple, joined exactly as AggregateValues joins it. Rendering goes through
		// ValueText for every component, bucketed or not: storage has already floored a bucketed
		// component to an integer bucket start, and bucketKeyText renders that start with the same
		// FormatInt, so one renderer covers both and there is no second spelling to drift.
		if len(g.Values) != len(groupCols) {
			return nil, false // a component short: nothing here can invent the missing one
		}
		for j := range g.Values {
			scratch[j] = ValueText(g.Values[j])
		}
		text := strings.Join(scratch, "\x00")
		if j, dup := rowAt[text]; dup {
			mergeGroupStats(merged[j], g)
			continue
		}
		rowAt[text] = len(merged)
		merged = append(merged, g)
		labels = append(labels, append([]string(nil), scratch...))
	}
	for n, g := range merged {
		values := make([]string, len(aggs))
		for i, a := range aggs {
			switch {
			case cslot[i] >= 0:
				// COUNT(*) FILTER: the records of the group that passed the filter. A group
				// none of them passed still appears, with zero -- a filter narrows the
				// aggregate and never the group.
				values[i] = strconv.Itoa(g.CountOf[cslot[i]])
			case slot[i] < 0:
				values[i] = strconv.Itoa(g.Count)
			default:
				values[i] = numAggValue(a.Func, g.Stats[slot[i]])
			}
		}
		rows = append(rows, AggRow{Group: labels[n], Values: values})
	}
	return rows, true
}

// mergeGroupStats folds src into dst, for two storage keys that render as the same group. Combining the
// stats rather than re-reading is exact for every aggregate served here: counts and sums add, min/max
// take the extreme, and the type-promotion flags OR together the same way a single pass over both sets of
// records would have set them.
func mergeGroupStats(dst, src *collections.GroupStats) {
	dst.Count += src.Count
	// Two storage keys rendering as one group merge their filtered record counts the same way
	// they merge the unfiltered one: a record passed a filter or it did not, whichever key it
	// was filed under.
	for i := range dst.CountOf {
		if i < len(src.CountOf) {
			dst.CountOf[i] += src.CountOf[i]
		}
	}
	for i := range dst.Stats {
		d, s := &dst.Stats[i], src.Stats[i]
		if s.N == 0 {
			continue
		}
		if d.N == 0 {
			*d = s
			continue
		}
		// IntMin/IntMax are read only when no real contributed to either side, so both hold
		// values whenever the result is rendered from them.
		if !d.AnyReal && !s.AnyReal {
			d.IntMin = min(d.IntMin, s.IntMin)
			d.IntMax = max(d.IntMax, s.IntMax)
		}
		d.N += s.N
		d.Sum += s.Sum
		d.IntSum += s.IntSum
		d.AnyReal = d.AnyReal || s.AnyReal
		d.AnyBool = d.AnyBool || s.AnyBool
		d.AnyNaN = d.AnyNaN || s.AnyNaN
		if s.Min < d.Min {
			d.Min = s.Min
		}
		if s.Max > d.Max {
			d.Max = s.Max
		}
	}
}
