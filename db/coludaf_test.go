package db

import (
	"errors"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// The two promises implementing ColumnAccumulator makes, as aggregates that break exactly one each.
// Neither can be caught by a signature, and both produce the same symptom in production: one query,
// two answers, depending on a rung choice the caller never made.

// colAscendingOnly sums, but its AddNum counts a value only when it does not go backwards. Over a
// sample in ascending order that is exactly the sum, so it agrees with Add and passes every merge
// law -- Merge and Add are plain addition. Only re-feeding the same values in another order tells
// it apart, which is the whole reason the pass's block order has to be a stated promise.
//
// Its first draft kept the FIRST value instead, and was rejected for a non-commutative merge before
// the column laws ran at all. A broken-law test has to break exactly one law or it attributes the
// rejection to the wrong rule.
type colAscendingOnly struct{}

func (colAscendingOnly) NewAccumulator() Accumulator { return &colAscendingOnlyAcc{} }

type colAscendingOnlyAcc struct {
	n    int64
	last int64
	seen bool
}

func (a *colAscendingOnlyAcc) Add(v classad.Value) {
	if i, err := v.IntValue(); err == nil {
		a.n += i
	}
}
func (a *colAscendingOnlyAcc) Merge(o Accumulator)   { a.n += o.(*colAscendingOnlyAcc).n }
func (a *colAscendingOnlyAcc) Result() classad.Value { return classad.NewIntValue(a.n) }
func (a *colAscendingOnlyAcc) AddNum(v NumValue) {
	if a.seen && v.I < a.last {
		return
	}
	a.n += v.I
	a.last, a.seen = v.I, true
}

// colDisagrees sums on both paths but doubles on the column one: the two rungs then answer the same
// query differently, which is the failure this interface is most exposed to.
type colDisagrees struct{}

func (colDisagrees) NewAccumulator() Accumulator { return &colDisagreesAcc{} }

type colDisagreesAcc struct{ n int64 }

func (a *colDisagreesAcc) Add(v classad.Value) {
	if i, err := v.IntValue(); err == nil {
		a.n += i
	}
}
func (a *colDisagreesAcc) Merge(o Accumulator)   { a.n += o.(*colDisagreesAcc).n }
func (a *colDisagreesAcc) Result() classad.Value { return classad.NewIntValue(a.n) }
func (a *colDisagreesAcc) AddNum(v NumValue)     { a.n += 2 * v.I }

// colPanics fails where only the columnar rung would reach it.
type colPanics struct{}

func (colPanics) NewAccumulator() Accumulator { return &colPanicsAcc{} }

type colPanicsAcc struct{ ladderSumAcc }

func (a *colPanicsAcc) Merge(o Accumulator) { a.n += o.(*colPanicsAcc).n }
func (a *colPanicsAcc) AddNum(v NumValue)   { panic("no") }

func TestCheckAggregateRejectsEachBrokenColumnLaw(t *testing.T) {
	samples := ints(1, 2, 3, 4, 5, 6, 7)
	for _, c := range []struct {
		name string
		agg  Aggregate
		says string
	}{
		{"AddNum is order-dependent", colAscendingOnly{}, "order-dependent"},
		{"AddNum disagrees with Add", colDisagrees{}, "disagrees with Add"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := CheckAggregate(c.agg, samples)
			if err == nil {
				t.Fatalf("accepted a broken ColumnAccumulator (%s)", c.name)
			}
			if !strings.Contains(err.Error(), c.says) {
				t.Fatalf("rejected, but not for the reason it is broken:\n got: %v\nwant a message naming %q", err, c.says)
			}
		})
	}
}

// TestCheckAggregateColumnLawsNeedNumbers guards the column half from passing vacuously: a sample
// with nothing the pass would ever yield compares the two paths over nothing.
func TestCheckAggregateColumnLawsNeedNumbers(t *testing.T) {
	strs := []classad.Value{
		classad.NewStringValue("a"), classad.NewStringValue("b"),
		classad.NewUndefinedValue(), classad.NewErrorValue(),
	}
	if err := CheckAggregate(ladderSumColAgg{}, strs); !errors.Is(err, ErrBadAggregate) {
		t.Fatalf("want ErrBadAggregate when no sample is numeric, got %v", err)
	}
	// And a correct one passes once there are numbers to compare over.
	if err := CheckAggregate(ladderSumColAgg{}, ints(1, 2, 3, 4, 5, 6, 7)); err != nil {
		t.Fatalf("a correct ColumnAccumulator was rejected: %v", err)
	}
}

