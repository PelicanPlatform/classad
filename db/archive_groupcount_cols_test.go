package db

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// A COUNT(*) GROUPED BY one numeric column reached none of the archive's fast paths: index-answering
// handles only the CATEGORICAL index (so a numeric group column misses it), the constrained-count fast
// path is gated on there being no grouping, and ColumnarAggregate has no attribute to aggregate for
// COUNT(*). So the grouped form of a query whose ungrouped form takes 38 ms took 8 s.

// scanAggregate is the reference: the same scan-and-reduce AggregateCols itself falls back to, so the
// comparison is against the path in production and not a second reimplementation of grouping.
func scanAggregate(tb testing.TB, a *ArchiveTable, constraint string, groupCols []GroupCol, aggs []AggSpec) []AggRow {
	tb.Helper()
	attrs, groupCol, aggCol := AggProjection(groupCols, aggs)
	// The scan path parses the constraint, and an EMPTY one is not parsable even though the aggregate
	// treats it as match-all (IsMatchAll). Spell it as the literal the parser accepts so the reference
	// covers the same records the caller meant.
	if constraint == "" {
		constraint = "true"
	}
	seq, err := a.QueryProject(constraint, attrs)
	if err != nil {
		tb.Fatal(err)
	}
	rows, err := AggregateValues(seq, attrs, groupCols, aggs, groupCol, aggCol, nil)
	if err != nil {
		tb.Fatal(err)
	}
	return rows
}

// rowMap flattens grouped rows to group-text -> value so the two paths can be compared without
// depending on row order (the scan path emits first-seen order, the columnar path sorted).
func rowMap(tb testing.TB, rows []AggRow) map[string]string {
	tb.Helper()
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		if len(r.Group) != 1 || len(r.Values) != 1 {
			tb.Fatalf("unexpected row shape %+v", r)
		}
		if prev, dup := out[r.Group[0]]; dup {
			tb.Errorf("group %q appears twice (%s then %s): one group was split into two rows",
				r.Group[0], prev, r.Values[0])
		}
		out[r.Group[0]] = r.Values[0]
	}
	return out
}

func TestArchiveGroupedCountMatchesScan(t *testing.T) {
	cat, a := archiveCountFixture(t, 5000)
	defer cat.Close()

	countStar := []AggSpec{{Func: AggCount, Arg: "*"}}
	for _, tc := range []struct{ constraint, group string }{
		{"RequestMemory > 4096", "ProcId"},
		{"RequestMemory > 4096 && RequestCpus >= 4", "JobStatus"},
		{"JobStatus == 4 && RequestMemory > 2048", "ProcId"},
		{"ClusterId >= 1000 && ClusterId < 1100", "RequestCpus"},
		{"RequestCpus >= 0", "RequestCpus"}, // group column is the predicate column
		{"ClusterId > 9999999", "ProcId"},   // nothing matches: no groups at all
		{`Owner == "user3"`, "ProcId"},      // string predicate: whichever tier serves it must agree
		{"RequestMemory > 4096", "Owner"},   // string GROUP column: must decline and still be right
		{"RequestMemory > RequestCpus", "ProcId"},
		{"true", "ProcId"}, // unconstrained: the index path's territory
	} {
		groupCols := []GroupCol{{Attr: tc.group}}
		got, err := a.AggregateCols(tc.constraint, groupCols, countStar)
		if err != nil {
			t.Fatalf("%s GROUP BY %s: %v", tc.constraint, tc.group, err)
		}
		want := scanAggregate(t, a, tc.constraint, groupCols, countStar)
		gotM, wantM := rowMap(t, got), rowMap(t, want)
		if len(gotM) != len(wantM) {
			t.Errorf("%s GROUP BY %s: %d groups, scan found %d", tc.constraint, tc.group, len(gotM), len(wantM))
		}
		for g, v := range wantM {
			if gotM[g] != v {
				t.Errorf("%s GROUP BY %s: group %q count %q, scan %q", tc.constraint, tc.group, g, gotM[g], v)
			}
		}
		for g := range gotM {
			if _, ok := wantM[g]; !ok {
				t.Errorf("%s GROUP BY %s: group %q is not in the scan's result at all",
					tc.constraint, tc.group, g)
			}
		}
		_, served := a.GroupCountConstraint(tc.constraint, tc.group)
		t.Logf("%-42s GROUP BY %-12s groups=%-4d columnar=%v", tc.constraint, tc.group, len(gotM), served)
	}
}

