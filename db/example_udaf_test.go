package db_test

import (
	"fmt"
	"math"
	"sort"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// stdDev is a user-defined aggregate: the population standard deviation of an attribute.
//
// It is the shape a user-defined aggregate is FOR. The built-in SUM and AVG retain every value in
// the group so that the final answer can be computed by the reference implementation, which makes
// their state O(group size); this one keeps three floats whatever the group holds.
type stdDev struct{}

// NewAccumulator returns the identity -- the answer for a group that contributed no rows.
func (stdDev) NewAccumulator() db.Accumulator { return &stdDevAcc{} }

// stdDevAcc accumulates by Welford's method, which is numerically stable and, more to the point
// here, has a MERGE: Chan's formula combines two partial states without either one seeing the
// other's values. An aggregate whose state cannot be merged cannot be fanned out.
type stdDevAcc struct {
	n    float64
	mean float64
	m2   float64 // sum of squared deviations from the running mean
}

// Add folds one row's value, from the record scan. It is handed EVERY row in the group, including
// rows whose attribute is absent, a string, or an expression that evaluated to an error -- so
// skipping non-numbers is this aggregate's own decision, and it is the same one SUM makes.
func (a *stdDevAcc) Add(v classad.Value) {
	x, err := v.NumberValue()
	if err != nil {
		return
	}
	a.addNumber(x)
}

// AddNum folds one value read straight from a numeric column. Declaring it (see ColumnAccumulator)
// is a promise about TWO things no signature can check, and both have to be true of this aggregate
// before the method may be written:
//
//   - The answer must not depend on the order values arrive in. The column pass walks segments and
//     blocks and skips whole blocks a zone map rules out, so the order is not the scan's and not
//     insertion order. Welford is order-dependent in its last bits -- see the tolerance in
//     ExampleCheckAggregateEqual below -- but not in its answer.
//   - It must not count what it was never offered. The pass yields only the column's NUMERIC
//     values, so a row whose attribute is absent or a string is not offered at all. This aggregate
//     drops those in Add too, so the two rungs count the same rows.
func (a *stdDevAcc) AddNum(v db.NumValue) {
	switch v.Kind {
	case db.NumInt, db.NumBool:
		a.addNumber(float64(v.I))
	default:
		a.addNumber(v.F)
	}
}

func (a *stdDevAcc) addNumber(x float64) {
	a.n++
	d := x - a.mean
	a.mean += d / a.n
	a.m2 += d * (x - a.mean)
}

// Merge folds another accumulator of the same registration into this one. It must be ASSOCIATIVE
// AND COMMUTATIVE: the engine fans the columnar pass out across segment windows, and which worker
// draws which segment is a race. An order-sensitive merge is correct in every test its author
// writes and wrong in production.
func (a *stdDevAcc) Merge(other db.Accumulator) {
	b := other.(*stdDevAcc)
	if b.n == 0 {
		return
	}
	if a.n == 0 {
		*a = *b
		return
	}
	n := a.n + b.n
	d := b.mean - a.mean
	a.m2 += b.m2 + d*d*a.n*b.n/n
	a.mean += d * b.n / n
	a.n = n
}

// Result renders the group's answer. It may be called more than once and must not mutate the
// state, so it reads and returns rather than finalizing in place.
func (a *stdDevAcc) Result() classad.Value {
	if a.n == 0 {
		return classad.NewUndefinedValue()
	}
	return classad.NewRealValue(math.Sqrt(a.m2 / a.n))
}

// Example_userDefinedAggregate registers the aggregate above and runs a GROUP BY with it over a
// live table, exactly as a query naming SUM would run.
func Example_userDefinedAggregate() {
	if err := db.RegisterAggregate("stddev", stdDev{}); err != nil {
		panic(err)
	}

	d, err := db.OpenConfig(db.Config{})
	if err != nil {
		panic(err)
	}
	defer d.Close()

	tx := d.Begin()
	for i, src := range []string{
		`[ Pool = "cm1"; Memory = 1024 ]`,
		`[ Pool = "cm1"; Memory = 2048 ]`,
		`[ Pool = "cm1"; Memory = 4096 ]`,
		`[ Pool = "cm2"; Memory = 8192 ]`,
		`[ Pool = "cm2"; Memory = 8192 ]`,
		`[ Pool = "cm2"; Memory = "lots" ]`, // not a number: no aggregate counts it
	} {
		ad, perr := classad.Parse(src)
		if perr != nil {
			panic(perr)
		}
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if cerr := tx.Commit(); cerr != nil {
		panic(cerr)
	}

	// AggUser plus the registered name is how a request names it. Everything else about the
	// request -- the constraint, the group columns, the other aggregates -- is unchanged.
	rows, err := d.AggregateColsStats("true",
		[]db.GroupCol{{Attr: "Pool"}},
		[]db.AggSpec{
			{Func: db.AggCount, Arg: "*"},
			{Func: db.AggUser, UserFunc: "stddev", Arg: "Memory"},
		}, nil, nil)
	if err != nil {
		panic(err)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Group[0] < rows[j].Group[0] })
	for _, r := range rows {
		fmt.Printf("%s count=%s stddev=%s\n", r.Group[0], r.Values[0], r.Values[1])
	}

	// Output:
	// "cm1" count=3 stddev=1277.1523880188386
	// "cm2" count=3 stddev=0
}

// ExampleCheckAggregateEqual tests an aggregate against the laws the reduction relies on, over a
// sample of the caller's own choosing.
//
// The comparison is the caller's because this aggregate does floating-point arithmetic, and float
// addition is not associative: the same values divided into partial states a different way give
// answers differing in the last bits. That IS the order dependence the law is about, and it cannot
// be removed -- so an aggregate that needs slack says so here, in its own tests, where a reader can
// see how much.
func ExampleCheckAggregateEqual() {
	samples := []classad.Value{
		classad.NewIntValue(1),
		classad.NewIntValue(2),
		classad.NewRealValue(3.5),
		classad.NewIntValue(100),
		classad.NewStringValue("not a number"), // a value the aggregate must ignore
		classad.NewUndefinedValue(),            // and another
	}
	closeEnough := func(a, b classad.Value) bool {
		x, xerr := a.NumberValue()
		y, yerr := b.NumberValue()
		if xerr != nil || yerr != nil {
			return a.String() == b.String()
		}
		return math.Abs(x-y) <= 1e-9*math.Max(1, math.Abs(x))
	}
	fmt.Println(db.CheckAggregateEqual(stdDev{}, samples, closeEnough))

	// Output:
	// <nil>
}
