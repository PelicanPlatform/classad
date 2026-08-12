package db

import (
	"strconv"

	"github.com/PelicanPlatform/classad/collections"
)

// A GROUP BY on one numeric column served from the columnar blocks.
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
//   - Group identity. Storage keys a group by (bits, kind), so an integer 3 and a real 3.0 are distinct
//     keys; the scan path keys by rendered text, where both are "3". Merging on the text reproduces the
//     scan's partition instead of emitting one group as two rows sharing a label.
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

// groupedFromColumns answers a single-numeric-column GROUP BY from the columns, or ok=false to scan.
//
// Served: one group column with no bucket width, and aggregates drawn from COUNT(*), COUNT(attr), MIN,
// MAX, SUM and AVG over numeric attributes the schema carries. A per-aggregate FILTER declines -- the
// columnar pass knows nothing about it, so answering would report an unfiltered aggregate as a filtered
// one. COUNT(DISTINCT) declines: it needs the values, not their aggregate.
func (t *ArchiveTable) groupedFromColumns(constraint string, groupCols []GroupCol, aggs []AggSpec) ([]AggRow, bool) {
	if len(groupCols) != 1 || groupCols[0].BucketWidth != 0 || len(aggs) == 0 {
		return nil, false
	}
	// Which attribute each aggregate reads, and where its stats land. Attributes are de-duplicated so
	// MIN(x) alongside MAX(x) reads the column once.
	var attrs []string
	slot := make([]int, len(aggs)) // -1 for COUNT(*), which needs no column
	at := map[string]int{}
	for i, a := range aggs {
		if a.Filter != "" {
			return nil, false
		}
		switch a.Func {
		case AggCount:
			if a.Arg == "*" {
				slot[i] = -1
				continue
			}
		case AggMin, AggMax, AggSum, AggAvg:
		default:
			return nil, false
		}
		if a.Arg == "" || a.Arg == "*" {
			return nil, false
		}
		j, ok := at[a.Arg]
		if !ok {
			j = len(attrs)
			at[a.Arg] = j
			attrs = append(attrs, a.Arg)
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
		groups, ok = t.a.GroupStatsAll(groupCols[0].Attr, attrs)
	} else {
		groups, ok = t.a.GroupStatsConstraint(constraint, groupCols[0].Attr, attrs)
	}
	if !ok {
		return nil, false
	}

	rows := make([]AggRow, 0, len(groups))
	rowAt := make(map[string]int, len(groups))
	merged := make([]*collections.GroupStats, 0, len(groups))
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
		text := ValueText(g.Value)
		if j, dup := rowAt[text]; dup {
			mergeGroupStats(merged[j], g)
			continue
		}
		rowAt[text] = len(merged)
		merged = append(merged, g)
	}
	for _, g := range merged {
		values := make([]string, len(aggs))
		for i, a := range aggs {
			if slot[i] < 0 {
				values[i] = strconv.Itoa(g.Count)
				continue
			}
			values[i] = numAggValue(a.Func, g.Stats[slot[i]])
		}
		rows = append(rows, AggRow{Group: []string{ValueText(g.Value)}, Values: values})
	}
	return rows, true
}

// mergeGroupStats folds src into dst, for two storage keys that render as the same group. Combining the
// stats rather than re-reading is exact for every aggregate served here: counts and sums add, min/max
// take the extreme, and the type-promotion flags OR together the same way a single pass over both sets of
// records would have set them.
func mergeGroupStats(dst, src *collections.GroupStats) {
	dst.Count += src.Count
	for i := range dst.Stats {
		d, s := &dst.Stats[i], src.Stats[i]
		if s.N == 0 {
			continue
		}
		if d.N == 0 {
			*d = s
			continue
		}
		d.N += s.N
		d.Sum += s.Sum
		d.IntSum += s.IntSum
		d.AnyReal = d.AnyReal || s.AnyReal
		d.AnyBool = d.AnyBool || s.AnyBool
		if s.Min < d.Min {
			d.Min = s.Min
		}
		if s.Max > d.Max {
			d.Max = s.Max
		}
	}
}