// TestColumnarUserAggregateRefusals pins what the rung will not serve. Each is a shape where
// answering would report a different aggregate from the one asked for.
func TestColumnarUserAggregateRefusals(t *testing.T) {
	name := registerOnce(t, "refuse_sum_col", ladderSumColAgg{})
	scanName := registerOnce(t, "refuse_sum_scan", ladderSumScanAgg{})
	values := func(attr string, sink func(NumValue)) bool {
		t.Errorf("the rung ran a scan for a request it should have refused")
		return false
	}
	for _, c := range []struct {
		what   string
		groups []GroupCol
		aggs   []AggSpec
	}{
		{"grouped", []GroupCol{{Attr: "Owner"}}, []AggSpec{{Func: AggUser, UserFunc: name, Arg: "N"}}},
		{"two aggregates", nil, []AggSpec{{Func: AggUser, UserFunc: name, Arg: "N"}, {Func: AggCount, Arg: "*"}}},
		{"per-aggregate filter", nil, []AggSpec{{Func: AggUser, UserFunc: name, Arg: "N", Filter: "N > 1"}}},
		{"over star", nil, []AggSpec{{Func: AggUser, UserFunc: name, Arg: "*"}}},
		{"a built-in", nil, []AggSpec{{Func: AggSum, Arg: "N"}}},
		{"an aggregate that did not opt in", nil, []AggSpec{{Func: AggUser, UserFunc: scanName, Arg: "N"}}},
		{"an unregistered name", nil, []AggSpec{{Func: AggUser, UserFunc: "nope_not_here", Arg: "N"}}},
	} {
		t.Run(c.what, func(t *testing.T) {
			rows, ok, err := ColumnarUserAggregate(values, c.groups, c.aggs)
			if ok || err != nil {
				t.Fatalf("served %s: rows=%+v ok=%v err=%v", c.what, rows, ok, err)
			}
		})
	}
}

// TestColumnarUserAggregateDeclinesWhenTheScanCannot covers the other direction: the pass itself
// reporting that it cannot serve the column. The rung must decline, not answer from a partly-fed
// accumulator.
func TestColumnarUserAggregateDeclinesWhenTheScanCannot(t *testing.T) {
	name := registerOnce(t, "halffed_sum_col", ladderSumColAgg{})
	values := func(attr string, sink func(NumValue)) bool {
		sink(NumValue{F: 7, I: 7, Kind: NumInt}) // fed something, then gave up
		return false
	}
	rows, ok, err := ColumnarUserAggregate(values, nil, []AggSpec{{Func: AggUser, UserFunc: name, Arg: "N"}})
	if ok || err != nil || rows != nil {
		t.Fatalf("answered from a pass that declined: rows=%+v ok=%v err=%v", rows, ok, err)
	}
}

// TestColumnarUserAggregatePanicFailsTheQuery: the containment rule holds on this rung too, and for
// the same reason -- a half-accumulated group is a wrong answer that looks like a right one.
func TestColumnarUserAggregatePanicFailsTheQuery(t *testing.T) {
	name := registerOnce(t, "panic_sum_col", colPanics{})
	values := func(attr string, sink func(NumValue)) bool {
		sink(NumValue{F: 1, I: 1, Kind: NumInt})
		return true
	}
	rows, ok, err := ColumnarUserAggregate(values, nil, []AggSpec{{Func: AggUser, UserFunc: name, Arg: "N"}})
	if err == nil {
		t.Fatalf("a panicking AddNum should fail the query, got rows=%+v ok=%v", rows, ok)
	}
	if rows != nil || ok {
		t.Fatalf("a failed query must return no rows: rows=%+v ok=%v", rows, ok)
	}
	if !strings.Contains(err.Error(), name) {
		t.Fatalf("the error should name the aggregate that panicked, got %v", err)
	}
}
