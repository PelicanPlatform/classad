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

// TestArchiveUnbucketedGroupStillDeclinesUnbucketable is the other half: WITHOUT a bucket, an absent
// or non-numeric group value still has to give the query back, because the scan renders it as its own
// group (`undefined`, or the string) and a column read cannot produce that group.
func TestArchiveUnbucketedGroupStillDeclinesUnbucketable(t *testing.T) {
	cat, a := bucketOddFixture(t)
	defer cat.Close()

	if _, ok := GroupedFromColumns(a, "true",
		[]GroupCol{{Attr: "CompletionDate"}},
		[]AggSpec{{Func: AggCount, Arg: "*"}}); ok {
		t.Fatal("unbucketed group answered from columns despite records it cannot render a group for; " +
			"those records would be silently missing from the result")
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
