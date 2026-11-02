package db

import (
	"strconv"
	"strings"

	"github.com/PelicanPlatform/classad/collections"
)

// A GROUP BY reduced by user-defined aggregates, from the columns.
//
// The ungrouped rung (coludaf.go) brought a single user-defined aggregate to parity with the
// built-in SUM. A GROUP BY did not move, and it is the shape that matters: a dashboard asks "per
// owner", "per day", "per status", almost never "over everything". This rung serves it from the
// same grouping machinery the built-in aggregates use -- the same key packing, the same bucketing,
// the same zone pruning, the same per-aggregate FILTER planes -- with each group carrying the
// caller's accumulators instead of the fixed numeric statistics.

// GroupValuesSource is the storage-side grouped pass reduced into caller-supplied accumulators. It
// mirrors GroupStatsSource, which is the same pass reduced into NumStats.
type GroupValuesSource interface {
	// GroupValuesConstraintAggs answers the groups for rows matching constraint, giving each
	// group a sink from newSink the first time it is seen.
	GroupValuesConstraintAggs(constraint string, groups []collections.GroupSpec,
		aggCols []collections.GroupAggCol, countFilters []string,
		newSink func() collections.GroupSink,
		mergeSink collections.MergeGroupSink) ([]collections.GroupValues, bool)
	// GroupValuesAllAggs answers them over every row, for a constraint the caller has established
	// is match-all.
	GroupValuesAllAggs(groups []collections.GroupSpec, aggCols []collections.GroupAggCol,
		countFilters []string, newSink func() collections.GroupSink,
		mergeSink collections.MergeGroupSink) ([]collections.GroupValues, bool)
}

// udafGroupSink is one group's accumulators, in the order the aggregate columns were requested.
type udafGroupSink struct{ accs []ColumnAccumulator }

func (s *udafGroupSink) AddNum(agg int, v collections.NumValue) { s.accs[agg].AddNum(v) }

// mergeUDAFGroupSinks folds one group's accumulators into another's, column by column, with each
// aggregate's own Merge.
//
// The storage layer cannot do this: it holds a GroupSink, which only accepts values. It is needed
// because a fanned-out grouped pass gives every worker its own sink per group, so a group two
// workers both saw arrives as two sets of accumulators to fold into one -- the same reason Merge
// exists at all, now reached by a second caller.
//
// Both sides come from this package's own newSink, so the assertion cannot fail for any input; it
// would fail only if the storage layer handed back a sink it did not get from us, which is a bug
// here rather than bad data, and the panic is contained for the query like every other.
func mergeUDAFGroupSinks(dst, src collections.GroupSink) {
	d, s := dst.(*udafGroupSink), src.(*udafGroupSink)
	for i := range d.accs {
		d.accs[i].Merge(s.accs[i])
	}
}

