package db

import (
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// --- aggregates under test -------------------------------------------------------------------
//
// sumInts is the reference for everything here: an exact integer sum with O(1) state, skipping
// anything that is not a number. It is deliberately a thing the built-ins already do, so that an
// end-to-end result can be checked against an answer nobody has to take on trust.

type sumInts struct{}

func (sumInts) NewAccumulator() Accumulator { return &sumIntsAcc{} }

type sumIntsAcc struct{ n int64 }

func (a *sumIntsAcc) Add(v classad.Value) {
	if i, err := v.IntValue(); err == nil {
		a.n += i
	}
}
func (a *sumIntsAcc) Merge(other Accumulator) { a.n += other.(*sumIntsAcc).n }
func (a *sumIntsAcc) Result() classad.Value   { return classad.NewIntValue(a.n) }

// The broken ones below each violate exactly one law, so a rejection can be attributed. They are
// the test of CheckAggregate itself: a checker that accepts everything reads like a check and is
// worth less than none, because it ends the search.

// firstWins keeps the first value it was given. Add-order sensitivity makes it non-commutative
// under merge: a.Merge(b) keeps a's first, b.Merge(a) keeps b's.
type firstWins struct{}

func (firstWins) NewAccumulator() Accumulator { return &firstWinsAcc{} }

type firstWinsAcc struct {
	v   int64
	set bool
}

func (a *firstWinsAcc) Add(v classad.Value) {
	if i, err := v.IntValue(); err == nil && !a.set {
		a.v, a.set = i, true
	}
}
func (a *firstWinsAcc) Merge(other Accumulator) {
	o := other.(*firstWinsAcc)
	if !a.set && o.set {
		a.v, a.set = o.v, true
	}
}
func (a *firstWinsAcc) Result() classad.Value { return classad.NewIntValue(a.v) }

// drainOnResult finalizes in place, so the second Result is 0 and a state that is read before
// being merged into is silently emptied.
type drainOnResult struct{}

func (drainOnResult) NewAccumulator() Accumulator { return &drainAcc{} }

type drainAcc struct{ n int64 }

func (a *drainAcc) Add(v classad.Value) {
	if i, err := v.IntValue(); err == nil {
		a.n += i
	}
}
func (a *drainAcc) Merge(other Accumulator) { a.n += other.(*drainAcc).n }
func (a *drainAcc) Result() classad.Value {
	n := a.n
	a.n = 0
	return classad.NewIntValue(n)
}

// takesOther replaces its own state with the other side's instead of combining, so merging the
// identity in from the right wipes the group.
type takesOther struct{}

func (takesOther) NewAccumulator() Accumulator { return &takesOtherAcc{} }

type takesOtherAcc struct{ n int64 }

func (a *takesOtherAcc) Add(v classad.Value) {
	if i, err := v.IntValue(); err == nil {
		a.n += i
	}
}
func (a *takesOtherAcc) Merge(other Accumulator) { a.n = other.(*takesOtherAcc).n }
func (a *takesOtherAcc) Result() classad.Value   { return classad.NewIntValue(a.n) }

// depthSkew is the shape a non-associative merge actually takes in practice: an implementation
// that behaves differently ONCE IT HAS BEEN MERGED INTO -- switching representation after the
// first merge, say -- and gets the switched path slightly wrong. Every earlier law holds. Its
// merge is commutative, because both sides of any two-way check are fresh folds; and a two-way
// merge agrees with the serial fold, because the receiver has not been merged into yet. Only the
// regrouping tells it apart: ((a+b)+c) merges into a receiver of depth 1, (a+(b+c)) does not.
type depthSkew struct{}

func (depthSkew) NewAccumulator() Accumulator { return &depthSkewAcc{} }

type depthSkewAcc struct {
	n     int64
	depth int
}

func (a *depthSkewAcc) Add(v classad.Value) {
	if i, err := v.IntValue(); err == nil {
		a.n += i
	}
}
func (a *depthSkewAcc) Merge(other Accumulator) {
	o := other.(*depthSkewAcc)
	a.n += o.n
	if a.depth >= 1 {
		a.n++
	}
	a.depth++
}
func (a *depthSkewAcc) Result() classad.Value { return classad.NewIntValue(a.n) }

// singletonSkew is wrong ONLY when one side of a merge holds exactly one element -- the shape an
// implementation takes when it special-cases a state of one and gets the special case wrong. It is
// commutative, it merges the identity correctly, it is associative over the three-way split, and it
// agrees with the serial fold at every cut except the two that put a single element on one side. It
// is here because without it the interior split points cost nothing to remove, and the cuts exist
// precisely for implementations this package will never see.
type singletonSkew struct{}

func (singletonSkew) NewAccumulator() Accumulator { return &singletonSkewAcc{} }

type singletonSkewAcc struct {
	n int64
	c int
}

func (a *singletonSkewAcc) Add(v classad.Value) {
	if i, err := v.IntValue(); err == nil {
		a.n += i
		a.c++
	}
}
func (a *singletonSkewAcc) Merge(other Accumulator) {
	o := other.(*singletonSkewAcc)
	if a.c == 1 || o.c == 1 {
		return // drops a side, symmetrically, so only the serial comparison can see it
	}
	a.n += o.n
	a.c += o.c
}
func (a *singletonSkewAcc) Result() classad.Value { return classad.NewIntValue(a.n) }

// panicsOnAdd fails the way a real one would: not by returning an error, which the interface has
// no room for, but by blowing up on a value it did not expect.
type panicsOnAdd struct{}

func (panicsOnAdd) NewAccumulator() Accumulator { return &panicAcc{} }

type panicAcc struct{}

func (a *panicAcc) Add(v classad.Value)   { panic("no") }
func (a *panicAcc) Merge(o Accumulator)   {}
func (a *panicAcc) Result() classad.Value { return classad.NewUndefinedValue() }

// --- helpers ---------------------------------------------------------------------------------

func ints(ns ...int64) []classad.Value {
	out := make([]classad.Value, len(ns))
	for i, n := range ns {
		out[i] = classad.NewIntValue(n)
	}
	return out
}

// mustRegister registers under a name unique to the test, since the registry is process-wide and
// RegisterAggregate refuses a name that is taken: a shared name would make one test's failure
// depend on another having run.
func mustRegister(t *testing.T, name string, agg Aggregate) string {
	t.Helper()
	if err := RegisterAggregate(name, agg); err != nil {
		t.Fatalf("RegisterAggregate(%q): %v", name, err)
	}
	return name
}

// registerOnce registers name unless it is already there, for a test that may run more than once in
// a process (-count=2) against a registry RegisterAggregate will not overwrite.
func registerOnce(t *testing.T, name string, agg Aggregate) string {
	t.Helper()
	if _, ok := LookupAggregate(name); ok {
		return name
	}
	return mustRegister(t, name, agg)
}

// rowSeq turns rows of values into the sequence the reduction core consumes.
func rowSeq(rows [][]classad.Value) iter.Seq[[]classad.Value] {
	return func(yield func([]classad.Value) bool) {
		for _, r := range rows {
			if !yield(r) {
				return
			}
		}
	}
}

// --- the checker's own tests -----------------------------------------------------------------

func TestCheckAggregateAcceptsACorrectOne(t *testing.T) {
	if err := CheckAggregate(sumInts{}, ints(1, 2, 3, 4, 5, 6, 7)); err != nil {
		t.Fatalf("a correct aggregate was rejected: %v", err)
	}
	// Including the values that are not numbers at all, which is what the engine really hands an
	// aggregate for a row missing the attribute.
	mixed := append(ints(1, 2, 3, 4), classad.NewUndefinedValue(), classad.NewErrorValue(),
		classad.NewStringValue("x"), classad.NewRealValue(1.5))
	if err := CheckAggregate(sumInts{}, mixed); err != nil {
		t.Fatalf("a correct aggregate was rejected on mixed values: %v", err)
	}
}

func TestCheckAggregateRejectsEachBrokenLaw(t *testing.T) {
	samples := ints(1, 2, 3, 4, 5, 6, 7)
	for _, c := range []struct {
		name string
		agg  Aggregate
		says string // the law the message must name, so a rejection is attributable
	}{
		{"non-commutative merge", firstWins{}, "commutative"},
		{"Result finalizes in place", drainOnResult{}, "repeatable"},
		{"merge is not the identity", takesOther{}, "identity"},
		{"non-associative merge", depthSkew{}, "associative"},
		{"wrong only when a side holds one element", singletonSkew{}, "1/"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := CheckAggregate(c.agg, samples)
			if err == nil {
				t.Fatalf("accepted a broken aggregate (%s)", c.name)
			}
			if !strings.Contains(err.Error(), c.says) {
				t.Fatalf("rejected, but not for the reason it is broken:\n got: %v\nwant a message naming %q", err, c.says)
			}
		})
	}
}

