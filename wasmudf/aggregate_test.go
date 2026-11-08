package wasmudf

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// loadAgg builds a Loader with opts and loads one aggregate fixture into it.
func loadAgg(t *testing.T, opts Options, name string) *UDAF {
	t.Helper()
	l := newLoader(t, opts)
	a, err := l.LoadAggregate(context.Background(), name, fixture(t, name))
	if err != nil {
		t.Fatalf("loading aggregate %s: %v", name, err)
	}
	return a
}

// loadAggErr loads an aggregate fixture expecting failure.
func loadAggErr(t *testing.T, opts Options, name string) error {
	t.Helper()
	l := newLoader(t, opts)
	_, err := l.LoadAggregate(context.Background(), name, fixture(t, name))
	if err == nil {
		t.Fatalf("loading aggregate %s succeeded; want an error", name)
	}
	return err
}

// newAcc builds one accumulator from a loaded aggregate.
func newAcc(t *testing.T, a *UDAF) db.Accumulator {
	t.Helper()
	acc := a.Aggregate().NewAccumulator()
	if acc == nil {
		t.Fatal("NewAccumulator returned nil")
	}
	return acc
}

// wantAggPanic runs fn expecting it to panic with an *AggError, and returns it.
//
// An aggregate reports a failure by panicking rather than by returning, because
// the engine's accumulator interface has no error return anywhere -- the
// containment is per query, caught by the aggregate pass, and reported against
// the aggregate's registered name. A unit test has to meet it the same way.
func wantAggPanic(t *testing.T, fn func()) *AggError {
	t.Helper()
	var got any
	func() {
		defer func() { got = recover() }()
		fn()
	}()
	if got == nil {
		t.Fatal("expected a panic from the aggregate, got none")
	}
	err, ok := got.(error)
	if !ok {
		t.Fatalf("panicked with %T (%v), want an error", got, got)
	}
	var ae *AggError
	if !errors.As(err, &ae) {
		t.Fatalf("panicked with %v, want an *AggError", err)
	}
	return ae
}

func ints(vs ...int64) []classad.Value {
	out := make([]classad.Value, len(vs))
	for i, v := range vs {
		out[i] = classad.NewIntValue(v)
	}
	return out
}

func addAll(acc db.Accumulator, vs []classad.Value) {
	for _, v := range vs {
		acc.Add(v)
	}
}

func TestAggregateSumsThroughTheGuest(t *testing.T) {
	a := loadAgg(t, Options{}, "aggsum")
	acc := newAcc(t, a)
	addAll(acc, ints(1, 2, 3, 4, 5))
	if got := db.ValueText(acc.Result()); got != "15" {
		t.Fatalf("sum = %s, want 15", got)
	}
	if s := a.Stats(); s.Failures != 0 {
		t.Fatalf("unexpected failures: %v", s.ByReason)
	}
}

// TestAggregateIdentityIsTheAnswerForNoRows: a fresh accumulator finalizes to the
// identity without the guest ever having folded anything.
//
// The identity is read from the guest ONCE, at load, and copied per accumulator.
// That is what makes building an accumulator free of side effects -- which
// matters because the engine decides whether an aggregate takes column values by
// building one and type-asserting it.
func TestAggregateIdentityIsTheAnswerForNoRows(t *testing.T) {
	a := loadAgg(t, Options{}, "aggsum")
	if got := db.ValueText(newAcc(t, a).Result()); got != "0" {
		t.Fatalf("identity = %s, want 0", got)
	}
	// Three accumulators, one init: the guest was asked for the identity at load
	// and not again.
	newAcc(t, a)
	newAcc(t, a)
	if got := a.Stats().Folds; got != 0 {
		t.Fatalf("%d folds for accumulators that were never fed", got)
	}
}

