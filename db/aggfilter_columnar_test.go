package db

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/collections"
)

// Per-aggregate FILTER on the COLUMNAR path.
//
// db/aggfilter_test.go covers what a filter MEANS, on a seven-record table that never seals and so
// is answered by the record scan every time. These cover the same meaning on a table the columnar
// accelerator actually serves, which is a different code path reaching the same answers -- and the
// tier assertions below are what stops them passing by quietly falling back to that scan, which is
// how a columnar fast path comes to be unprotected while its tests are green.

// aggRowMap flattens grouped rows to group-text -> values, so the sorted columnar result and the
// first-seen-order scan result compare without depending on an order neither defines.
func aggRowMap(tb testing.TB, rows []AggRow) map[string][]string {
	tb.Helper()
	out := make(map[string][]string, len(rows))
	for _, r := range rows {
		k := strings.Join(r.Group, "|")
		if _, dup := out[k]; dup {
			tb.Errorf("group %q appears twice: one group was split into two rows", k)
		}
		out[k] = r.Values
	}
	return out
}

// sameAsScan compares a request's columnar answer with the record scan's, which is the reference
// every rung is defined against. It returns the tier that answered so a caller can insist on one.
func sameAsScan(t *testing.T, a *ArchiveTable, constraint string, groupCols []GroupCol,
	aggs []AggSpec) collections.AggTier {
	t.Helper()
	var stats collections.ScanStats
	got, err := a.AggregateColsStats(constraint, groupCols, aggs, &stats)
	if err != nil {
		t.Fatalf("%s: %v", constraint, err)
	}
	want := scanAggregate(t, a, constraint, groupCols, aggs)
	gotM, wantM := aggRowMap(t, got), aggRowMap(t, want)
	if len(gotM) != len(wantM) {
		t.Errorf("%s: %d groups, scan found %d", constraint, len(gotM), len(wantM))
	}
	keys := make([]string, 0, len(wantM))
	for g := range wantM {
		keys = append(keys, g)
	}
	sort.Strings(keys)
	for _, g := range keys {
		w := wantM[g]
		v, ok := gotM[g]
		if !ok {
			t.Errorf("%s: group %q missing (scan has %v)", constraint, g, w)
			continue
		}
		if len(v) != len(w) {
			t.Errorf("%s: group %q has %d values, scan %d", constraint, g, len(v), len(w))
			continue
		}
		for i := range w {
			if v[i] != w[i] {
				t.Errorf("%s: group %q value %d = %q, scan %q", constraint, g, i, v[i], w[i])
			}
		}
	}
	for g := range gotM {
		if _, ok := wantM[g]; !ok {
			t.Errorf("%s: group %q is not in the scan's result at all", constraint, g)
		}
	}
	return stats.Tier
}

// TestFilteredAggregateIsColumnar is the anti-vacuity check. Every comparison below could pass by
// declining to the scan on both sides -- which is what this path did before filters were served --
// so the pivot shape the feature exists for has to be answered BY THE COLUMNS or there is nothing
// here to test.
func TestFilteredAggregateIsColumnar(t *testing.T) {
	cat, a := archiveCountFixture(t, 5000)
	defer cat.Close()

	pivot := []AggSpec{
		{Func: AggCount, Arg: "*"},
		{Func: AggCount, Arg: "*", Filter: "JobStatus == 4"},
		{Func: AggSum, Arg: "RequestCpus", Filter: "JobStatus == 4"},
		{Func: AggMax, Arg: "RequestCpus"},
	}
	groupCols := []GroupCol{{Attr: "ProcId"}}
	if tier := sameAsScan(t, a, "RequestMemory > 2048", groupCols, pivot); tier != collections.TierColumnar {
		t.Fatalf("the pivot shape was answered by %v, not the columns: nothing about per-aggregate "+
			"FILTER on the columnar path is exercised", tier)
	}
	// And with no WHERE clause at all, which is a separate entry point (GroupStatsAllAggs) with no
	// predicate analysis to ride in on.
	if tier := sameAsScan(t, a, "true", groupCols, pivot); tier != collections.TierColumnar {
		t.Fatalf("the unconstrained pivot was answered by %v, not the columns", tier)
	}
}

