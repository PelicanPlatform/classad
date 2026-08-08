package db

import (
	"strconv"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// Routing a single numeric aggregate to the columnar accelerator.
//
// The columnar block stores each hot numeric column contiguously and its scan yields values, so
// MIN/MAX/SUM/AVG/COUNT(attr) are one pass over a column rather than a decode of every record.
// Only COUNT(*) was wired up before, which is why an unconstrained MAX over a large table scanned
// row-wise.

// NumStats is one columnar pass's numeric aggregate inputs (see collections.NumStats).
type NumStats = collections.NumStats

// NumStats computes the aggregate inputs for attr over the records matching constraint, via the
// columnar scan. ok=false means the columnar path cannot serve it and the caller should scan.
func (db *DB) NumStats(constraint, attr string) (NumStats, bool) {
	q, ok := numStatsQuery(constraint)
	if !ok {
		return NumStats{}, false
	}
	return db.c.NumStatsQuery(q, attr)
}

// NumStats is DB.NumStats for an archive (history) table.
func (t *ArchiveTable) NumStats(constraint, attr string) (NumStats, bool) {
	q, ok := numStatsQuery(constraint)
	if !ok {
		return NumStats{}, false
	}
	return t.a.NumStatsQuery(q, attr)
}

// numStatsQuery turns a constraint into the query the columnar aggregate takes: nil for match-all
// (no predicate at all), otherwise the parsed query. ok=false for a constraint that does not
// parse, which the caller reports through the ordinary scan path rather than here.
func numStatsQuery(constraint string) (*vm.Query, bool) {
	if IsMatchAll(constraint) {
		return nil, true
	}
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, false
	}
	return q, true
}

// ColumnarAggregate answers aggs over constraint from the columnar accelerator when it can:
// exactly one aggregate, no grouping, no per-aggregate FILTER, and a numeric argument the current
// schema carries. ok=false means nothing was computed and the caller must scan.
//
// COUNT(attr), MIN, MAX, SUM and AVG are served. The result TYPE follows the reference exactly
// (see numAggValue): SUM accumulates integers in int64 and only becomes a real once a real value
// appears, AVG is always a real, and MIN/MAX keep their element's type. A formatting difference
// would be as wrong as a numeric one and far easier to ship unnoticed, so the test compares text.
//
// Two refusals. A FILTER is refused rather than approximated: the columnar pass knows nothing about
// it, so answering would report an unfiltered aggregate as if it were filtered. And a column that
// turned up a BOOLEAN declines: the reference coerces booleans to 1/0 and then has a further quirk
// for a lone boolean element, which is not worth reproducing for data this pathological -- the scan
// gives the exact answer.
func ColumnarAggregate(stats func(attr string) (NumStats, bool), groupCols []GroupCol, aggs []AggSpec) ([]AggRow, bool) {
	if len(groupCols) != 0 || len(aggs) != 1 {
		return nil, false
	}
	a := aggs[0]
	if a.Filter != "" || a.Arg == "" || a.Arg == "*" {
		return nil, false
	}
	switch a.Func {
	case AggCount, AggMin, AggMax, AggSum, AggAvg:
	default:
		return nil, false // COUNT(DISTINCT) needs the values themselves, not their aggregate
	}
	ns, ok := stats(a.Arg)
	if !ok {
		return nil, false
	}
	if ns.AnyBool {
		return nil, false
	}
	return []AggRow{{Values: []string{numAggValue(a.Func, ns)}}}, true
}

// numAggValue renders one aggregate from a columnar pass through the same value rendering the
// scanning aggregator uses, reproducing the reference's result types:
//
//	SUM   int64 accumulation, promoted to a real only if a real value appeared (builtinSum);
//	      an empty SUM is int 0, not undefined.
//	AVG   always a real (builtinAvg); an empty AVG is int 0.
//	MIN   the element's own type, so an integer column prints an integer.
//	MAX   likewise.
//
// The int64 accumulation matters past 2^53, where rendering from the float sum would disagree
// with the scan on large values.
func numAggValue(fn AggFunc, ns NumStats) string {
	switch fn {
	case AggCount:
		return strconv.Itoa(ns.N)
	case AggSum:
		if ns.N == 0 {
			return ValueText(classad.NewIntValue(0)) // reference: sum of nothing is int 0
		}
		if ns.AnyReal {
			return ValueText(classad.NewRealValue(ns.Sum))
		}
		return ValueText(classad.NewIntValue(ns.IntSum))
	case AggAvg:
		if ns.N == 0 {
			return ValueText(classad.NewIntValue(0)) // reference: avg of nothing is int 0
		}
		return ValueText(classad.NewRealValue(ns.Sum / float64(ns.N)))
	}
	if ns.N == 0 {
		return ValueText(classad.NewUndefinedValue())
	}
	v := ns.Min
	if fn == AggMax {
		v = ns.Max
	}
	if ns.AnyReal {
		return ValueText(classad.NewRealValue(v))
	}
	return ValueText(classad.NewIntValue(int64(v)))
}