// TestAggregateMergeCarriesEveryFieldOfTheState is the test the fan-out rests on.
//
// The two sides hold DIFFERENT fields of the state: one has only integers, the
// other only a real. A merge that dropped the second state, folded the first one
// twice, or carried the sums but not the "a real was seen" flag each produces a
// different wrong answer, and all three are listed below so a failure says which
// happened rather than only that something did.
func TestAggregateMergeCarriesEveryFieldOfTheState(t *testing.T) {
	a := loadAgg(t, Options{}, "aggsum")
	x := newAcc(t, a)
	addAll(x, ints(1, 2, 3))
	y := newAcc(t, a)
	y.Add(classad.NewRealValue(0.5))

	x.Merge(y)
	got := db.ValueText(x.Result())
	switch got {
	case "6.500000000000000E+00", "6.5":
		// right
	case "6":
		t.Fatal("the real half of the merged state was dropped")
	case "12":
		t.Fatal("merge folded the first state twice instead of folding in the second")
	default:
		t.Fatalf("merged sum = %s, want 6.5", got)
	}
}

// TestMergeFoldsValuesNeitherSideHadFlushed: a merge is not allowed to leave the
// buffered values behind.
//
// Values wait in the host until there are enough to be worth a guest call, so an
// accumulator that saw fewer than a batch has folded NOTHING -- its whole
// contribution is still pending. A merge that combined the two guest states and
// ignored the buffers would return the identity here, and in a real query would
// return a sum over only the workers that happened to cross a batch boundary.
// Both sides are checked, since flushing one and not the other is the easy half
// of the mistake.
func TestMergeFoldsValuesNeitherSideHadFlushed(t *testing.T) {
	// A batch far larger than either side feeds it, so nothing can have been
	// flushed by the time Merge runs.
	a := loadAgg(t, Options{AggBatch: 4096}, "aggsum")
	x := newAcc(t, a)
	addAll(x, ints(10, 20))
	y := newAcc(t, a)
	addAll(y, ints(1, 2))
	if n := a.Stats().Folds; n != 0 {
		t.Fatalf("%d folds before the merge; the fixture is not exercising the pending case", n)
	}
	x.Merge(y)
	if got := db.ValueText(x.Result()); got != "33" {
		t.Fatalf("merged sum = %s, want 33 (30 from the receiver, 3 from the other side)", got)
	}
}

// TestResultIsRepeatableAndTracksLaterValues: Result may be called twice, and
// must answer the same twice -- but it is not a seal, so a value added
// afterwards still counts.
func TestResultIsRepeatableAndTracksLaterValues(t *testing.T) {
	a := loadAgg(t, Options{}, "aggsum")
	acc := newAcc(t, a)
	addAll(acc, ints(7, 8))
	first := db.ValueText(acc.Result())
	if second := db.ValueText(acc.Result()); second != first {
		t.Fatalf("Result moved between calls: %s then %s", first, second)
	}
	if got := a.Stats().Results; got != 1 {
		t.Fatalf("%d finalize calls for two Results; the second was not served from the cache", got)
	}
	acc.Add(classad.NewIntValue(5))
	if got := db.ValueText(acc.Result()); got != "20" {
		t.Fatalf("sum after a later Add = %s, want 20; the cached answer was not invalidated", got)
	}
}

// TestBatchingIsInvisible: the same values give the same answer whatever the
// batch size, including one that forces a fold per value.
//
// The batch exists only to amortize copying the state across the boundary. If it
// could change an answer it would be a semantic knob, and an operator tuning
// throughput would be tuning results.
func TestBatchingIsInvisible(t *testing.T) {
	vals := ints(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13)
	want := ""
	for _, batch := range []int{1, 2, 3, 13, 14, 4096} {
		a := loadAgg(t, Options{AggBatch: batch}, "aggsum")
		acc := newAcc(t, a)
		addAll(acc, vals)
		got := db.ValueText(acc.Result())
		if want == "" {
			want = got
		}
		if got != want {
			t.Fatalf("AggBatch %d gave %s, want %s", batch, got, want)
		}
		if wantFolds := uint64((len(vals) + batch - 1) / batch); a.Stats().Folds != wantFolds {
			t.Fatalf("AggBatch %d: %d folds, want %d; the fixture is not exercising the batch boundary",
				batch, a.Stats().Folds, wantFolds)
		}
	}
	if want != "91" {
		t.Fatalf("sum = %s, want 91", want)
	}
}