// TestFilteredAggregateMatchesScan sweeps the filter shapes the columns serve against the reference,
// over one and two group columns, bucketed and raw, constrained and not.
func TestFilteredAggregateMatchesScan(t *testing.T) {
	cat, a := archiveCountFixture(t, 5000)
	defer cat.Close()

	const day = 86400
	for _, tc := range []struct {
		name       string
		constraint string
		groups     []GroupCol
		aggs       []AggSpec
	}{
		{"count-and-filtered-count", "RequestMemory > 2048", []GroupCol{{Attr: "ProcId"}},
			[]AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggCount, Arg: "*", Filter: "JobStatus == 4"}}},
		{"two-filters-one-column", "true", []GroupCol{{Attr: "ProcId"}},
			[]AggSpec{
				{Func: AggCount, Arg: "*", Filter: "JobStatus == 4"},
				{Func: AggCount, Arg: "*", Filter: "JobStatus == 2"},
			}},
		// The same attribute under two different filters: two columns with two accumulators, which
		// is the case a dedup key that ignored the filter would silently collapse into one.
		{"same-attr-two-filters", "true", []GroupCol{{Attr: "ProcId"}},
			[]AggSpec{
				{Func: AggSum, Arg: "RequestCpus", Filter: "JobStatus == 4"},
				{Func: AggSum, Arg: "RequestCpus", Filter: "JobStatus == 2"},
				{Func: AggSum, Arg: "RequestCpus"},
			}},
		// MIN(x) beside MAX(x) under ONE filter still shares its column.
		{"shared-column-one-filter", "true", []GroupCol{{Attr: "JobStatus"}},
			[]AggSpec{
				{Func: AggMin, Arg: "RequestCpus", Filter: "RequestMemory > 4096"},
				{Func: AggMax, Arg: "RequestCpus", Filter: "RequestMemory > 4096"},
			}},
		{"conjunction-filter", "true", []GroupCol{{Attr: "ProcId"}},
			[]AggSpec{{Func: AggCount, Arg: "*", Filter: "JobStatus == 4 && RequestCpus >= 4"}}},
		{"filter-on-the-group-column", "true", []GroupCol{{Attr: "JobStatus"}},
			[]AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggCount, Arg: "*", Filter: "JobStatus > 2"}}},
		{"filter-on-the-where-column", "RequestMemory > 2048", []GroupCol{{Attr: "ProcId"}},
			[]AggSpec{{Func: AggCount, Arg: "*", Filter: "RequestMemory > 8192"}}},
		// A filter nothing passes, and one everything passes: the two edges of the plane.
		{"filter-matches-nothing", "true", []GroupCol{{Attr: "ProcId"}},
			[]AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggCount, Arg: "*", Filter: "JobStatus > 9999"},
				{Func: AggAvg, Arg: "RequestCpus", Filter: "JobStatus > 9999"}}},
		{"filter-matches-everything", "true", []GroupCol{{Attr: "ProcId"}},
			[]AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggCount, Arg: "*", Filter: "JobStatus >= 0"}}},
		{"two-group-columns", "true", []GroupCol{{Attr: "ProcId"}, {Attr: "JobStatus"}},
			[]AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggCount, Arg: "*", Filter: "RequestCpus >= 4"}}},
		{"bucketed-group-column", "true", []GroupCol{{Attr: "CompletionDate", BucketWidth: day}},
			[]AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggCount, Arg: "*", Filter: "JobStatus == 4"}}},
		{"string-group-column", "true", []GroupCol{{Attr: "Owner"}},
			[]AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggCount, Arg: "*", Filter: "JobStatus == 4"}}},
		{"every-function-filtered", "true", []GroupCol{{Attr: "ProcId"}},
			[]AggSpec{
				{Func: AggCount, Arg: "RequestCpus", Filter: "JobStatus == 4"},
				{Func: AggMin, Arg: "RequestCpus", Filter: "JobStatus == 4"},
				{Func: AggMax, Arg: "RequestCpus", Filter: "JobStatus == 4"},
				{Func: AggSum, Arg: "RequestCpus", Filter: "JobStatus == 4"},
				{Func: AggAvg, Arg: "RequestCpus", Filter: "JobStatus == 4"},
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tier := sameAsScan(t, a, tc.constraint, tc.groups, tc.aggs)
			t.Logf("tier=%v", tier)
		})
	}
}

// TestFilteredAggregateDeclinesGracefully covers the filters the column narrowing cannot express.
// Each must still return the SCAN's answer -- a decline is a slow query and never a wrong one --
// and the tier must say so, because a shape silently answered columnar here would be answering a
// filter it had not actually applied.
func TestFilteredAggregateDeclinesGracefully(t *testing.T) {
	cat, a := archiveCountFixture(t, 5000)
	defer cat.Close()

	for _, tc := range []struct{ name, filter string }{
		{"string-comparison", `Owner == "user3"`},
		{"disjunction", "JobStatus == 4 || JobStatus == 2"},
		{"attribute-to-attribute", "RequestMemory > RequestCpus"},
		{"unknown-attribute", "NoSuchAttribute == 1"},
		{"arithmetic", "RequestCpus * 2 > 8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			aggs := []AggSpec{
				{Func: AggCount, Arg: "*"},
				{Func: AggCount, Arg: "*", Filter: tc.filter},
			}
			tier := sameAsScan(t, a, "true", []GroupCol{{Attr: "ProcId"}}, aggs)
			if tier == collections.TierColumnar {
				t.Errorf("filter %q was answered columnar; the column narrowing cannot express it, "+
					"so it was either lowered wrongly or not applied", tc.filter)
			}
		})
	}
}

