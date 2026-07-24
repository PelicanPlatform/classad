package db

import (
	"iter"
	"math"
	"strconv"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
)

// This file holds the shared server-side GROUP BY / reduce engine. Both the mutable-table
// aggregate (dbrpc's opAggregate) and the archive aggregate (ArchiveTable.Aggregate) drive
// it, so their grouping and reduction are byte-for-byte identical -- an archive COUNT/SUM/
// AVG/MIN/MAX over a history table behaves exactly like the same aggregate over a live one.
// The RPC layer aliases the public types here (AggFunc/AggSpec/AggRow/GroupCol) so its wire
// API is unchanged.

// AggFunc is a SQL aggregate function.
type AggFunc uint8

const (
	AggCount AggFunc = iota // COUNT(*) or COUNT(col)
	AggSum
	AggAvg
	AggMin
	AggMax
)

// AggSpec is one aggregate in a query: a function over an argument attribute.
// Arg "*" (only meaningful for COUNT) counts every row in the group; otherwise
// Arg is an attribute name evaluated per ad.
type AggSpec struct {
	Func AggFunc
	Arg  string
}

// AggRow is one group's result: the group-by column values followed by the
// aggregate values, all rendered as strings (aligned with the request's group
// columns and aggregate specs).
type AggRow struct {
	Group  []string
	Values []string
}

// GroupCol is one GROUP BY column for a (possibly bucketed) aggregate: the
// attribute Attr, optionally floored into fixed-width buckets. BucketWidth == 0
// groups by the raw attribute value; BucketWidth > 0 (seconds) groups by the
// epoch-aligned bucket floor(number(Attr)/BucketWidth)*BucketWidth, and a row whose
// Attr is not a finite number drops out of the result. This is the shared shape a
// bucketed aggregate and a bucketed materialized view can both group by.
type GroupCol struct {
	Attr        string
	BucketWidth int64
}

// AggProjection builds the deduplicated list of attributes the aggregation reads
// (group columns then non-"*" aggregate arguments) and the index of each group
// column / aggregate argument within that list. An aggregate whose argument is
// "*" (COUNT(*)) gets index -1. A caller projects a scan to attrs and feeds the
// resulting value rows, groupCol, and aggCol to AggregateValues.
func AggProjection(groupCols []GroupCol, aggs []AggSpec) (attrs []string, groupCol, aggCol []int) {
	idx := map[string]int{}
	intern := func(name string) int {
		if i, ok := idx[name]; ok {
			return i
		}
		i := len(attrs)
		idx[name] = i
		attrs = append(attrs, name)
		return i
	}
	groupCol = make([]int, len(groupCols))
	for i, g := range groupCols {
		groupCol[i] = intern(g.Attr)
	}
	aggCol = make([]int, len(aggs))
	for i, a := range aggs {
		if a.Arg == "*" {
			aggCol[i] = -1
			continue
		}
		aggCol[i] = intern(a.Arg)
	}
	return attrs, groupCol, aggCol
}

// AggregateValues is the shared GROUP BY core: it buckets a sequence of projected
// value rows (each aligned to the attribute list AggProjection returned) by the
// (possibly time-bucketed) group tuple and reduces each group with the COUNT/SUM/
// AVG/MIN/MAX accumulators, returning one AggRow per group in first-seen order.
// groupCol[i]/aggCol[i] index into each value row (aggCol[i] < 0 means COUNT(*)).
// With no group columns it returns a single row aggregating the whole sequence,
// still yielding one row over an empty sequence (SQL semantics: COUNT is 0, others
// undefined). If stop is non-nil it is polled per row and, when it returns true,
// the scan halts early with the groups accumulated so far -- used to abandon a
// scan whose client has gone away.
func AggregateValues(seq iter.Seq[[]classad.Value], groupCols []GroupCol, aggs []AggSpec, groupCol, aggCol []int, stop func() bool) []AggRow {
	nGroup := len(groupCols)

	// Hash-map aggregation: bucket by the joined group-value tuple, preserving
	// first-seen order for stable output. scratch builds each row's key without
	// allocating a slice per row; a group keeps its own copy only when created.
	groups := map[string]*groupState{}
	var order []string
	scratch := make([]string, nGroup)
	for vals := range seq {
		if stop != nil && stop() {
			break // client gone: stop the scan
		}
		drop := false
		for i, g := range groupCols {
			if g.BucketWidth > 0 {
				kt, ok := bucketKeyText(vals[groupCol[i]], g.BucketWidth)
				if !ok {
					drop = true // non-numeric bucket attribute: row leaves the series
					break
				}
				scratch[i] = kt
			} else {
				scratch[i] = ValueText(vals[groupCol[i]])
			}
		}
		if drop {
			continue
		}
		key := strings.Join(scratch, "\x00")
		gs := groups[key]
		if gs == nil {
			gs = &groupState{gvals: append([]string(nil), scratch...), accs: make([]aggAcc, len(aggs))}
			groups[key] = gs
			order = append(order, key)
		}
		for i, a := range aggs {
			var v classad.Value
			if aggCol[i] >= 0 {
				v = vals[aggCol[i]]
			}
			gs.accs[i].update(a, v)
		}
	}

	// A group-less aggregate over an empty match still yields one row.
	if nGroup == 0 && len(order) == 0 {
		gs := &groupState{accs: make([]aggAcc, len(aggs))}
		row := AggRow{Values: make([]string, len(aggs))}
		for i, a := range aggs {
			row.Values[i] = gs.accs[i].result(a)
		}
		return []AggRow{row}
	}

	out := make([]AggRow, 0, len(order))
	for _, key := range order {
		gs := groups[key]
		row := AggRow{Group: gs.gvals, Values: make([]string, len(aggs))}
		for i, a := range aggs {
			row.Values[i] = gs.accs[i].result(a)
		}
		out = append(out, row)
	}
	return out
}