// TestStringsReachTheGuestIntact pins the string arena.
//
// A string value's slot carries a length and a POINTER into an arena the host
// lays out after the slots, so a wrong pointer, a wrong length, or bytes from the
// previous batch all give an answer rather than an error. The fixture sums the
// bytes it finds, which no wrong pointer reproduces.
func TestStringsReachTheGuestIntact(t *testing.T) {
	a := loadAgg(t, Options{}, "aggstrbytes")
	acc := newAcc(t, a)
	var want int64
	for _, s := range []string{"ab", "", "hello", "z", "classad"} {
		acc.Add(classad.NewStringValue(s))
		for _, b := range []byte(s) {
			want += int64(b)
		}
	}
	// Non-strings in the same batch, to show the slot walk does not lose its
	// place over a value it skips.
	acc.Add(classad.NewIntValue(99))
	acc.Add(classad.NewUndefinedValue())
	acc.Add(classad.NewStringValue("Q"))
	want += int64('Q')
	if got := db.ValueText(acc.Result()); got != fmt.Sprint(want) {
		t.Fatalf("string byte sum = %s, want %d", got, want)
	}
}

// TestTheStringArenaForcesAnEarlyFlush: a batch whose strings would overflow the
// arena is folded in pieces, and the pieces add up.
//
// The slot count is not the only bound on a batch, and a host that only counted
// slots would write string bytes past the region -- into whatever the guest keeps
// there.
func TestTheStringArenaForcesAnEarlyFlush(t *testing.T) {
	// A small arena, and a slot budget the fixture never reaches, so only the
	// arena can force a flush -- and small enough that a host that stopped
	// counting string bytes would run off the end of the values area rather than
	// merely folding in one piece.
	a := loadAgg(t, Options{RegionBytes: 4096, AggBatch: 64}, "aggstrbytes")
	acc := newAcc(t, a)
	const each = 600
	s := strings.Repeat("a", each)
	const n = 40
	for range n {
		acc.Add(classad.NewStringValue(s))
	}
	want := int64(n) * each * 'a'
	if got := db.ValueText(acc.Result()); got != fmt.Sprint(want) {
		t.Fatalf("string byte sum = %s, want %d", got, want)
	}
	if folds := a.Stats().Folds; folds < 2 {
		t.Fatalf("%d folds: the arena never overflowed, so this test is not exercising the early flush", folds)
	}
}

// TestAStringLargerThanTheArenaFails: no batch size can carry it, so it is a
// failure and not a smaller batch.
func TestAStringLargerThanTheArenaFails(t *testing.T) {
	a := loadAgg(t, Options{RegionBytes: 4096}, "aggstrbytes")
	acc := newAcc(t, a)
	err := wantAggPanic(t, func() { acc.Add(classad.NewStringValue(strings.Repeat("x", 8192))) })
	if err.Reason != ReasonArgsTooLarge {
		t.Fatalf("reason %v, want %v", err.Reason, ReasonArgsTooLarge)
	}
}

// TestAListArgumentFailsTheQueryRatherThanBeingSkipped: a value this ABI cannot carry is an
// error, not a silent omission.
//
// This ABI has no tag for a list or a nested ad. Skipping one would change the
// aggregate's answer on exactly the ads that carry it, silently; substituting
// undefined would do the same. So it fails, where an operator can see it.
func TestAListArgumentFailsTheQueryRatherThanBeingSkipped(t *testing.T) {
	a := loadAgg(t, Options{}, "aggsum")
	acc := newAcc(t, a)
	acc.Add(classad.NewIntValue(1))
	err := wantAggPanic(t, func() { acc.Add(classad.NewListValue(nil)) })
	if err.Reason != ReasonUnsupportedArg {
		t.Fatalf("reason %v, want %v", err.Reason, ReasonUnsupportedArg)
	}
	if !strings.Contains(err.Error(), a.Name()) {
		t.Fatalf("the error should name the aggregate, got %v", err)
	}
}

// --- what the guest can do wrong ---------------------------------------------

