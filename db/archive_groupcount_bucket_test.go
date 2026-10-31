package db

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// A GROUP BY whose single column is floored into fixed-width buckets -- "how many per day" -- used to
// be declined outright by GroupedFromColumns, so every time series fell to a record scan while its
// ungrouped sibling was served from the columns. These tests pin both halves of the fix: the numbers
// still match the scan, and the fast path is the one that produces them.

// bucketRows runs a bucketed aggregate through the normal dispatch and returns group-text -> values.
func bucketRows(tb testing.TB, rows []AggRow, nAgg int) map[string][]string {
	tb.Helper()
	out := make(map[string][]string, len(rows))
	for _, r := range rows {
		if len(r.Group) != 1 || len(r.Values) != nAgg {
			tb.Fatalf("unexpected row shape %+v", r)
		}
		if prev, dup := out[r.Group[0]]; dup {
			tb.Errorf("bucket %q appears twice (%v then %v): one bucket was split into two rows",
				r.Group[0], prev, r.Values[0])
		}
		out[r.Group[0]] = r.Values
	}
	return out
}

func TestArchiveBucketedGroupMatchesScan(t *testing.T) {
	cat, a := archiveCountFixture(t, 5000)
	defer cat.Close()

	for _, tc := range []struct {
		constraint string
		group      string
		width      int64
		aggs       []AggSpec
	}{
		// The dashboard's own shape: completions per day over a window.
		{"CompletionDate >= 1700001000", "CompletionDate", 86400, []AggSpec{{Func: AggCount, Arg: "*"}}},
		// A width that does not divide the range evenly, so a partial last bucket is covered.
		{"CompletionDate >= 1700001000", "CompletionDate", 7000, []AggSpec{{Func: AggCount, Arg: "*"}}},
		// Match-all takes the GroupStatsAll path rather than the predicate one.
		{"true", "CompletionDate", 3600, []AggSpec{{Func: AggCount, Arg: "*"}}},
		// Aggregates beyond COUNT(*), since bucketing must not disturb them.
		{"RequestCpus >= 4", "CompletionDate", 86400, []AggSpec{
			{Func: AggCount, Arg: "*"}, {Func: AggSum, Arg: "RequestMemory"},
			{Func: AggMin, Arg: "ClusterId"}, {Func: AggMax, Arg: "ClusterId"},
			{Func: AggAvg, Arg: "RequestMemory"},
		}},
		// Bucketing a column that is not a timestamp at all: the floor is arithmetic, not calendar.
		{"ClusterId >= 1000 && ClusterId < 4000", "ClusterId", 250, []AggSpec{{Func: AggCount, Arg: "*"}}},
		// A bucket of 1 must agree with the raw grouping it degenerates to.
		{"ClusterId < 50", "ClusterId", 1, []AggSpec{{Func: AggCount, Arg: "*"}}},
		// Nothing matches: no buckets at all, not a zero bucket.
		{"CompletionDate > 1800000000", "CompletionDate", 86400, []AggSpec{{Func: AggCount, Arg: "*"}}},
		// A string predicate, which is served by the vector tier rather than predicate analysis --
		// both must bucket identically or group identity depends on which tier answered.
		{`Owner == "user3"`, "CompletionDate", 86400, []AggSpec{{Func: AggCount, Arg: "*"}}},
	} {
		name := fmt.Sprintf("%s/%s/%d", tc.constraint, tc.group, tc.width)
		t.Run(name, func(t *testing.T) {
			groupCols := []GroupCol{{Attr: tc.group, BucketWidth: tc.width}}
			got, err := a.AggregateCols(tc.constraint, groupCols, tc.aggs)
			if err != nil {
				t.Fatal(err)
			}
			want := scanAggregate(t, a, tc.constraint, groupCols, tc.aggs)
			gotM := bucketRows(t, got, len(tc.aggs))
			wantM := bucketRows(t, want, len(tc.aggs))
			if len(gotM) != len(wantM) {
				t.Fatalf("got %d buckets, scan says %d", len(gotM), len(wantM))
			}
			for k, wv := range wantM {
				gv, ok := gotM[k]
				if !ok {
					t.Errorf("bucket %s missing; scan has %v", k, wv)
					continue
				}
				for i := range wv {
					if gv[i] != wv[i] {
						t.Errorf("bucket %s agg %d = %s, scan says %s", k, i, gv[i], wv[i])
					}
				}
			}
		})
	}
}