// TestArchiveGroupedCountServesTheReportedShape is the anti-vacuity check: the shape the user reported
// slow must actually be served by the column path. Without it every case above could pass by falling
// through to the scan, i.e. by changing nothing.
func TestArchiveGroupedCountServesTheReportedShape(t *testing.T) {
	cat, a := archiveCountFixture(t, 5000)
	defer cat.Close()
	if _, ok := a.GroupCountConstraint("RequestMemory > 4096", "ProcId"); !ok {
		t.Fatal("the reported shape (numeric predicate, numeric group column, COUNT(*)) is not served " +
			"columnar, so nothing about this change is exercised")
	}
	// A FILTER on the aggregate must not be answered from counts that know nothing about it.
	rows, err := a.AggregateCols("RequestMemory > 2048", []GroupCol{{Attr: "ProcId"}},
		[]AggSpec{{Func: AggCount, Arg: "*", Filter: "RequestCpus >= 4"}})
	if err != nil {
		t.Fatal(err)
	}
	want := scanAggregate(t, a, "RequestMemory > 2048", []GroupCol{{Attr: "ProcId"}},
		[]AggSpec{{Func: AggCount, Arg: "*", Filter: "RequestCpus >= 4"}})
	if got, wantM := rowMap(t, rows), rowMap(t, want); len(got) != len(wantM) {
		t.Errorf("filtered grouped count: %d groups, scan %d", len(got), len(wantM))
	} else {
		for g, v := range wantM {
			if got[g] != v {
				t.Errorf("filtered grouped count: group %q = %q, scan %q (the filter was ignored)", g, got[g], v)
			}
		}
	}
	// A second aggregate alongside the count is out of scope and must fall through, not be dropped.
	rows, err = a.AggregateCols("RequestMemory > 4096", []GroupCol{{Attr: "ProcId"}},
		[]AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggMax, Arg: "RequestCpus"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if len(r.Values) != 2 {
			t.Fatalf("grouped COUNT(*), MAX(RequestCpus) returned %d values per row: %+v", len(r.Values), r)
		}
	}
}

// TestArchiveUnconstrainedGroupedCount covers the shape with no WHERE at all. The index path answers
// that shape only for a CATEGORICALLY indexed group column, so `GROUP BY <numeric>` with no constraint
// fell to a record scan exactly like the constrained form -- and it cannot come through the predicate
// analysis, which has no probes to work with, so it is a separate entry point.
func TestArchiveUnconstrainedGroupedCount(t *testing.T) {
	cat, a := archiveCountFixture(t, 5000)
	defer cat.Close()
	countStar := []AggSpec{{Func: AggCount, Arg: "*"}}
	groupCols := []GroupCol{{Attr: "ProcId"}}
	if _, ok := a.GroupCountAll("ProcId"); !ok {
		t.Fatal("the whole-column histogram is not served, so this test exercises nothing")
	}
	// ProcId is not categorically indexed here, so the index path must be the one declining -- otherwise
	// this passes on a query that was already fast and says nothing about the new path.
	if _, ok := a.CategoricalGroupCounts("ProcId"); ok {
		t.Fatal("ProcId is index-answerable in this fixture, so it never reached the columnar path")
	}
	for _, constraint := range []string{"", "true", "1 == 1"} {
		got, err := a.AggregateCols(constraint, groupCols, countStar)
		if err != nil {
			t.Fatalf("%q: %v", constraint, err)
		}
		want := scanAggregate(t, a, constraint, groupCols, countStar)
		gotM, wantM := rowMap(t, got), rowMap(t, want)
		if len(gotM) != len(wantM) {
			t.Errorf("%q: %d groups, scan found %d", constraint, len(gotM), len(wantM))
		}
		total := 0
		for g, v := range wantM {
			if gotM[g] != v {
				t.Errorf("%q: group %q count %q, scan %q", constraint, g, gotM[g], v)
			}
			n, _ := strconv.Atoi(gotM[g])
			total += n
		}
		// With no constraint the groups have to account for every record in the archive.
		if total != a.Count() {
			t.Errorf("%q: groups sum to %d but the archive holds %d records", constraint, total, a.Count())
		}
	}
}

