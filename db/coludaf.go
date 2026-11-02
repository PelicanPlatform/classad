package db

import (
	"github.com/PelicanPlatform/classad/collections"
)

// Routing a user-defined aggregate to the columnar value pass.
//
// A user-defined aggregate is served from the record scan: every matching record decoded, its
// argument evaluated into a classad.Value, one Add per row. The built-in numeric aggregates are not
// -- they read the argument as a contiguous column, with whole blocks ruled out by zone maps and
// per-block extremes, and no decode and no boxing for what survives. Measured on 50k records that
// is 8.4x ungrouped and 6.4x grouped.
//
// An aggregate that can take its values from a column closes most of that. It says so by
// implementing ColumnAccumulator, and the implementing IS the declaration -- see the two things it
// promises there, neither of which an ordinary Accumulator has to.

// NumValue is one value read from a numeric column (see collections.NumValue).
type NumValue = collections.NumValue

// NumValueKind says how a column value was stored (see collections.NumValueKind).
type NumValueKind = collections.NumValueKind

// The column value kinds, re-exported so an aggregate implementing ColumnAccumulator does not have
// to import the storage package to read the one field that decides how to fold a value.
const (
	NumInt  = collections.NumInt
	NumReal = collections.NumReal
	NumBool = collections.NumBool
)

// ColumnAccumulator is an Accumulator that can be fed straight from a numeric column instead of a
// boxed value per row.
//
// IMPLEMENTING IT IS A DECLARATION OF TWO THINGS, because the columnar pass differs from the record
// scan in two ways that an aggregate can be sensitive to, and no signature can check either:
//
//  1. ORDER. The pass walks segments, then blocks, skipping blocks a zone map rules out. That is
//     not the order a record scan visits the same records in, and not insertion order. An aggregate
//     whose answer depends on the order it is fed will give a DIFFERENT answer depending on which
//     rung served the query, which is the worst kind of difference: both answers look right. The
//     reference's own min()/max() cannot make this promise over a column holding a NaN, which is
//     why the built-in path declines that case rather than answering it.
//
//  2. WHAT IS OFFERED. The pass yields only the NUMERIC values of the column. A record whose
//     argument is absent, undefined, a string, or an expression is not offered at all -- where the
//     record scan calls Add for every row in the group, undefined included. So an aggregate that
//     counts what it was given, or that treats undefined as a contribution, must not implement this
//     interface: it would count different things on the two rungs. An aggregate that already skips
//     non-numbers -- which is what the built-in SUM, AVG, MIN and MAX all do -- loses nothing.
//
// Result must work from AddNum alone, since a query served by the columnar pass never calls Add.
type ColumnAccumulator interface {
	Accumulator

	// AddNum folds one numeric column value.
	AddNum(v NumValue)
}

// NumValueScan feeds the numeric values of attr, over the records a request matched, to one sink per
// worker. It reports false when the columnar path cannot serve the request and the caller must scan.
//
// newSink is called once per worker and merge once afterwards, both on the CALLING goroutine, and
// each sink is fed by exactly one goroutine -- so an accumulator needs no synchronization of its
// own. A pass that declines calls merge not at all.
type NumValueScan func(attr string, newSink func() func(NumValue), merge func()) bool

// ColumnarUserAggregate answers one ungrouped user-defined aggregate from the columnar value pass,
// when the registered aggregate's accumulator takes column values. ok=false means nothing was
// computed and the caller must scan.
//
// The refusals mirror ColumnarAggregate's and add one of its own. Grouping is refused because the
// pass yields values with no group key attached; a FILTER is refused rather than approximated,
// since the pass knows nothing about it and answering would report an unfiltered aggregate as a
// filtered one; and an accumulator that is not a ColumnAccumulator is refused because it has not
// made the two promises the pass requires.
func ColumnarUserAggregate(values NumValueScan, groupCols []GroupCol, aggs []AggSpec) ([]AggRow, bool, error) {
	if len(groupCols) != 0 || len(aggs) != 1 {
		return nil, false, nil
	}
	a := aggs[0]
	if a.Func != AggUser || a.Filter != "" || a.Arg == "" || a.Arg == "*" {
		return nil, false, nil
	}
	agg, ok := LookupAggregate(a.UserFunc)
	if !ok {
		// Not this rung's business to fail the query: the scan below resolves the same name and
		// reports ErrNoSuchAggregate, in one place, with the argument checks beside it.
		return nil, false, nil
	}
	// Deciding whether the aggregate opted in needs an instance, and that instance is then the
	// first worker's -- not a throwaway. NewAccumulator is the caller's code and may count its
	// calls or hold a resource, so building one only to type-assert it and drop it is a side
	// effect this rung has no business causing.
	first, ok := agg.NewAccumulator().(ColumnAccumulator)
	if !ok {
		return nil, false, nil
	}
	// One accumulator per worker, folded with the aggregate's own Merge.
	//
	// THIS IS WHAT MERGE IS FOR. The four-operation interface carried Merge from the start on the
	// argument that a parallel pass would need it, and until now nothing parallel used it -- its
	// only real caller folded two storage groups that rendered to one label. Here it is doing the
	// job it was designed for, and the laws CheckAggregate already enforces on it (associative,
	// commutative, identity) are exactly the laws a fan-out needs, because which worker gets which
	// segment is a race and the fold order is therefore not fixed.
	var accs []ColumnAccumulator
	// A panic is contained for the whole query exactly as the scan contains one, and for the same
	// reason: a state that stopped being updated part-way yields a wrong answer that looks like a
	// right one. A panic on a WORKER is re-raised on this goroutine once every worker has returned,
	// so it arrives here as an ordinary panic and this recover sees it.
	var row []AggRow
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = &udafPanic{name: a.UserFunc, val: r}
			}
		}()
		newSink := func() func(NumValue) {
			acc := first
			if acc == nil {
				// A second type assertion, and it can fail where the first did not only if the
				// aggregate returns different types from one call to the next. That is a broken
				// aggregate, and the panic it raises here is caught by the recover above and
				// reported against its name, which is the diagnosis a caller needs.
				acc = agg.NewAccumulator().(ColumnAccumulator)
			}
			first = nil
			accs = append(accs, acc)
			return acc.AddNum
		}
		merge := func() {
			for _, b := range accs[1:] {
				accs[0].Merge(b)
			}
		}
		if !values(a.Arg, newSink, merge) {
			return nil // ok stays false below
		}
		row = []AggRow{{Values: []string{ValueText(accs[0].Result())}}}
		return nil
	}()
	if err != nil {
		return nil, false, err
	}
	return row, row != nil, nil
}