// TestArchiveBucketedGroupServedFromColumns pins that the columnar path ANSWERS the bucketed query.
//
// The test above compares numbers, and both paths get those right -- that is the point of the fix --
// so it cannot tell a pushdown from a silent regression back to the scan it was meant to replace. This
// asks GroupedFromColumns directly and requires it not to decline.
func TestArchiveBucketedGroupServedFromColumns(t *testing.T) {
	cat, a := archiveCountFixture(t, 5000)
	defer cat.Close()

	for _, constraint := range []string{"CompletionDate >= 1700001000", "true"} {
		rows, ok := GroupedFromColumns(a, constraint,
			[]GroupCol{{Attr: "CompletionDate", BucketWidth: 86400}},
			[]AggSpec{{Func: AggCount, Arg: "*"}})
		if !ok {
			t.Fatalf("%s: columnar path declined the bucketed group, so this is a record scan again", constraint)
		}
		if len(rows) == 0 {
			t.Fatalf("%s: columnar path answered with no buckets", constraint)
		}
	}
}

// bucketOddFixture is an archive where the bucketed attribute is missing from some records and a
// string in others -- the two cases where the bucketed and unbucketed forms must behave DIFFERENTLY.
func bucketOddFixture(tb testing.TB) (*Catalog, *ArchiveTable) {
	tb.Helper()
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: tb.TempDir()})
	if err != nil {
		tb.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{SegmentSize: 1 << 16})
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < 4000; i++ {
		var src string
		switch {
		case i%50 == 7: // absent
			src = fmt.Sprintf("ClusterId = %d\nJobStatus = 4\n", i)
		case i%50 == 11: // present but not a number
			src = fmt.Sprintf("ClusterId = %d\nJobStatus = 4\nCompletionDate = \"never\"\n", i)
		case i%50 == 23: // a boolean, which the columnar layer calls a number and the reference does not
			src = fmt.Sprintf("ClusterId = %d\nJobStatus = 4\nCompletionDate = true\n", i)
		default:
			src = fmt.Sprintf("ClusterId = %d\nJobStatus = 4\nCompletionDate = %d\n", i, 1700000000+i*60)
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

// TestArchiveBucketedGroupDropsUnbucketable is the semantic the fix deliberately changed.
//
// A record whose group value is absent or not a number has no bucket to be floored into, and the
// scanning aggregator drops it from the series. The columnar path must drop it too -- the alternative
// considered, declining, would hand back exactly the queries worth accelerating, since a real history
// archive always has some record missing the attribute.
func TestArchiveBucketedGroupDropsUnbucketable(t *testing.T) {
	cat, a := bucketOddFixture(t)
	defer cat.Close()

	groupCols := []GroupCol{{Attr: "CompletionDate", BucketWidth: 3600}}
	aggs := []AggSpec{{Func: AggCount, Arg: "*"}}

	rows, ok := GroupedFromColumns(a, "true", groupCols, aggs)
	if !ok {
		t.Fatal("columnar path declined a bucketed group over an archive with unbucketable records")
	}
	got := bucketRows(t, rows, 1)
	want := bucketRows(t, scanAggregate(t, a, "true", groupCols, aggs), 1)
	if len(got) != len(want) {
		t.Fatalf("got %d buckets, scan says %d", len(got), len(want))
	}
	var total int
	for k, wv := range want {
		if got[k] == nil {
			t.Errorf("bucket %s missing", k)
			continue
		}
		if got[k][0] != wv[0] {
			t.Errorf("bucket %s = %s, scan says %s", k, got[k][0], wv[0])
		}
		n, err := strconv.Atoi(wv[0])
		if err != nil {
			t.Fatalf("non-numeric count %q", wv[0])
		}
		total += n
	}
	// 240 of the 4000 records are unbucketable: one absent, one string and one boolean in every 50. Checking the
	// total against the fixture rather than only against the scan is what catches both paths agreeing
	// on a wrong answer -- if each counted an unbucketable record into a floored-zero bucket, they
	// would still match each other.
	const unbucketable = 4000 / 50 * 3
	if total != 4000-unbucketable {
		t.Errorf("buckets hold %d records, want %d (the %d unbucketable ones must be dropped)",
			total, 4000-unbucketable, unbucketable)
	}
}

// TestArchiveUnbucketedGroupKeepsUnbucketable is the other half, and the contrast is the point:
// WITHOUT a bucket the same records are GROUPED rather than dropped.
//
// They have no bucket to be floored into, but they do have a value to be grouped by -- `undefined`
// for an absent attribute, the text for a string, true/false for a boolean -- and the columnar reader
// renders all three. So the unbucketed form keeps every record the bucketed form drops, and the two
// totals differ by exactly the unbucketable count.
//
// This test previously asserted the opposite, that the unbucketed form must DECLINE here. That was
// true when a column read could not render those values; it is a limitation rather than a
// correctness rule, and it no longer holds.
func TestArchiveUnbucketedGroupKeepsUnbucketable(t *testing.T) {
	cat, a := bucketOddFixture(t)
	defer cat.Close()

	groupCols := []GroupCol{{Attr: "CompletionDate"}}
	aggs := []AggSpec{{Func: AggCount, Arg: "*"}}

	rows, ok := GroupedFromColumns(a, "true", groupCols, aggs)
	if !ok {
		t.Fatal("columnar path declined an unbucketed group over renderable non-numeric values")
	}
	got := bucketRows(t, rows, 1)
	want := bucketRows(t, scanAggregate(t, a, "true", groupCols, aggs), 1)
	if len(got) != len(want) {
		t.Fatalf("got %d groups, scan says %d", len(got), len(want))
	}
	var total int
	for k, wv := range want {
		if got[k] == nil {
			t.Errorf("group %s missing; scan has %v", k, wv)
			continue
		}
		if got[k][0] != wv[0] {
			t.Errorf("group %s = %s, scan says %s", k, got[k][0], wv[0])
		}
		n, err := strconv.Atoi(wv[0])
		if err != nil {
			t.Fatalf("non-numeric count %q", wv[0])
		}
		total += n
	}
	// Every record is in some group, unlike the bucketed form above which drops 240 of them.
	if total != 4000 {
		t.Errorf("groups hold %d records, want all 4000", total)
	}
}

// TestArchiveUnbucketedGroupStillDeclinesExpressions is what survives of the rule above. An
// EXPRESSION whose value depends on the clock is not a value a column read can render -- only
// evaluating it at query time produces one -- and the scan does make a group out of it, so serving the
// query would return a result with a group missing. Unlike absent or string values, this declines
// whether or not a bucket was asked for. (An expression over the record alone is computed when its
// segment is columnarized, and is served; see TestComputedValuesMatchBruteForce.)
func TestArchiveUnbucketedGroupStillDeclinesExpressions(t *testing.T) {
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4000; i++ {
		src := fmt.Sprintf("ClusterId = %d\nJobStatus = 4\nCompletionDate = %d\n", i, 1700000000+i*60)
		if i%50 == 7 {
			src = fmt.Sprintf("ClusterId = %d\nJobStatus = 4\nCompletionDate = ClusterId + 1 + time() * 0\n", i)
		}
		ad, perr := classad.ParseOld(src)
		if perr != nil {
			t.Fatal(perr)
		}
		if aerr := a.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	if !a.BuildAndEnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to accelerate")
	}

	for _, width := range []int64{0, 3600} {
		if _, ok := GroupedFromColumns(a, "true",
			[]GroupCol{{Attr: "CompletionDate", BucketWidth: width}},
			[]AggSpec{{Func: AggCount, Arg: "*"}}); ok {
			t.Errorf("bucket width %d: served a group column holding an expression; the scan makes a "+
				"group out of it, so that group would be silently missing", width)
		}
	}
}

// TestArchiveBucketedGroupRealColumn pins that a real-valued column buckets to the same INTEGER keys an
// int column would, which is what lets the scan and the columnar path agree on a bucket's name.
func TestArchiveBucketedGroupRealColumn(t *testing.T) {
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4000; i++ {
		ad, err := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nWallTime = %d.5\n", i, i))
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

	groupCols := []GroupCol{{Attr: "WallTime", BucketWidth: 100}}
	aggs := []AggSpec{{Func: AggCount, Arg: "*"}}
	rows, ok := GroupedFromColumns(a, "true", groupCols, aggs)
	if !ok {
		t.Fatal("columnar path declined a bucketed group over a real column")
	}
	got := bucketRows(t, rows, 1)
	want := bucketRows(t, scanAggregate(t, a, "true", groupCols, aggs), 1)
	if len(got) != len(want) {
		t.Fatalf("got %d buckets, scan says %d", len(got), len(want))
	}
	for k, wv := range want {
		if _, err := strconv.ParseInt(k, 10, 64); err != nil {
			t.Errorf("bucket key %q is not an integer; a bucket start must render the same whatever the column's type", k)
		}
		if got[k] == nil || got[k][0] != wv[0] {
			t.Errorf("bucket %s = %v, scan says %s", k, got[k], wv[0])
		}
	}
}

// TestMutableBucketedGroupMatchesScan covers the other table kind. A mutable table reaches the same
// GroupedFromColumns through *DB, so relaxing the guard gave `jobs` the bucketed pushdown too -- and
// the live queue is where "submitted per day" for work that has not finished yet has to come from.
func TestMutableBucketedGroupMatchesScan(t *testing.T) {
	d := mutableGroupFixture(t, 5000)

	for _, tc := range []struct {
		constraint string
		width      int64
	}{
		{"ClusterId >= 1000", 250},
		{"true", 1000},
		{"RequestCpus >= 4", 500},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.constraint, tc.width), func(t *testing.T) {
			groupCols := []GroupCol{{Attr: "ClusterId", BucketWidth: tc.width}}
			aggs := []AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggSum, Arg: "RequestMemory"}}

			rows, ok := GroupedFromColumns(d, tc.constraint, groupCols, aggs)
			if !ok {
				t.Fatalf("columnar path declined a bucketed group on a mutable table")
			}
			got := bucketRows(t, rows, len(aggs))
			want := bucketRows(t, scanGroupedMutable(t, d, tc.constraint, groupCols, aggs), len(aggs))
			if len(got) != len(want) {
				t.Fatalf("got %d buckets, scan says %d", len(got), len(want))
			}
			for k, wv := range want {
				gv, ok := got[k]
				if !ok {
					t.Errorf("bucket %s missing; scan has %v", k, wv)
					continue
				}
				for i := range wv {
					if gv[i] != wv[i] {
						t.Errorf("bucket %s agg %d = %s, scan says %s", k, i, gv[i], wv[i])
					}
				}
			}
		})
	}
}