// TestArchiveGroupedCountRealAndIntSameGroup pins the divergence the storage layer cannot see: it keys a
// group by (bits, type), so an integer 3 and a real 3.0 are different keys, while the scan path keys by
// rendered text and both render "3". Unmerged, the same label would appear on two rows.
func TestArchiveGroupedCountRealAndIntSameGroup(t *testing.T) {
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5000; i++ {
		// Tally holds the same NUMBER written as a real in most records and as an integer in a few.
		// The proportion matters: a field's kind is the DOMINANT kind among its values, and a field
		// whose dominant kind covers too little of the sample is dropped from the schema altogether
		// (so an even int/real split declines instead of exercising this). Mostly-real with rare
		// integer literals keeps the field real and makes those few records escape their slot,
		// where they keep their own kind -- which is how one number comes back under two keys.
		tally := fmt.Sprintf("Tally = %d.0", i%4)
		if i%100 == 3 {
			tally = fmt.Sprintf("Tally = %d", i%4)
		}
		ad, err := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nRequestMemory = %d\n%s",
			i, 1024+(i%32)*512, tally))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	if !a.BuildAndEnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to accelerate")
	}
	groupCols := []GroupCol{{Attr: "Tally"}}
	countStar := []AggSpec{{Func: AggCount, Arg: "*"}}
	// The merge under test only runs on the columnar path, and only if that path really does hand back
	// int and real keys for the same number -- so check both, or this passes by never merging anything.
	raw, served := a.GroupCountConstraint("RequestMemory > 4096", "Tally")
	if !served {
		t.Fatal("mixed int/real column was not served columnar, so the merge is not exercised")
	}
	if len(raw) <= 4 {
		t.Fatalf("storage returned %d keys for 4 distinct numbers: it already merged int with real, so "+
			"this test no longer covers the case it was written for", len(raw))
	}
	got, err := a.AggregateCols("RequestMemory > 4096", groupCols, countStar)
	if err != nil {
		t.Fatal(err)
	}
	want := scanAggregate(t, a, "RequestMemory > 4096", groupCols, countStar)
	// rowMap fails on a duplicated group label, which is the failure this test is here to catch.
	gotM, wantM := rowMap(t, got), rowMap(t, want)
	labels := make([]string, 0, len(wantM))
	for g := range wantM {
		labels = append(labels, g)
	}
	sort.Strings(labels)
	for _, g := range labels {
		if gotM[g] != wantM[g] {
			t.Errorf("group %q: count %q, scan %q", g, gotM[g], wantM[g])
		}
	}
	if len(gotM) != len(wantM) {
		t.Errorf("%d groups, scan found %d (%v vs %v)", len(gotM), len(wantM), gotM, wantM)
	}
	t.Logf("mixed int/real groups: %v", gotM)
}