// GroupedUserFromColumns answers a GROUP BY whose value aggregates are all user-defined, from src's
// columns. ok=false means nothing was computed and the caller must scan.
//
// WHAT IT SERVES, and the one asymmetry with the built-in grouped rung. Every value aggregate must
// be an AggUser whose accumulator takes column values; COUNT(*) rides along, filtered or not,
// because a group's record count and its filtered counts are kept beside the accumulators rather
// than derived from them. A built-in SUM or MIN in the same request is refused: a group holds
// either the fixed statistics or the caller's accumulators, never both, so serving the mixture
// would mean two passes, and two passes over one request is a different plan rather than this one.
//
// COLUMNS ARE NOT DE-DUPLICATED HERE, which is the opposite of what the built-in rung does. There,
// MIN(x) beside MAX(x) reads x once and shares one statistics accumulator, because the statistics
// are the same statistics. Two user-defined aggregates over one column are two different
// computations and need two accumulators, so each gets its own column entry and the column is read
// once per aggregate. Sharing them would hand both the same accumulator and silently answer one
// aggregate twice.
func GroupedUserFromColumns(src GroupValuesSource, constraint string, groupCols []GroupCol,
	aggs []AggSpec) ([]AggRow, bool, error) {
	if len(groupCols) == 0 || len(groupCols) > collections.MaxGroupCols || len(aggs) == 0 {
		return nil, false, nil
	}
	specs := make([]collections.GroupSpec, len(groupCols))
	for i, g := range groupCols {
		if g.BucketWidth < 0 {
			return nil, false, nil
		}
		specs[i] = collections.GroupSpec{Attr: g.Attr, BucketWidth: g.BucketWidth}
	}

	var cols []collections.GroupAggCol
	var userAggs []Aggregate
	var countFilters []string
	slot := make([]int, len(aggs))  // index into cols, or -1
	cslot := make([]int, len(aggs)) // index into countFilters, or -1
	cat := map[string]int{}
	anyUser := false
	for i, a := range aggs {
		slot[i], cslot[i] = -1, -1
		if a.Func == AggCount && a.Arg == "*" {
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
		if a.Func != AggUser || a.Arg == "" || a.Arg == "*" {
			return nil, false, nil
		}
		agg, ok := LookupAggregate(a.UserFunc)
		if !ok {
			// The scan resolves the same name and reports ErrNoSuchAggregate, in one place.
			return nil, false, nil
		}
		if _, ok := agg.NewAccumulator().(ColumnAccumulator); !ok {
			return nil, false, nil // did not make the promises the column pass requires
		}
		slot[i] = len(cols)
		cols = append(cols, collections.GroupAggCol{Attr: a.Arg, Filter: a.Filter})
		userAggs = append(userAggs, agg)
		anyUser = true
	}
	if !anyUser {
		return nil, false, nil // a grouped COUNT(*) is the built-in rung's, not this one's
	}

	newSink := func() collections.GroupSink {
		s := &udafGroupSink{accs: make([]ColumnAccumulator, len(userAggs))}
		for i, agg := range userAggs {
			s.accs[i] = agg.NewAccumulator().(ColumnAccumulator)
		}
		return s
	}

	// Contained for the whole query, as everywhere else a user-defined aggregate runs: a group
	// whose accumulation stopped part-way gives a wrong answer that looks like a right one. The
	// scan below resolves the same aggregates and will report a panic the same way.
	var rows []AggRow
	var served bool
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				rows, served, err = nil, false, &udafPanic{name: userAggName(aggs), val: r}
			}
		}()
		var groups []collections.GroupValues
		var ok bool
		if IsMatchAll(constraint) {
			groups, ok = src.GroupValuesAllAggs(specs, cols, countFilters, newSink, mergeUDAFGroupSinks)
		} else {
			groups, ok = src.GroupValuesConstraintAggs(constraint, specs, cols, countFilters, newSink, mergeUDAFGroupSinks)
		}
		if !ok {
			return nil
		}
		// A source that answered without filling the filtered counts ignored the filters, and
		// reading past what it returned would panic rather than say so. The interface is exported,
		// so this turns an implementation that predates them into a decline.
		for i := range groups {
			if len(groups[i].CountOf) != len(countFilters) {
				return nil
			}
		}
		// Two storage groups can render to the SAME tuple -- an integer 1 and a real 1.0 are
		// distinct group values and one label -- so the built-in rung merges them before
		// rendering, and this must merge the same way or it would emit a duplicate row the
		// reference does not. Merging two groups' accumulators is Merge, which makes this the
		// first thing in the engine that calls it: until now the reduction was serial and every
		// accumulator was folded by Add alone. The law Merge is held to -- associative and
		// commutative -- is exactly what makes merging in storage-group order sound here.
		rowAt := make(map[string]int, len(groups))
		merged := make([]*collections.GroupValues, 0, len(groups))
		labels := make([][]string, 0, len(groups))
		scratch := make([]string, len(groupCols))
		for i := range groups {
			g := &groups[i]
			if len(g.Values) != len(groupCols) {
				return nil // a component short: nothing here can invent the missing one
			}
			if _, isSink := g.Sink.(*udafGroupSink); !isSink {
				return nil // not a sink this made; answering from it would be a guess
			}
			for j := range g.Values {
				scratch[j] = ValueText(g.Values[j])
			}
			text := strings.Join(scratch, "\x00")
			if j, dup := rowAt[text]; dup {
				mergeGroupValues(merged[j], g)
				continue
			}
			rowAt[text] = len(merged)
			merged = append(merged, g)
			labels = append(labels, append([]string(nil), scratch...))
		}
		out := make([]AggRow, 0, len(merged))
		for n, g := range merged {
			sink := g.Sink.(*udafGroupSink)
			values := make([]string, len(aggs))
			for i := range aggs {
				switch {
				case cslot[i] >= 0:
					// COUNT(*) FILTER: the records of the group that passed the filter. A
					// group none of them passed still appears, with zero.
					values[i] = strconv.Itoa(g.CountOf[cslot[i]])
				case slot[i] < 0:
					values[i] = strconv.Itoa(g.Count)
				default:
					values[i] = ValueText(sink.accs[slot[i]].Result())
				}
			}
			out = append(out, AggRow{Group: labels[n], Values: values})
		}
		rows, served = out, true
		return nil
	}()
	if err != nil {
		return nil, false, err
	}
	return rows, served, nil
}

// mergeGroupValues folds b's accumulators into a's, for two storage groups that render to one
// label. It mirrors mergeGroupStats, which does the same for the built-in statistics.
func mergeGroupValues(a, b *collections.GroupValues) {
	a.Count += b.Count
	for i := range a.CountOf {
		if i < len(b.CountOf) {
			a.CountOf[i] += b.CountOf[i]
		}
	}
	as, bs := a.Sink.(*udafGroupSink), b.Sink.(*udafGroupSink)
	for i := range as.accs {
		as.accs[i].Merge(bs.accs[i])
	}
}