// TestATrappingFoldFailsAndNamesTheAggregate: a guest that dies mid-fold fails the query, and the
// error says whose module it was.
func TestATrappingFoldFailsAndNamesTheAggregate(t *testing.T) {
	a := loadAgg(t, Options{}, "aggtrap")
	acc := newAcc(t, a)
	err := wantAggPanic(t, func() {
		acc.Add(classad.NewIntValue(1))
		acc.Result()
	})
	if err.Reason != ReasonTrap {
		t.Fatalf("reason %v, want %v", err.Reason, ReasonTrap)
	}
	if !strings.Contains(err.Error(), "aggtrap") {
		t.Fatalf("the error should name the aggregate, got %v", err)
	}
	if n := a.Stats().ByReason[ReasonTrap]; n != 1 {
		t.Fatalf("counters %v, want one trap", a.Stats().ByReason)
	}
}

// TestTheStepBudgetStopsARunawayFold: an aggregate runs under the same
// deterministic budget as a function, and hits it rather than the clock.
func TestTheStepBudgetStopsARunawayFold(t *testing.T) {
	a := loadAgg(t, Options{Steps: 50_000}, "aggspin")
	acc := newAcc(t, a)
	err := wantAggPanic(t, func() {
		acc.Add(classad.NewIntValue(1))
		acc.Result()
	})
	if err.Reason != ReasonStepLimit {
		t.Fatalf("reason %v, want %v -- the clock must not get there first", err.Reason, ReasonStepLimit)
	}
}

// TestAGuestRefusingAFoldIsNotAStateOverflow keeps the two negative outcomes
// apart: a guest declining its input, and a guest reporting a state longer than
// the buffer it was given.
func TestAGuestRefusingAFoldIsNotAStateOverflow(t *testing.T) {
	a := loadAgg(t, Options{}, "aggrefuse")
	acc := newAcc(t, a)
	err := wantAggPanic(t, func() {
		acc.Add(classad.NewIntValue(1))
		acc.Result()
	})
	if err.Reason != ReasonStatus {
		t.Fatalf("reason %v, want %v", err.Reason, ReasonStatus)
	}
}

// TestAResultThatWasNeverWrittenIsRefused: the poison tag, which is what stops a
// pooled instance's previous answer being returned for this query.
func TestAResultThatWasNeverWrittenIsRefused(t *testing.T) {
	a := loadAgg(t, Options{}, "aggsilent")
	acc := newAcc(t, a)
	acc.Add(classad.NewIntValue(1))
	err := wantAggPanic(t, func() { acc.Result() })
	if err.Reason != ReasonBadResult {
		t.Fatalf("reason %v, want %v", err.Reason, ReasonBadResult)
	}
}

// TestAGuestRefusingToFinalizeIsNotBelieved: the status says whether the result
// slot means anything, and a slot holding a plausible number does not override it.
func TestAGuestRefusingToFinalizeIsNotBelieved(t *testing.T) {
	a := loadAgg(t, Options{}, "aggnoresult")
	acc := newAcc(t, a)
	acc.Add(classad.NewIntValue(1))
	err := wantAggPanic(t, func() { acc.Result() })
	if err.Reason != ReasonStatus {
		t.Fatalf("reason %v, want %v", err.Reason, ReasonStatus)
	}
	if !strings.Contains(err.Error(), "status 7") {
		t.Fatalf("the error should carry the status the guest returned, got %v", err)
	}
}

// TestTheLoadChecks: everything that can be refused before a query is.
func TestTheLoadChecks(t *testing.T) {
	for _, c := range []struct{ fixture, says string }{
		{"aggbiginit", "aggregate state does not fit"},
		{"aggbadsig", "classad_udaf_merge must be"},
		{"aggversion", "aggregate ABI version 99"},
		// A scalar module exports none of the aggregate entry points.
		{"identity", "no exported function \"classad_udaf_abi_version\""},
		{"hostimport", "this module imports"},
		{"nomemory", "no exported memory"},
	} {
		t.Run(c.fixture, func(t *testing.T) {
			err := loadAggErr(t, Options{}, c.fixture)
			if !strings.Contains(err.Error(), c.says) {
				t.Fatalf("loading %s said %q, want something containing %q", c.fixture, err, c.says)
			}
		})
	}
}