// TestFilteredAggregateKeepsEmptyGroups pins what a group whose every record fails the filter
// RENDERS as: zero for a filtered COUNT(*) and undefined for the other functions, rather than the
// group's unfiltered numbers. That a filter cannot DROP such a group is structural here -- group
// identity is decided before any filter is consulted and the key has no filter in it -- so what is
// worth testing is the rendering, which a count that forgot to consult the filter gets wrong.
func TestFilteredAggregateKeepsEmptyGroups(t *testing.T) {
	cat, a := archiveCountFixture(t, 5000)
	defer cat.Close()

	// In this fixture ProcId is i%10 and JobStatus is 1+i%5, so JobStatus is determined by ProcId:
	// group 0 has JobStatus 1 throughout and group 5 is the only other one that does. The filter
	// below therefore passes in exactly ONE of the ten groups, and the nine others exist only
	// because a filter does not decide which groups there are.
	groupCols := []GroupCol{{Attr: "ProcId"}}
	aggs := []AggSpec{
		{Func: AggCount, Arg: "*"},
		{Func: AggCount, Arg: "*", Filter: "JobStatus == 1 && ProcId == 0"},
		{Func: AggMin, Arg: "RequestCpus", Filter: "JobStatus == 1 && ProcId == 0"},
	}
	var stats collections.ScanStats
	rows, err := a.AggregateColsStats("true", groupCols, aggs, &stats)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Tier != collections.TierColumnar {
		t.Fatalf("answered by %v, not the columns", stats.Tier)
	}
	byGroup := aggRowMap(t, rows)
	if len(byGroup) != 10 {
		t.Fatalf("got %d groups, want all 10 ProcIds: a filter dropped groups it must only narrow", len(byGroup))
	}
	for g, v := range byGroup {
		if g == "0" {
			continue
		}
		if v[1] != "0" {
			t.Errorf("ProcId %s: filtered count %q, want 0", g, v[1])
		}
		if v[2] != "undefined" {
			t.Errorf("ProcId %s: MIN over no rows = %q, want undefined", g, v[2])
		}
	}
	// And the group that does pass is not zero, or the filter matched nothing anywhere and the
	// check above is vacuous.
	if byGroup["0"][1] == "0" {
		t.Fatal("ProcId 0 passed no rows either: the filter matches nothing, so this proves nothing")
	}
}

// TestFilteredAggregateAcrossSegments covers a filter whose attribute is MISSING from some
// segments' schemas, which sends those segments to the row fallback while the rest stay columnar.
// The two have to agree on what the filter means or the answer depends on where a record landed.
func TestFilteredAggregateAcrossSegments(t *testing.T) {
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{SegmentSize: 1 << 14})
	if err != nil {
		t.Fatal(err)
	}
	// The first half carries Phase, the second does not: a filter on Phase can be narrowed in the
	// segments that have the column and must be evaluated per record in the ones that do not.
	for i := 0; i < 4000; i++ {
		rec := fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = %d\nRequestCpus = %d\n",
			i, i%8, 1+i%5, 1+i%4)
		if i < 2000 {
			rec += fmt.Sprintf("Phase = %d\n", i%3)
		}
		if err := a.AppendOld(rec); err != nil {
			t.Fatal(err)
		}
	}
	if !a.BuildAndEnableSchemaScan(1000, 8) {
		t.Skip("no sealed segments to accelerate")
	}
	aggs := []AggSpec{
		{Func: AggCount, Arg: "*"},
		{Func: AggCount, Arg: "*", Filter: "Phase == 1"},
		{Func: AggSum, Arg: "RequestCpus", Filter: "Phase == 1"},
	}
	sameAsScan(t, a, "true", []GroupCol{{Attr: "ProcId"}}, aggs)
	sameAsScan(t, a, "JobStatus == 4", []GroupCol{{Attr: "ProcId"}}, aggs)
}

// BenchmarkFilteredGroupAggregate is the before/after: the "b" arm is literally what this request
// did before filters were served, because declining sent it to exactly this scan. Both arms run
// over one fixture so the comparison is the code path and not two archives (see the per-arm store
// variance note in the group benchmarks).
func BenchmarkFilteredGroupAggregate(b *testing.B) {
	cat, a := archiveCountFixture(b, 50000)
	defer cat.Close()
	groupCols := []GroupCol{{Attr: "ProcId"}}
	aggs := []AggSpec{
		{Func: AggCount, Arg: "*"},
		{Func: AggCount, Arg: "*", Filter: "JobStatus == 4"},
		{Func: AggSum, Arg: "RequestCpus", Filter: "JobStatus == 4"},
	}
	b.Run("columnar", func(b *testing.B) {
		for b.Loop() {
			if _, err := a.AggregateCols("true", groupCols, aggs); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("scan", func(b *testing.B) {
		for b.Loop() {
			scanAggregate(b, a, "true", groupCols, aggs)
		}
	})
}