// groupState holds one group's key values and per-aggregate accumulators.
type groupState struct {
	gvals []string
	accs  []aggAcc
}

// aggAcc accumulates one group's data for one aggregate. COUNT needs only
// counters; SUM/AVG/MIN/MAX collect the evaluated argument values and hand them
// to the ClassAd library's aggregate functions at the end, so type coercion
// (int64-exact sums, int+real promotion, boolean and undefined handling, error
// propagation, numeric-only min/max) matches HTCondor's sum()/avg()/min()/max()
// exactly rather than a re-implementation.
type aggAcc struct {
	rows int             // all rows in the group (COUNT(*))
	defN int             // rows where the argument is defined (COUNT(col))
	vals []classad.Value // argument values for SUM/AVG/MIN/MAX
}

// update folds one row's already-resolved argument value v into the accumulator.
// For COUNT(*) (spec.Arg == "*") v is ignored.
func (a *aggAcc) update(spec AggSpec, v classad.Value) {
	a.rows++
	if spec.Arg == "*" {
		return
	}
	if !v.IsUndefined() && !v.IsError() {
		a.defN++
	}
	if spec.Func != AggCount {
		a.vals = append(a.vals, v) // the library aggregates skip undefined / coerce
	}
}

func (a *aggAcc) result(spec AggSpec) string {
	switch spec.Func {
	case AggCount:
		// COUNT(*) counts every row; COUNT(col) counts rows where col is defined.
		if spec.Arg == "*" {
			return strconv.Itoa(a.rows)
		}
		return strconv.Itoa(a.defN)
	case AggSum:
		return ValueText(classad.Sum(a.vals))
	case AggAvg:
		return ValueText(classad.Avg(a.vals))
	case AggMin:
		return ValueText(classad.Min(a.vals))
	case AggMax:
		return ValueText(classad.Max(a.vals))
	}
	return "undefined"
}

// --- small value helpers (server-side, string-rendering) ---

// bucketKeyText floors a numeric value into an epoch-aligned bucket of the given
// width (seconds) and returns its integer-seconds text. ok is false when v is not a
// finite number (undefined/error/non-numeric string), so the row drops out of the
// series -- matching the client-side time_bucket semantics.
func bucketKeyText(v classad.Value, width int64) (string, bool) {
	f, ok := numberOf(v)
	if !ok {
		return "", false
	}
	b := int64(math.Floor(f/float64(width))) * width
	return strconv.FormatInt(b, 10), true
}

// numberOf returns the numeric value of v (integer or real) and whether it is one.
func numberOf(v classad.Value) (float64, bool) {
	switch {
	case v.IsInteger():
		i, _ := v.IntValue()
		return float64(i), true
	case v.IsReal():
		r, _ := v.RealValue()
		return r, true
	}
	return 0, false
}

func trimFloat(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

// ValueText renders a value as a group-key/display string.
func ValueText(v classad.Value) string {
	switch {
	case v.IsUndefined():
		return "undefined"
	case v.IsError():
		return "error"
	case v.IsBool():
		b, _ := v.BoolValue()
		return strconv.FormatBool(b)
	case v.IsString():
		s, _ := v.StringValue()
		return s
	case v.IsInteger():
		i, _ := v.IntValue()
		return strconv.FormatInt(i, 10)
	case v.IsReal():
		r, _ := v.RealValue()
		return trimFloat(r)
	default:
		return v.String()
	}
}