// TestAnAggregateModuleIsNotAFunctionModule, and the other way round. The two
// ABIs are separate export sets under separate versions, and loading one as the
// other is a load error rather than a module that half works.
func TestAnAggregateModuleIsNotAFunctionModule(t *testing.T) {
	l := newLoader(t, Options{})
	if _, err := l.Load(context.Background(), "aggsum", 1, 1, fixture(t, "aggsum")); err == nil {
		t.Fatal("an aggregate module loaded as a scalar UDF")
	}
	if _, err := l.LoadAggregate(context.Background(), "identity", fixture(t, "identity")); err == nil {
		t.Fatal("a scalar module loaded as an aggregate")
	}
}

// --- the columnar opt-in -------------------------------------------------------

// TestTheNumericFoldExportIsWhatDeclaresAColumnAccumulator, and nothing else does.
//
// This is a GATE and not a label: db reads it as the aggregate's promise that its
// answer does not depend on the order values arrive in and that it does not count
// what it was never offered. An aggregate that counts undefined -- aggcount --
// breaks the second promise, withholds the export, and must not come back as
// something the column pass will feed.
func TestTheNumericFoldExportIsWhatDeclaresAColumnAccumulator(t *testing.T) {
	for _, c := range []struct {
		fixture  string
		columnar bool
	}{
		{"aggsum", true},
		{"aggcount", false},
		{"aggstrbytes", false},
	} {
		t.Run(c.fixture, func(t *testing.T) {
			a := loadAgg(t, Options{}, c.fixture)
			if a.Columnar() != c.columnar {
				t.Fatalf("Columnar() = %v, want %v", a.Columnar(), c.columnar)
			}
			_, isCol := newAcc(t, a).(db.ColumnAccumulator)
			if isCol != c.columnar {
				t.Fatalf("accumulator implements db.ColumnAccumulator = %v, want %v", isCol, c.columnar)
			}
		})
	}
}

// TestTheTwoFoldPathsAgree: the numeric entry point and the value one give the
// same answer over the same numbers, which is the first of the two promises the
// opt-in makes.
func TestTheTwoFoldPathsAgree(t *testing.T) {
	a := loadAgg(t, Options{}, "aggsum")

	// Integers only first, and the answer stays an integer, so the comparison is
	// exact. One value is past 2^53, where a column value's float field is lossy
	// and its integer field is not: an AddNum reading F would agree with Add on
	// every small number and lose the low bits of a real one -- a byte count, or
	// an epoch in nanoseconds. Mixing a real in would hide it, because the
	// promotion to a float rounds both sides to the same number.
	nums := []int64{3, 1, 4, 1, 5, 9, 2, 6, (1 << 60) + 1}
	var want int64
	for _, n := range nums {
		want += n
	}

	byValue := newAcc(t, a)
	for _, n := range nums {
		byValue.Add(classad.NewIntValue(n))
	}
	byColumn := newAcc(t, a).(db.ColumnAccumulator)
	for _, n := range nums {
		byColumn.AddNum(db.NumValue{I: n, F: float64(n), Kind: db.NumInt})
	}
	if got := db.ValueText(byColumn.Result()); got != fmt.Sprint(want) {
		t.Fatalf("column path gave %s, want the exact %d", got, want)
	}
	if got := db.ValueText(byValue.Result()); got != fmt.Sprint(want) {
		t.Fatalf("value path gave %s, want the exact %d", got, want)
	}

	// Then with a real in the mix, where the two paths must still agree on the
	// promotion as well as on the arithmetic.
	byValue.Add(classad.NewRealValue(0.25))
	byColumn.AddNum(db.NumValue{F: 0.25, Kind: db.NumReal})
	if got, want := db.ValueText(byColumn.Result()), db.ValueText(byValue.Result()); got != want {
		t.Fatalf("column path gave %s, value path %s", got, want)
	}
}