// TestCheckAggregateRefusesATinySample guards the check against being vacuous. Three values cannot
// fill a three-way split, so associativity would compare a merge with an empty part against
// itself and pass whatever it was given.
func TestCheckAggregateRefusesATinySample(t *testing.T) {
	err := CheckAggregate(sumInts{}, ints(1, 2, 3))
	if !errors.Is(err, ErrBadAggregate) {
		t.Fatalf("want ErrBadAggregate for a 3-value sample, got %v", err)
	}
	if err := CheckAggregate(sumInts{}, ints(1, 2, 3, 4)); err != nil {
		t.Fatalf("4 values should be enough: %v", err)
	}
}

// TestCheckAggregateEqualTakesTheCallersComparison covers the reason the comparison is a parameter:
// an aggregate doing floating-point arithmetic cannot be exactly associative, and the exact
// comparison must reject it so that a caller is made to say what slack it needs.
func TestCheckAggregateEqualTakesTheCallersComparison(t *testing.T) {
	// A value whose last bits depend on the grouping: adding the small ones together first gives
	// a different sum from folding them into the large one.
	samples := []classad.Value{
		classad.NewRealValue(1e16), classad.NewRealValue(1), classad.NewRealValue(1),
		classad.NewRealValue(1), classad.NewRealValue(1), classad.NewRealValue(1),
	}
	exact := CheckAggregate(floatSum{}, samples)
	if exact == nil {
		t.Skip("this platform's float addition happened to be associative over these values")
	}
	near := func(a, b classad.Value) bool {
		x, _ := a.NumberValue()
		y, _ := b.NumberValue()
		d := x - y
		if d < 0 {
			d = -d
		}
		return d <= 4 // absolute, because the magnitudes here are fixed and known
	}
	if err := CheckAggregateEqual(floatSum{}, samples, near); err != nil {
		t.Fatalf("a float aggregate was rejected even with a tolerance: %v", err)
	}
}

