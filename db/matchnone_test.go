package db

import (
	"fmt"
	"testing"
	"time"
)

// A constraint that can never match should read nothing. `select min(ProcId) from history where false`
// measured 3.5s on a real table -- a full scan to establish that nothing matches.
func TestArchiveMatchNoneReadsNothing(t *testing.T) {
	cat, a := archiveCountFixture(t, 2000)
	defer cat.Close()

	for _, tc := range []struct {
		constraint string
		folds      bool
	}{
		{"false", true},
		{"1 == 2", true},
		{"undefined", true},
		{"error", true},
		{"false && true", true},
		{"true && false", true},
		// Anything referencing an attribute must NOT fold, however it evaluates against an empty ad.
		// `JobStatus == 2` is UNDEFINED with JobStatus absent, so a naive fold would call it match-none
		// while it matches plenty of records.
		{"JobStatus == 2", false},
		{"ProcId is undefined", false},
		{"true", false}, // match-ALL, not none
		{"", false},
	} {
		if got := IsMatchNone(tc.constraint); got != tc.folds {
			t.Errorf("IsMatchNone(%q) = %v, want %v", tc.constraint, got, tc.folds)
		}
	}

	// The answer must be what a scan matching nothing produces, aggregate by aggregate.
	for _, tc := range []struct {
		agg  AggSpec
		want string
	}{
		{AggSpec{Func: AggCount, Arg: "*"}, "0"},
		{AggSpec{Func: AggCount, Arg: "ProcId"}, "0"},
		{AggSpec{Func: AggMin, Arg: "ProcId"}, "undefined"},
		{AggSpec{Func: AggMax, Arg: "ProcId"}, "undefined"},
		// An empty SUM is int 0 rather than undefined, and AVG follows it -- deliberate, and documented at
		// ColumnarAggregate. Spelled out here because "undefined for everything but COUNT" is the obvious
		// guess and it is wrong for two of the six.
		{AggSpec{Func: AggSum, Arg: "ProcId"}, "0"},
		{AggSpec{Func: AggAvg, Arg: "ProcId"}, "0"},
	} {
		rows, err := a.AggregateCols("false", nil, []AggSpec{tc.agg})
		if err != nil {
			t.Fatalf("%v: %v", tc.agg, err)
		}
		if len(rows) != 1 || len(rows[0].Values) != 1 {
			t.Fatalf("%v: unexpected shape %+v", tc.agg, rows)
		}
		if rows[0].Values[0] != tc.want {
			t.Errorf("%v(%s) where false = %q, want %q", tc.agg.Func, tc.agg.Arg, rows[0].Values[0], tc.want)
		}
		// And it must equal what a constraint that matches nothing but CANNOT fold produces, which is the
		// scan's own answer for an empty result -- so the fold cannot drift from the thing it replaces.
		scanned, err := a.AggregateCols("ClusterId > 99999999", nil, []AggSpec{tc.agg})
		if err != nil {
			t.Fatal(err)
		}
		if scanned[0].Values[0] != rows[0].Values[0] {
			t.Errorf("%v(%s): folded %q but a scan matching nothing gave %q",
				tc.agg.Func, tc.agg.Arg, rows[0].Values[0], scanned[0].Values[0])
		}
	}
}

// TestArchiveMatchNoneIsFast pins the point of the change: it must not scan.
func TestArchiveMatchNoneIsFast(t *testing.T) {
	cat, a := archiveCountFixture(t, 20000)
	defer cat.Close()
	aggs := []AggSpec{{Func: AggMin, Arg: "ProcId"}}

	// Warm anything lazy, then compare a folded constraint against one that must scan.
	if _, err := a.AggregateCols("ClusterId >= 0", nil, aggs); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := a.AggregateCols("false", nil, aggs); err != nil {
		t.Fatal(err)
	}
	folded := time.Since(start)
	start = time.Now()
	if _, err := a.AggregateCols("ClusterId > 99999999", nil, aggs); err != nil {
		t.Fatal(err)
	}
	scanned := time.Since(start)
	t.Logf("min(ProcId): folded=%v  equivalent-but-unfoldable=%v", folded, scanned)
	// Deliberately loose: the claim is "does not read the table", and one is bounded work while the other
	// is proportional to it. A tight ratio would be a timing assertion, which is how flakes get written.
	if folded > scanned {
		t.Errorf("folding was not faster than scanning (%v vs %v); the fold is not taking effect",
			folded, scanned)
	}
	_ = fmt.Sprint
}