// TestBooleansCoerceTheSameOnBothFoldPaths. A boolean column value carries its 0
// or 1 in I; a boolean classad.Value carries it in the payload. They have to
// arrive as the same tag and the same payload or the two rungs disagree on any
// aggregate over a boolean attribute.
func TestBooleansCoerceTheSameOnBothFoldPaths(t *testing.T) {
	a := loadAgg(t, Options{}, "aggsum")
	byValue := newAcc(t, a)
	byValue.Add(classad.NewBoolValue(true))
	byValue.Add(classad.NewBoolValue(false))
	byValue.Add(classad.NewBoolValue(true))

	byColumn := newAcc(t, a).(db.ColumnAccumulator)
	byColumn.AddNum(db.NumValue{I: 1, F: 1, Kind: db.NumBool})
	byColumn.AddNum(db.NumValue{I: 0, F: 0, Kind: db.NumBool})
	byColumn.AddNum(db.NumValue{I: 1, F: 1, Kind: db.NumBool})

	if got, want := db.ValueText(byColumn.Result()), db.ValueText(byValue.Result()); got != want {
		t.Fatalf("column path gave %s, value path %s", got, want)
	}
	if got := db.ValueText(byValue.Result()); got != "2" {
		t.Fatalf("sum over true,false,true = %s, want 2", got)
	}
}

// TestOneAccumulatorFedBothWaysKeepsEveryValue: a buffer holding values for two entry points is
// split rather than sent to one of them.
//
// The engine serves a query from one rung, so it never mixes the two -- but
// nothing in the interfaces forbids a caller doing it, and the buffered values
// have to go to the entry point they were pushed for. A buffer that sent
// everything to whichever path the LAST value chose would lose nothing visibly:
// it would just hand some values to a function that counts them differently.
func TestOneAccumulatorFedBothWaysKeepsEveryValue(t *testing.T) {
	a := loadAgg(t, Options{AggBatch: 4096}, "aggsum")
	acc := newAcc(t, a).(db.ColumnAccumulator)
	acc.AddNum(db.NumValue{I: 1, F: 1, Kind: db.NumInt})
	acc.Add(classad.NewIntValue(10))
	acc.AddNum(db.NumValue{I: 100, F: 100, Kind: db.NumInt})
	acc.Add(classad.NewIntValue(1000))
	if got := db.ValueText(acc.Result()); got != "1111" {
		t.Fatalf("sum = %s, want 1111", got)
	}
	// Four values in a batch of 4096 is one fold's worth, so every fold past the
	// first is a switch of entry point. Without the switch there would be one.
	if folds := a.Stats().Folds; folds != 4 {
		t.Fatalf("%d folds for four alternating values; the entry point switch did not force a flush", folds)
	}
}

// TestCheckAggregateAcceptsTheGuest runs the engine's own law checker over a wasm
// aggregate.
//
// The laws -- associative, commutative, identity, and the two column promises --
// are what the fan-out's correctness rests on, and they are checked here against
// the real boundary rather than against a Go reimplementation of it. The
// comparison carries a tolerance because the sample has reals in it and float
// addition is not associative; the integer-only run beside it is exact, which is
// what says the tolerance is paying for the floats and not hiding a bug.
func TestCheckAggregateAcceptsTheGuest(t *testing.T) {
	a := loadAgg(t, Options{}, "aggsum")

	exact := []classad.Value{
		classad.NewIntValue(5), classad.NewIntValue(-3), classad.NewIntValue(0),
		classad.NewIntValue(1 << 40), classad.NewUndefinedValue(),
		classad.NewStringValue("not a number"), classad.NewBoolValue(true),
	}
	if err := db.CheckAggregate(a.Aggregate(), exact); err != nil {
		t.Fatalf("the integer laws must hold exactly: %v", err)
	}

	mixed := append([]classad.Value{
		classad.NewRealValue(0.1), classad.NewRealValue(0.2), classad.NewRealValue(1e16),
		classad.NewRealValue(-7.5),
	}, exact...)
	near := func(x, y classad.Value) bool {
		fx, ex := x.RealValue()
		fy, ey := y.RealValue()
		if ex != nil || ey != nil {
			return db.ValueText(x) == db.ValueText(y)
		}
		return math.Abs(fx-fy) <= 1e-6*(math.Abs(fx)+math.Abs(fy)+1)
	}
	if err := db.CheckAggregateEqual(a.Aggregate(), mixed, near); err != nil {
		t.Fatalf("the laws must hold to a float error bound: %v", err)
	}
}