type floatSum struct{}

func (floatSum) NewAccumulator() Accumulator { return &floatSumAcc{} }

type floatSumAcc struct{ f float64 }

func (a *floatSumAcc) Add(v classad.Value) {
	if f, err := v.NumberValue(); err == nil {
		a.f += f
	}
}
func (a *floatSumAcc) Merge(other Accumulator) { a.f += other.(*floatSumAcc).f }
func (a *floatSumAcc) Result() classad.Value   { return classad.NewRealValue(a.f) }

// --- the registry ----------------------------------------------------------------------------

func TestRegisterAggregate(t *testing.T) {
	name := mustRegister(t, "reg_sum", sumInts{})
	if _, ok := LookupAggregate(name); !ok {
		t.Fatal("registered aggregate does not look up")
	}
	if _, ok := LookupAggregate("REG_SUM"); !ok {
		t.Fatal("lookup is case-sensitive; a query layer folds names")
	}
	if err := RegisterAggregate(name, sumInts{}); !errors.Is(err, ErrAggregateExists) {
		t.Fatalf("re-registering should fail with ErrAggregateExists, got %v", err)
	}
	if err := RegisterAggregate("REG_SUM", sumInts{}); !errors.Is(err, ErrAggregateExists) {
		t.Fatalf("re-registering under a different case should still collide, got %v", err)
	}
	if err := RegisterAggregate("not a name", sumInts{}); !errors.Is(err, ErrBadAggregate) {
		t.Fatalf("want ErrBadAggregate for an unusable name, got %v", err)
	}
	if err := RegisterAggregate("nil_agg", nil); !errors.Is(err, ErrBadAggregate) {
		t.Fatalf("want ErrBadAggregate for a nil aggregate, got %v", err)
	}
	if _, ok := LookupAggregate("never_registered"); ok {
		t.Fatal("an unregistered name looked up")
	}
	found := false
	for _, n := range RegisteredAggregates() {
		if n == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("RegisteredAggregates() omitted %q", name)
	}
}

// --- end to end through the reduction core -----------------------------------------------------

func TestUserAggregateReducesLikeTheBuiltinItImitates(t *testing.T) {
	name := mustRegister(t, "e2e_sum", sumInts{})
	rows := [][]classad.Value{
		{classad.NewStringValue("alice"), classad.NewIntValue(3)},
		{classad.NewStringValue("bob"), classad.NewIntValue(10)},
		{classad.NewStringValue("alice"), classad.NewIntValue(4)},
		{classad.NewStringValue("alice"), classad.NewUndefinedValue()},
	}
	groups := []GroupCol{{Attr: "Owner"}}
	aggs := []AggSpec{
		{Func: AggUser, UserFunc: name, Arg: "N"},
		{Func: AggSum, Arg: "N"}, // the built-in, side by side, as the oracle
	}
	attrs, groupCol, aggCol := AggProjection(groups, aggs)
	if len(attrs) != 2 {
		t.Fatalf("a user aggregate should project its argument like any other: %v", attrs)
	}
	out, err := AggregateValues(rowSeq(rows), attrs, groups, aggs, groupCol, aggCol, nil)
	if err != nil {
		t.Fatalf("AggregateValues: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 groups, got %d: %+v", len(out), out)
	}
	for _, r := range out {
		if r.Values[0] != r.Values[1] {
			t.Fatalf("group %v: user aggregate said %q, the built-in it imitates said %q",
				r.Group, r.Values[0], r.Values[1])
		}
	}
}