// BenchmarkArchiveGroupedCount is the point of the change: grouping should cost what counting costs.
func BenchmarkArchiveGroupedCount(b *testing.B) {
	cat, a := archiveCountFixture(b, 60000)
	defer cat.Close()
	groupCols := []GroupCol{{Attr: "ProcId"}}
	countStar := []AggSpec{{Func: AggCount, Arg: "*"}}
	if _, ok := a.GroupCountConstraint("RequestMemory > 4096", "ProcId"); !ok {
		b.Fatal("not served columnar")
	}
	b.Run("grouped_columnar", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := a.AggregateCols("RequestMemory > 4096", groupCols, countStar); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("grouped_scan", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			attrs, groupCol, aggCol := AggProjection(groupCols, countStar)
			seq, err := a.QueryProject("RequestMemory > 4096", attrs)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := AggregateValues(seq, attrs, groupCols, countStar, groupCol, aggCol, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("ungrouped_count", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := a.AggregateCols("RequestMemory > 4096", nil, countStar); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// groupAggFixture has what archiveCountFixture lacks for value aggregates: a REAL column, so SUM/AVG
// type promotion is exercised, and a column ABSENT from some records, so a group can exist (its group
// value is present) while its MIN/MAX over that column is undefined.
func groupAggFixture(tb testing.TB, n int) (*Catalog, *ArchiveTable) {
	tb.Helper()
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: tb.TempDir()})
	if err != nil {
		tb.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{SegmentSize: 1 << 16})
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < n; i++ {
		src := fmt.Sprintf("ClusterId = %d\nProcId = %d\nRequestMemory = %d\nRequestCpus = %d\n"+
			"Score = %d.%d\n", i, i%20, 1024+(i%32)*512, 1+i%8, i%50, i%7)
		// Runtime is missing from one whole group's records, so that group exists (its GROUP value is
		// present) while MIN/MAX over Runtime is undefined for it. It stays above the schema's 0.90
		// presence threshold -- below that the field is not in the schema at all and every case here
		// declines for a reason that has nothing to do with what is being tested.
		if i%20 != 4 {
			src += fmt.Sprintf("Runtime = %d\n", 60+i%900)
		}
		ad, err := classad.ParseOld(src)
		if err != nil {
			tb.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			tb.Fatal(err)
		}
	}
	if !a.BuildAndEnableSchemaScan(4000, 8) {
		tb.Skip("no sealed segments to accelerate")
	}
	return cat, a
}

func TestArchiveGroupedValueAggregatesMatchScan(t *testing.T) {
	cat, a := groupAggFixture(t, 6000)
	defer cat.Close()

	for _, tc := range []struct {
		constraint string
		group      string
		aggs       []AggSpec
		// wantServed says whether the COLUMNS must answer this shape. Asserting it per case is what
		// keeps the comparison from passing by quietly falling through to the scan for all of them --
		// and, for the cases that must decline, from passing because the decline is right for some
		// other reason than the one under test.
		wantServed bool
		why        string
	}{
		{"RequestMemory > 4096", "ProcId",
			[]AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggMax, Arg: "RequestCpus"}}, true, ""},
		{"RequestMemory > 4096", "ProcId",
			[]AggSpec{{Func: AggMin, Arg: "Runtime"}, {Func: AggMax, Arg: "Runtime"}}, true, ""},
		{"RequestMemory > 2048", "ProcId",
			[]AggSpec{{Func: AggSum, Arg: "Runtime"}, {Func: AggAvg, Arg: "Runtime"}}, true,
			"Runtime is an integer column, so its sum is exact in int64 and order cannot change it"},
		{"RequestMemory > 2048", "ProcId",
			[]AggSpec{{Func: AggSum, Arg: "Score"}, {Func: AggAvg, Arg: "Score"}}, true,
			"adding reals in a different order can round differently, which is permitted: no aggregate " +
				"promises an addition order"},
		{"RequestMemory > 2048", "RequestCpus",
			[]AggSpec{{Func: AggMin, Arg: "Score"}, {Func: AggMax, Arg: "Score"}}, true,
			"MIN/MAX over a real column is order-independent, so it stays served"},
		{"RequestMemory > 2048", "ProcId",
			[]AggSpec{{Func: AggCount, Arg: "Runtime"}, {Func: AggCount, Arg: "*"}}, true, ""},
		// Every served function at once, plus a repeated attribute.
		{"RequestCpus >= 4", "ProcId", []AggSpec{
			{Func: AggCount, Arg: "*"}, {Func: AggCount, Arg: "Runtime"},
			{Func: AggMin, Arg: "Runtime"}, {Func: AggMax, Arg: "Runtime"},
			{Func: AggSum, Arg: "Runtime"}, {Func: AggAvg, Arg: "Runtime"},
		}, true, ""},
		// Unconstrained, where the predicate analysis cannot serve it and match-all is asked for
		// explicitly instead.
		{"true", "ProcId", []AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggAvg, Arg: "Runtime"}}, true, ""},
	} {
		groupCols := []GroupCol{{Attr: tc.group}}
		got, err := a.AggregateCols(tc.constraint, groupCols, tc.aggs)
		if err != nil {
			t.Fatalf("%s GROUP BY %s: %v", tc.constraint, tc.group, err)
		}
		want := scanAggregate(t, a, tc.constraint, groupCols, tc.aggs)
		// Compare the whole row, aggregate values included: a formatting difference (int vs real, an
		// empty MIN) is as wrong as a numeric one and much easier to ship unnoticed. Only float
		// summation rounding is allowed to differ -- see sameAggValues.
		gotM, wantM := rowsByGroup(t, got), rowsByGroup(t, want)
		if len(gotM) != len(wantM) {
			t.Errorf("%s GROUP BY %s: %d groups, scan found %d", tc.constraint, tc.group, len(gotM), len(wantM))
		}
		rounding := 0
		for g, v := range wantM {
			if !sameAggValues(gotM[g], v) {
				t.Errorf("%s GROUP BY %s: group %q = %v, scan = %v", tc.constraint, tc.group, g, gotM[g], v)
				continue
			}
			if fmt.Sprint(gotM[g]) != fmt.Sprint(v) {
				rounding++
			}
		}
		if rounding > 0 {
			// Logged, not asserted: which groups round differently depends on the data and on the order
			// each path happens to walk, and nothing promises either. Visible here so a reader can see
			// the tolerance being used rather than wonder whether it is dead.
			t.Logf("%s GROUP BY %s: %d of %d groups differ only in float summation rounding",
				tc.constraint, tc.group, rounding, len(wantM))
		}
		if _, served := a.groupedFromColumns(tc.constraint, groupCols, tc.aggs); served != tc.wantServed {
			t.Errorf("%s GROUP BY %s %v: served columnar = %v, want %v (%s)",
				tc.constraint, tc.group, tc.aggs, served, tc.wantServed, tc.why)
		}
	}
	// One group's records all lack Runtime, so its MIN/MAX must come back undefined -- the case that
	// distinguishes "the group does not exist" from "the group exists with nothing to aggregate".
	rows, err := a.AggregateCols("RequestMemory > 2048", []GroupCol{{Attr: "ProcId"}},
		[]AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggMin, Arg: "Runtime"}})
	if err != nil {
		t.Fatal(err)
	}
	undef := 0
	for _, r := range rows {
		if r.Values[1] == "undefined" {
			undef++
			if r.Values[0] == "0" {
				t.Errorf("group %q has an undefined MIN and a zero count: it should not be a row at all",
					r.Group[0])
			}
		}
	}
	if undef != 1 {
		t.Errorf("%d groups have an undefined MIN(Runtime), want exactly 1 -- the fixture no longer "+
			"covers a group whose records all lack the aggregated attribute", undef)
	}
}