// mixedStringColumnFixture is a column the schema types as a STRING because strings dominate it,
// but which also carries records holding a NUMBER. That is the case a column-typed-by-its-majority
// always has in real data, and the case a homogeneous fixture never produces.
func mixedStringColumnFixture(tb testing.TB, n int) (*Catalog, *ArchiveTable) {
	tb.Helper()
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: tb.TempDir()})
	if err != nil {
		tb.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{SegmentSize: 1 << 15})
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < n; i++ {
		src := fmt.Sprintf("ClusterId = %d\nFoo = \"name-%d\"\n", i, i%23)
		if i%17 == 0 {
			src = fmt.Sprintf("ClusterId = %d\nFoo = %d\n", i, 1700000000+i*600)
		}
		ad, perr := classad.ParseOld(src)
		if perr != nil {
			tb.Fatal(perr)
		}
		if aerr := a.Append(ad); aerr != nil {
			tb.Fatal(aerr)
		}
	}
	if !a.BuildAndEnableSchemaScan(n, 8) {
		tb.Skip("no sealed segments to accelerate")
	}
	return cat, a
}

// TestArchiveBucketedGroupOnStringColumnKeepsNumbers is the regression for a wrong answer this
// path used to give with complete confidence.
//
// A bucket floors a number, and a string has none, so it was tempting to answer a bucketed group
// over a STRING COLUMN with "no groups at all". That is wrong, and the earlier version of this test
// asserted it -- passing only because its fixture's string column was homogeneous. A column is
// typed by what dominates it; the records whose value escaped that type still hold numbers, and
// those bucket normally. Skipping the column dropped them, leaving a plausible answer with most of
// its groups missing and no error anywhere.
//
// The assertion is equality with the scan, not a count, because the count is what made the old
// version look right.
func TestArchiveBucketedGroupOnStringColumnKeepsNumbers(t *testing.T) {
	cat, a := mixedStringColumnFixture(t, 3000)
	defer cat.Close()

	groupCols := []GroupCol{{Attr: "Foo", BucketWidth: 86400}}
	aggs := []AggSpec{{Func: AggCount, Arg: "*"}}

	rows, ok := GroupedFromColumns(a, "true", groupCols, aggs)
	if !ok {
		t.Fatal("columnar path declined a bucketed group on a string column")
	}
	got := bucketRows(t, rows, 1)
	want := bucketRows(t, scanAggregate(t, a, "true", groupCols, aggs), 1)
	if len(want) < 2 {
		t.Fatalf("fixture produced %d buckets; it cannot show the bug it exists for", len(want))
	}
	if len(got) != len(want) {
		t.Fatalf("columnar returned %d buckets, scan says %d", len(got), len(want))
	}
	var total int
	for k, wv := range want {
		if got[k] == nil {
			t.Errorf("bucket %s missing; scan has %v", k, wv)
			continue
		}
		if got[k][0] != wv[0] {
			t.Errorf("bucket %s = %s, scan says %s", k, got[k][0], wv[0])
		}
		n, err := strconv.Atoi(wv[0])
		if err != nil {
			t.Fatalf("non-numeric count %q", wv[0])
		}
		total += n
	}
	// The strings are dropped (no bucket to floor into) and the numbers are kept, so the groups
	// must hold exactly the records that carried a number.
	if wantRecords := (3000 + 16) / 17; total != wantRecords {
		t.Errorf("buckets hold %d records, want %d -- the numbers among the strings", total, wantRecords)
	}
}

// TestArchiveUnbucketedGroupOnStringColumnStillGroups is the control for the test above: without a
// bucket the same column must group normally, so the emptiness there is the bucket's doing and not a
// string column quietly failing to group at all.
func TestArchiveUnbucketedGroupOnStringColumnStillGroups(t *testing.T) {
	cat, a := archiveCountFixture(t, 5000)
	defer cat.Close()

	groupCols := []GroupCol{{Attr: "Owner"}}
	aggs := []AggSpec{{Func: AggCount, Arg: "*"}}

	rows, ok := GroupedFromColumns(a, "true", groupCols, aggs)
	if !ok {
		t.Fatal("columnar path declined an unbucketed group on a string column")
	}
	if len(rows) == 0 {
		t.Fatal("no groups at all from a string column without a bucket")
	}
	got := bucketRows(t, rows, 1)
	want := bucketRows(t, scanAggregate(t, a, "true", groupCols, aggs), 1)
	if len(got) != len(want) {
		t.Fatalf("got %d groups, scan says %d", len(got), len(want))
	}
	for k, wv := range want {
		if got[k] == nil || got[k][0] != wv[0] {
			t.Errorf("group %s = %v, scan says %s", k, got[k], wv[0])
		}
	}
}
