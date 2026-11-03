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
		// A group's label is the value's ClassAd literal, so a string group carries its quotes.
		// That is what keeps the string "4" and the integer 4 in separate groups, and it means an
		// expected label has to be written the same way rather than as the bare owner name.
		want[fmt.Sprintf("%q", fmt.Sprintf("user%d", i%37))]++
	}
	if len(rows) != len(want) {
		t.Fatalf("%d groups, want %d: the fold split or fused string groups", len(rows), len(want))
	}
	total := 0
	seen := map[string]bool{}
	for _, r := range rows {
		owner := r.Group[0]
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