// sameAggValues compares two rows' aggregate values. Text must match exactly, EXCEPT that two texts
// which parse as floats agreeing to within a few ULP are accepted: adding a group's reals in column
// order rather than scan order can change the last digit, and no aggregate promises an addition order.
// Everything else -- an integer sum, a count, "undefined" for an empty MIN, int-vs-real rendering --
// still has to match character for character, which is where a real bug would show up.
func sameAggValues(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] == want[i] {
			continue
		}
		g, gerr := strconv.ParseFloat(got[i], 64)
		w, werr := strconv.ParseFloat(want[i], 64)
		if gerr != nil || werr != nil {
			return false
		}
		// Both are finite floats that render differently: accept a relative difference at the scale
		// float64 accumulation can introduce, and nothing larger.
		scale := math.Max(math.Abs(g), math.Abs(w))
		if math.Abs(g-w) > 1e-12*scale {
			return false
		}
	}
	return true
}

// rowsByGroup keys whole rows (all aggregate values) by group label.
func rowsByGroup(tb testing.TB, rows []AggRow) map[string][]string {
	tb.Helper()
	out := make(map[string][]string, len(rows))
	for _, r := range rows {
		if len(r.Group) != 1 {
			tb.Fatalf("unexpected row shape %+v", r)
		}
		if _, dup := out[r.Group[0]]; dup {
			tb.Errorf("group %q appears twice: one group was split into two rows", r.Group[0])
		}
		out[r.Group[0]] = r.Values
	}
	return out
}