// --- registration and lifetime -------------------------------------------------

type fakeAggRegistry struct {
	names []string
	aggs  []db.Aggregate
	err   error
}

func (r *fakeAggRegistry) RegisterAggregate(name string, agg db.Aggregate) error {
	if r.err != nil {
		return r.err
	}
	r.names = append(r.names, name)
	r.aggs = append(r.aggs, agg)
	return nil
}

// TestRegisterAggregateRegistersWhatItLoaded, and registers a working aggregate
// rather than only a name.
func TestRegisterAggregateRegistersWhatItLoaded(t *testing.T) {
	l := newLoader(t, Options{})
	reg := &fakeAggRegistry{}
	a, err := RegisterAggregate(context.Background(), l, reg, "wsum", fixture(t, "aggsum"))
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.names) != 1 || reg.names[0] != "wsum" {
		t.Fatalf("registered %v, want [wsum]", reg.names)
	}
	acc := reg.aggs[0].NewAccumulator()
	addAll(acc, ints(4, 5, 6))
	if got := db.ValueText(acc.Result()); got != "15" {
		t.Fatalf("the registered aggregate gave %s, want 15", got)
	}
	if got, ok := l.LookupAggregate("wsum"); !ok || got != a {
		t.Fatal("the loaded aggregate is not reachable through the Loader")
	}
}

// TestNothingIsRegisteredWhenTheModuleIsBad: the load is first, so a module that
// cannot work never reaches a registry that cannot forget it.
func TestNothingIsRegisteredWhenTheModuleIsBad(t *testing.T) {
	l := newLoader(t, Options{})
	reg := &fakeAggRegistry{}
	if _, err := RegisterAggregate(context.Background(), l, reg, "bad", fixture(t, "aggbiginit")); err == nil {
		t.Fatal("a module whose identity does not fit was registered")
	}
	if len(reg.names) != 0 {
		t.Fatalf("registered %v after a failed load", reg.names)
	}
}

// TestAFailedRegistrationKeepsTheModuleLoaded, so a retry under another name does
// not recompile.
func TestAFailedRegistrationKeepsTheModuleLoaded(t *testing.T) {
	l := newLoader(t, Options{})
	reg := &fakeAggRegistry{err: errors.New("taken")}
	a, err := RegisterAggregate(context.Background(), l, reg, "wsum", fixture(t, "aggsum"))
	if err == nil {
		t.Fatal("expected the registration to fail")
	}
	if a == nil {
		t.Fatal("the loaded aggregate was discarded on a registration error")
	}
	if _, ok := l.LookupAggregate("wsum"); !ok {
		t.Fatal("the aggregate is not reachable through the Loader after a failed registration")
	}
}

// TestLoadingTheSameAggregateNameTwiceFails, so a name in a Loader means one module.
func TestLoadingTheSameAggregateNameTwiceFails(t *testing.T) {
	l := newLoader(t, Options{})
	if _, err := l.LoadAggregate(context.Background(), "s", fixture(t, "aggsum")); err != nil {
		t.Fatal(err)
	}
	if _, err := l.LoadAggregate(context.Background(), "s", fixture(t, "aggsum")); err == nil {
		t.Fatal("the same aggregate name loaded twice")
	}
}

// TestMergeRefusesAnAccumulatorFromAnotherAggregate. The engine never does this,
// so it is a bug in the engine rather than an input -- but the states are opaque
// bytes and a guest handed another aggregate's state computes on it rather than
// rejecting it, which would be a wrong number.
func TestMergeRefusesAnAccumulatorFromAnotherAggregate(t *testing.T) {
	l := newLoader(t, Options{})
	one, err := l.LoadAggregate(context.Background(), "one", fixture(t, "aggsum"))
	if err != nil {
		t.Fatal(err)
	}
	two, err := l.LoadAggregate(context.Background(), "two", fixture(t, "aggcount"))
	if err != nil {
		t.Fatal(err)
	}
	x := one.Aggregate().NewAccumulator()
	y := two.Aggregate().NewAccumulator()
	e := wantAggPanic(t, func() { x.Merge(y) })
	// The sentinel, not merely "it failed": the two fixtures have different
	// state lengths, so handing one's bytes to the other's merge would ALSO
	// fail -- as the guest refusing an input it could not read. That failure
	// happens to be loud here and would not be for two aggregates whose states
	// were the same size, which is the case this check exists for.
	if !errors.Is(e, errForeignAccumulator) {
		t.Fatalf("the merge should have been refused before reaching the guest, got %v", e)
	}
	if !strings.Contains(e.Error(), "one") {
		t.Fatalf("the error should name the receiving aggregate, got %v", e)
	}
}