// TestUserAggregateEmptyGroupGetsTheIdentity covers the case the accumulator has to exist for: a
// group every one of whose rows is excluded by THIS aggregate's filter still appears, and its
// answer can only come from a state nothing was added to.
func TestUserAggregateEmptyGroupGetsTheIdentity(t *testing.T) {
	name := mustRegister(t, "ident_sum", sumInts{})
	rows := [][]classad.Value{
		{classad.NewStringValue("alice"), classad.NewIntValue(3)},
		{classad.NewStringValue("alice"), classad.NewIntValue(4)},
	}
	groups := []GroupCol{{Attr: "Owner"}}
	aggs := []AggSpec{{Func: AggUser, UserFunc: name, Arg: "N", Filter: "N > 1000"}}
	attrs, groupCol, aggCol := AggProjection(groups, aggs)
	out, err := AggregateValues(rowSeq(rows), attrs, groups, aggs, groupCol, aggCol, nil)
	if err != nil {
		t.Fatalf("AggregateValues: %v", err)
	}
	if len(out) != 1 || out[0].Values[0] != "0" {
		t.Fatalf("a group whose rows all fail the filter should report the identity (0), got %+v", out)
	}
}

func TestUserAggregateUnknownNameFailsTheQuery(t *testing.T) {
	aggs := []AggSpec{{Func: AggUser, UserFunc: "no_such_aggregate_here", Arg: "N"}}
	attrs, groupCol, aggCol := AggProjection(nil, aggs)
	_, err := AggregateValues(rowSeq(nil), attrs, nil, aggs, groupCol, aggCol, nil)
	if !errors.Is(err, ErrNoSuchAggregate) {
		t.Fatalf("an unknown aggregate must fail the query, not answer it undefined; got %v", err)
	}
}

func TestUserAggregateOverStarIsRefused(t *testing.T) {
	name := mustRegister(t, "star_sum", sumInts{})
	aggs := []AggSpec{{Func: AggUser, UserFunc: name, Arg: "*"}}
	attrs, groupCol, aggCol := AggProjection(nil, aggs)
	_, err := AggregateValues(rowSeq(nil), attrs, nil, aggs, groupCol, aggCol, nil)
	if err == nil || !strings.Contains(err.Error(), "name an attribute") {
		t.Fatalf("a user aggregate over * should be refused like COUNT(DISTINCT *), got %v", err)
	}
}

// TestUserAggregatePanicFailsTheQueryRatherThanTheProcess pins the containment decision. A panic
// here is contained per QUERY and not per row, because a group whose accumulation stopped part-way
// does not give a worse answer, it gives a wrong one that looks like a right one.
func TestUserAggregatePanicFailsTheQueryRatherThanTheProcess(t *testing.T) {
	name := mustRegister(t, "panic_agg", panicsOnAdd{})
	rows := [][]classad.Value{{classad.NewIntValue(1)}}
	aggs := []AggSpec{{Func: AggUser, UserFunc: name, Arg: "N"}}
	attrs, groupCol, aggCol := AggProjection(nil, aggs)
	out, err := AggregateValues(rowSeq(rows), attrs, nil, aggs, groupCol, aggCol, nil)
	if err == nil {
		t.Fatalf("a panicking aggregate should fail the query, got rows %+v", out)
	}
	if out != nil {
		t.Fatalf("a failed query must return no rows, got %+v", out)
	}
	if !strings.Contains(err.Error(), name) {
		t.Fatalf("the error should name the aggregate that panicked, got %v", err)
	}
}

// TestColumnarAggregateDeclinesAUserAggregate pins the cliff that the registry's own comment leads
// with. The columnar pass reports a fixed set of numeric statistics -- count, sum, extremes and the
// type flags the built-ins need -- and an accumulator is not a function of those; answering from
// them would report some other aggregate's number.
func TestColumnarAggregateDeclinesAUserAggregate(t *testing.T) {
	name := mustRegister(t, "colagg_sum", sumInts{})
	stats := func(string) (NumStats, bool) {
		t.Fatal("the columnar path asked for statistics for a user aggregate")
		return NumStats{}, false
	}
	if rows, ok := ColumnarAggregate(stats, nil, []AggSpec{{Func: AggUser, UserFunc: name, Arg: "N"}}); ok {
		t.Fatalf("the columnar path served a user aggregate: %+v", rows)
	}
}