// TestArchiveGroupedAggregateRefusals pins what must NOT be served from the columns, each for a reason
// that would make the answer wrong rather than merely slow.
func TestArchiveGroupedAggregateRefusals(t *testing.T) {
	cat, a := groupAggFixture(t, 3000)
	defer cat.Close()
	for _, tc := range []struct {
		why        string
		constraint string
		groupCols  []GroupCol
		aggs       []AggSpec
	}{
		{"a FILTER is invisible to the columnar pass", "RequestMemory > 2048",
			[]GroupCol{{Attr: "ProcId"}}, []AggSpec{{Func: AggCount, Arg: "*", Filter: "RequestCpus >= 4"}}},
		{"COUNT(DISTINCT) needs the values, not their aggregate", "RequestMemory > 2048",
			[]GroupCol{{Attr: "ProcId"}}, []AggSpec{{Func: AggCountDistinct, Arg: "RequestCpus"}}},
		{"a bucketed group column is not this shape", "RequestMemory > 2048",
			[]GroupCol{{Attr: "ClusterId", BucketWidth: 100}}, []AggSpec{{Func: AggCount, Arg: "*"}}},
		{"two group columns", "RequestMemory > 2048",
			[]GroupCol{{Attr: "ProcId"}, {Attr: "RequestCpus"}}, []AggSpec{{Func: AggCount, Arg: "*"}}},
		{"aggregating a string column", "RequestMemory > 2048",
			[]GroupCol{{Attr: "ProcId"}}, []AggSpec{{Func: AggMax, Arg: "Owner"}}},
	} {
		if _, ok := a.groupedFromColumns(tc.constraint, tc.groupCols, tc.aggs); ok {
			t.Errorf("served a shape it must decline: %s", tc.why)
		}
		// Declining is only acceptable because the fallback is right: check the answer too.
		got, err := a.AggregateCols(tc.constraint, tc.groupCols, tc.aggs)
		if err != nil {
			t.Fatalf("%s: %v", tc.why, err)
		}
		want := scanAggregate(t, a, tc.constraint, tc.groupCols, tc.aggs)
		if len(got) != len(want) {
			t.Errorf("%s: %d rows, scan %d", tc.why, len(got), len(want))
		}
	}
}

// BenchmarkArchiveGroupedValueAggregates measures the shape a dashboard asks: a per-group count with a
// value aggregate beside it.
func BenchmarkArchiveGroupedValueAggregates(b *testing.B) {
	cat, a := groupAggFixture(b, 60000)
	defer cat.Close()
	groupCols := []GroupCol{{Attr: "ProcId"}}
	aggs := []AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggMin, Arg: "Runtime"},
		{Func: AggMax, Arg: "Runtime"}, {Func: AggAvg, Arg: "Runtime"}}
	if _, ok := a.groupedFromColumns("RequestMemory > 4096", groupCols, aggs); !ok {
		b.Fatal("not served columnar")
	}
	b.Run("columnar", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := a.AggregateCols("RequestMemory > 4096", groupCols, aggs); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("scan", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			attrs, groupCol, aggCol := AggProjection(groupCols, aggs)
			seq, err := a.QueryProject("RequestMemory > 4096", attrs)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := AggregateValues(seq, attrs, groupCols, aggs, groupCol, aggCol, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
}