// TestAnAggregateOutlivingItsLoaderFailsRatherThanCrashing, which it can, because closing a
// Loader withdraws nothing from db's registry.
//
// The two are not wired together: a registration outlives the Loader that serves
// it until somebody withdraws it by name.
// A scalar UDF in that position returns ERROR with ReasonInstance; an aggregate
// has no value to return, so it fails the query. What neither may do is take the
// process down, which is what an unguarded call on a closed module would.
func TestAnAggregateOutlivingItsLoaderFailsRatherThanCrashing(t *testing.T) {
	l, err := NewLoader(t.Context(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := l.LoadAggregate(t.Context(), "aggsum", fixture(t, "aggsum"))
	if err != nil {
		t.Fatal(err)
	}
	agg := a.Aggregate()
	if err := l.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Building one still works: the identity was read at load and is a copy.
	acc := agg.NewAccumulator()
	e := wantAggPanic(t, func() {
		acc.Add(classad.NewIntValue(1))
		acc.Result()
	})
	if e.Reason == ReasonPanic {
		t.Fatalf("a closed loader should fail cleanly, not panic inside the call path: %v", e)
	}
	if !strings.Contains(e.Error(), "aggsum") {
		t.Fatalf("the error should name the aggregate, got %v", e)
	}
}

// TestAccumulatorsAreIndependentAcrossGoroutines, although they share one instance pool.
//
// Each accumulator is fed by exactly one goroutine, but they share the UDAF's
// instance pool -- so a host that kept any per-accumulator state in an instance
// would hand one goroutine's values to another's sum. The answers are known and
// all different, so a crossed instance shows up as a wrong number rather than as
// a race the detector might or might not see.
func TestAccumulatorsAreIndependentAcrossGoroutines(t *testing.T) {
	a := loadAgg(t, Options{AggBatch: 8, MaxInstances: 3}, "aggsum")
	const workers = 16
	const per = 200
	got := make([]string, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			acc := a.Aggregate().NewAccumulator()
			for i := range per {
				acc.Add(classad.NewIntValue(int64(w*1000 + i)))
			}
			got[w] = db.ValueText(acc.Result())
		}()
	}
	wg.Wait()
	for w := range workers {
		var want int64
		for i := range per {
			want += int64(w*1000 + i)
		}
		if got[w] != fmt.Sprint(want) {
			t.Fatalf("worker %d summed to %s, want %d", w, got[w], want)
		}
	}
	if s := a.Stats(); s.Failures != 0 {
		t.Fatalf("unexpected failures: %v", s.ByReason)
	}
}

// TestATrappedInstanceIsNotReused. A guest stopped at an arbitrary instruction
// has its own invariants broken; the state an aggregate cares about is in the
// host, so nothing is lost by dropping the instance, and reusing it would mean
// the next fold computing rather than failing.
func TestATrappedInstanceIsNotReused(t *testing.T) {
	a := loadAgg(t, Options{MaxInstances: 1}, "aggtrap")
	before := a.Stats().Instances
	const traps = 4
	for range traps {
		acc := a.Aggregate().NewAccumulator()
		wantAggPanic(t, func() {
			acc.Add(classad.NewIntValue(1))
			acc.Result()
		})
	}
	// The load left one instance in the pool, which serves the first trap and is
	// then thrown away; every trap after that has to instantiate. A host that
	// returned a trapped instance to the pool would instantiate NONE of them.
	if got, want := a.Stats().Instances-before, uint64(traps-1); got != want {
		t.Fatalf("%d instances made for %d traps, want %d; a trapped instance must not go back in the pool",
			got, traps, want)
	}
}
