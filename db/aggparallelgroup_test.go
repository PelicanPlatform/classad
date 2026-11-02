package db

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// The grouped columnar pass fans out too, and its fold is harder than the ungrouped one: each
// worker builds its own group map AND its own query-local string table, so a string group key is a
// different integer in each. These tests are built around that.

// parGroupTable builds a grouped fixture with STRING group keys, which is the case the fold can get
// wrong, at a gate low enough to fan out cheaply.
func parGroupTable(t *testing.T, n, par int) *DB {
	t.Helper()
	d, err := OpenConfig(Config{SegmentSize: 1 << 16, QueryParallelism: par, AggMinBytes: testAggGate})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	tx := d.Begin()
	for i := 0; i < n; i++ {
		ad := classad.New()
		// MANY distinct owners, interleaved so every worker meets a different subset in a
		// different order: that is what makes the two string tables disagree about which id means
		// which name. With one owner, or with owners that arrive in blocks, a fold that ignored
		// the remap could still come out right.
		ad.InsertAttrString("Owner", fmt.Sprintf("user%d", i%37))
		ad.InsertAttr("Cpus", int64(i%64))
		ad.InsertAttr("JobStatus", int64(i%5))
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
		if i%20000 == 19999 {
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			tx = d.Begin()
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if !d.EnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to accelerate")
	}
	return d
}

// groupRows keys a result by its whole group TUPLE, since a GROUP BY promises no order and the two
// rungs do not agree on one. The package's rowsByGroup insists on a single group column; these
// tests also cover two.
func groupRows(rows []AggRow) map[string][]string {
	m := make(map[string][]string, len(rows))
	for _, r := range rows {
		m[fmt.Sprint(r.Group)] = r.Values
	}
	return m
}

// TestGroupedAggregatesAgreeAcrossTheFanout: the grouped pass gives the same groups and the same
// values fanned out as it does serially, for a string key and for an integer one.
func TestGroupedAggregatesAgreeAcrossTheFanout(t *testing.T) {
	const n = 20000
	serial := parGroupTable(t, n, 1)
	par := parGroupTable(t, n, 0)

	for _, c := range []struct {
		what   string
		groups []GroupCol
		aggs   []AggSpec
	}{
		{"string key, sum", []GroupCol{{Attr: "Owner"}}, []AggSpec{{Func: AggSum, Arg: "Cpus"}}},
		{"string key, count", []GroupCol{{Attr: "Owner"}}, []AggSpec{{Func: AggCount, Arg: "*"}}},
		{"string key, min+max", []GroupCol{{Attr: "Owner"}},
			[]AggSpec{{Func: AggMin, Arg: "Cpus"}, {Func: AggMax, Arg: "Cpus"}}},
		{"int key, sum", []GroupCol{{Attr: "JobStatus"}}, []AggSpec{{Func: AggSum, Arg: "Cpus"}}},
		{"two keys", []GroupCol{{Attr: "Owner"}, {Attr: "JobStatus"}},
			[]AggSpec{{Func: AggSum, Arg: "Cpus"}}},
		{"filtered count", []GroupCol{{Attr: "Owner"}},
			[]AggSpec{{Func: AggCount, Arg: "*", Filter: "Cpus > 30"}}},
	} {
		t.Run(c.what, func(t *testing.T) {
			var want, got []AggRow
			var err error
			if fannedOut(t, func() { want, err = serial.AggregateColsStats("true", c.groups, c.aggs, nil, nil) }) {
				t.Fatal("the serial table fanned out, so this compares two parallel runs")
			}
			if err != nil {
				t.Fatal(err)
			}
			if !fannedOut(t, func() { got, err = par.AggregateColsStats("true", c.groups, c.aggs, nil, nil) }) {
				t.Fatal("the pass never fanned out, so this compares the serial path with itself")
			}
			if err != nil {
				t.Fatal(err)
			}
			w, g := groupRows(want), groupRows(got)
			if len(w) != len(g) {
				t.Fatalf("%d groups serial, %d fanned out: the fold split or fused groups", len(w), len(g))
			}
			for k, wv := range w {
				gv, ok := g[k]
				if !ok {
					t.Fatalf("group %s is missing from the fanned-out answer", k)
				}
				if fmt.Sprint(wv) != fmt.Sprint(gv) {
					t.Fatalf("group %s: serial %v, fanned out %v", k, wv, gv)
				}
			}
		})
	}
}

// TestGroupedUserAggregateFoldsPerWorkerSinks: a user-defined aggregate under a GROUP BY gets one
// sink per worker per group, folded with its own Merge.
func TestGroupedUserAggregateFoldsPerWorkerSinks(t *testing.T) {
	var news, merges atomic.Int64
	name := registerOnce(t, "pargrp_fold_sum", parSum{news: &news, merges: &merges})
	const n = 20000
	serial := parGroupTable(t, n, 1)
	par := parGroupTable(t, n, 0)
	aggs := []AggSpec{{Func: AggUser, UserFunc: name, Arg: "Cpus"}}
	groups := []GroupCol{{Attr: "Owner"}}

	want, err := serial.AggregateColsStats("true", groups, aggs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	merges.Store(0)
	var got []AggRow
	if !fannedOut(t, func() { got, err = par.AggregateColsStats("true", groups, aggs, nil, nil) }) {
		t.Fatal("the pass never fanned out, so nothing here exercises the fold")
	}
	if err != nil {
		t.Fatal(err)
	}
	w, g := groupRows(want), groupRows(got)
	if len(w) != len(g) {
		t.Fatalf("%d groups serial, %d fanned out", len(w), len(g))
	}
	for k, wv := range w {
		if gv := g[k]; fmt.Sprint(gv) != fmt.Sprint(wv) {
			t.Fatalf("group %s: serial %v, fanned out %v", k, wv, gv)
		}
	}
	// A group seen by two workers has two sinks to fold. With 37 owners spread over far more
	// segments than workers, essentially every group is seen by more than one.
	if merges.Load() == 0 {
		t.Fatal("no sink was ever merged: the per-worker group maps were not folded")
	}
}

// TestGroupedFanoutIsNotFooledByStringIds is the fold's hard case, isolated.
//
// Each worker interns group strings into a table of its own, in the order it happens to meet them,
// so the same name is a different id in each and two different names collide on one id. A fold that
// unioned the maps by their stored keys would therefore SPLIT a group across ids and FUSE two
// groups that shared one -- and the result would be a plausible set of counts with nothing wrong
// about its shape. The guard is that every group's count is known exactly: owner i gets exactly the
// records with i == n%37.
func TestGroupedFanoutIsNotFooledByStringIds(t *testing.T) {
	const n = 20000
	par := parGroupTable(t, n, 0)
	var rows []AggRow
	var err error
	if !fannedOut(t, func() {
		rows, err = par.AggregateColsStats("true", []GroupCol{{Attr: "Owner"}},
			[]AggSpec{{Func: AggCount, Arg: "*"}}, nil, nil)
	}) {
		t.Fatal("the pass never fanned out, so the string tables were never folded")
	}
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{}
	for i := 0; i < n; i++ {
		want[fmt.Sprintf("user%d", i%37)]++
	}
	if len(rows) != len(want) {
		t.Fatalf("%d groups, want %d: the fold split or fused string groups", len(rows), len(want))
	}
	total := 0
	seen := map[string]bool{}
	for _, r := range rows {
		owner := fmt.Sprint(r.Group[0])
		w, ok := want[owner]
		if !ok {
			t.Fatalf("group %q is not an owner in the data; a string id was read against the wrong table", owner)
		}
		if seen[owner] {
			t.Fatalf("owner %s appears twice: the fold split one group across two string ids", owner)
		}
		seen[owner] = true
		if r.Values[0] != fmt.Sprint(w) {
			t.Fatalf("owner %s: count %s, want %d", owner, r.Values[0], w)
		}
		total += w
	}
	// Every record landed in exactly one group. A fold that fused two owners would lose a group
	// and show up above; one that dropped records shows up here.
	if total != n {
		t.Fatalf("the group counts sum to %d, not %d: records were lost or double-counted in the fold", total, n)
	}
}

// TestADecliningWindowDeclinesTheWholeGroupedFanout is the grouped counterpart of the ungrouped
// decline test, and it exists because its absence was a hole the mutation run found: ignoring a
// worker's decline and letting the others answer survived every test above.
//
// It has to, because what it produces is not an error or a crash. A worker that hits a window the
// pass cannot read, and simply stops, leaves the other workers' groups intact -- so the answer is a
// GROUP BY over a SUBSET of the records, with every group label right, every count plausible, and
// nothing about its shape to assert against. Only the total gives it away, and only if something
// knows what the total should be.
func TestADecliningWindowDeclinesTheWholeGroupedFanout(t *testing.T) {
	const n = 20000
	d, err := OpenConfig(Config{SegmentSize: 1 << 16, QueryParallelism: 0, AggMinBytes: testAggGate})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tx := d.Begin()
	for i := 0; i < n; i++ {
		ad := classad.New()
		ad.InsertAttrString("Owner", fmt.Sprintf("user%d", i%37))
		if i == n-1 {
			// The LAST record, so the pass declines only after almost everything is accumulated.
			// That is when a partial answer is at its most complete and most believable.
			ad.InsertExpr("Cpus", mustExpr(t, "1 + 1"))
		} else {
			ad.InsertAttr("Cpus", int64(i%64))
		}
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
		if i%20000 == 19999 {
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			tx = d.Begin()
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if !d.EnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to accelerate")
	}

	var news, merges atomic.Int64
	name := registerOnce(t, "pargrp_decline_col", parSum{news: &news, merges: &merges})
	groups := []GroupCol{{Attr: "Owner"}}

	for _, c := range []struct {
		what string
		aggs []AggSpec
		rung string
	}{
		{"built-in", []AggSpec{{Func: AggSum, Arg: "Cpus"}}, "grouped-columns"},
		{"user-defined", []AggSpec{{Func: AggUser, UserFunc: name, Arg: "Cpus"}}, "grouped-user-columns"},
	} {
		t.Run(c.what, func(t *testing.T) {
			var rows []AggRow
			var ok bool
			var err error
			if !fannedOut(t, func() {
				rows, ok, err = d.aggLadderAnswer(t, c.rung,
					AggRequest{Constraint: "true", GroupCols: groups, Aggs: c.aggs})
			}) {
				t.Fatal("the pass never fanned out, so this tests the serial decline, not the fan-out")
			}
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				t.Fatalf("the rung answered %d group(s) over a column it cannot read; those groups "+
					"cover only the records read before the pass gave up", len(rows))
			}
			if rows != nil {
				t.Fatalf("a declining rung must return no rows, got %d", len(rows))
			}
		})
	}

	// And the query as a whole is still right, from the rung below: every record in exactly one
	// group, including the one holding an expression.
	rows, err := d.AggregateColsStats("true", groups, []AggSpec{{Func: AggCount, Arg: "*"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{}
	for i := 0; i < n; i++ {
		want[fmt.Sprintf("user%d", i%37)]++
	}
	if len(rows) != len(want) {
		t.Fatalf("%d groups after the decline, want %d", len(rows), len(want))
	}
	total := 0
	for _, r := range rows {
		owner := fmt.Sprint(r.Group[0])
		if r.Values[0] != fmt.Sprint(want[owner]) {
			t.Fatalf("owner %s: count %s, want %d", owner, r.Values[0], want[owner])
		}
		total += want[owner]
	}
	if total != n {
		t.Fatalf("the scan's group counts sum to %d, not %d", total, n)
	}
}
