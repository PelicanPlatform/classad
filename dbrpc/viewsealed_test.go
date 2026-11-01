package dbrpc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// A continuous aggregate keeps its rows in two places. Sealing moves a closed bucket out of
// the live backing and into the view's archive, so a read that consults only the backing is
// not stale -- it silently returns the unsealed tail and nothing older. For a five-minute
// bucket the seal cadence is 150 seconds, so that is very nearly the whole series.
//
// opQuery has always unioned the sealed half. Nothing else did, and the ops the SQL layer
// actually uses are the other ones, so every projected read of a continuous aggregate was
// short by however much had sealed. These pin each read path against the same store, before
// and after a seal, so a path that forgets the sealed half fails by name.
type sealedFixture struct {
	s    *Server
	c    *Client
	cat  *db.Catalog
	view *db.View
}

const (
	sealBase    = "events"
	sealView    = "events_ca"
	sealWidth   = 300 // five-minute buckets
	sealBuckets = 12
	sealPerRow  = 2 // base rows per bucket
	sealT0      = 1700000000
)

// newSealedFixture builds a continuous aggregate over sealBuckets buckets of base rows. The
// catalog is on disk because an in-memory one has no archive, and a view with no archive
// never seals -- which is exactly the configuration that would hide this bug.
func newSealedFixture(t *testing.T) *sealedFixture {
	t.Helper()
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base, err := cat.CreateTable(sealBase)
	if err != nil {
		t.Fatal(err)
	}
	for b := 0; b < sealBuckets; b++ {
		for i := 0; i < sealPerRow; i++ {
			ad := classad.New()
			ad.InsertAttr("Stamp", int64(sealT0+b*sealWidth+i))
			ad.InsertAttr("Size", int64(10+b))
			if werr := base.Put(fmt.Sprintf("b%d_%d", b, i), ad); werr != nil {
				t.Fatal(werr)
			}
		}
	}
	spec := db.ViewSpec{
		BaseTable: sealBase,
		Groups:    []db.ViewGroupCol{{Attr: "Stamp", Alias: "time", BucketWidth: sealWidth}},
		Metrics: []db.ViewMetric{
			{Func: db.ViewCount, Arg: "*", Alias: "metric_events"},
			{Func: db.ViewSum, Arg: "Size", Alias: "metric_size"},
		},
		Cardinality: 1000,
	}
	if cerr := cat.CreateView(sealView, spec); cerr != nil {
		t.Fatal(cerr)
	}
	v, ok := cat.View(sealView)
	if !ok {
		t.Fatal("view missing right after creation")
	}
	// A view materializes on its own goroutine, so a read issued immediately can see a
	// partly-built series and would make this test flaky in the direction of passing.
	deadline := time.Now().Add(5 * time.Second)
	for v.SeriesCount() != sealBuckets {
		if time.Now().After(deadline) {
			t.Fatalf("view materialized %d series, want %d", v.SeriesCount(), sealBuckets)
		}
		time.Sleep(10 * time.Millisecond)
	}
	f := &sealedFixture{s: NewServerCatalog(cat), cat: cat, view: v}
	cconn, sconn := netPipe()
	go func() { _ = f.s.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true, Privileged: true}) }()
	f.c = NewClient(cconn)
	return f
}

func (f *sealedFixture) close() { f.c.Close(); f.s.Close(); f.cat.Close() }

// sealAll seals every bucket, which is what the daemon's tick does on its own within half a
// bucket width. Forcing it makes the race deterministic.
func (f *sealedFixture) sealAll() {
	f.view.Seal(int64(sealT0 + (sealBuckets+10)*sealWidth))
}

func TestViewSealedHistoryIsReadable(t *testing.T) {
	ctx := context.Background()

	// Each case reads every row of the view by a different path and reports how many it got.
	// They must all agree, before the seal and after it.
	paths := []struct {
		name string
		read func(t *testing.T, f *sealedFixture) int
	}{
		{"opQuery", func(t *testing.T, f *sealedFixture) int {
			rows, err := f.c.QueryTable(ctx, sealView, "true", 0)
			if err != nil {
				t.Fatal(err)
			}
			return len(rows)
		}},
		{"opQueryRawProj", func(t *testing.T, f *sealedFixture) int {
			rows, err := f.c.QueryRawProject(ctx, sealView, "true", []string{"time", "metric_events"}, 0)
			if err != nil {
				t.Fatal(err)
			}
			return len(rows)
		}},
		{"opQueryRawProjRefs", func(t *testing.T, f *sealedFixture) int {
			rows, err := f.c.QueryRawProjectRefs(ctx, sealView, "true", []string{"time", "metric_events"}, 0)
			if err != nil {
				t.Fatal(err)
			}
			return len(rows)
		}},
	}

	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			f := newSealedFixture(t)
			defer f.close()

			before := p.read(t, f)
			if before != sealBuckets {
				t.Fatalf("before sealing: %d rows, want %d (the view did not materialize)", before, sealBuckets)
			}
			f.sealAll()
			after := p.read(t, f)
			if after != sealBuckets {
				t.Errorf("after sealing: %d rows, want %d.\n"+
					"Sealing moves closed buckets into the view's archive; this path reads only the "+
					"live backing, so it silently drops every sealed bucket.", after, sealBuckets)
			}
		})
	}
}

// A limit must bound the union, not each half: a read of a sealed view with a limit below
// the row count should still fill the limit rather than return only what the live half holds.
func TestViewSealedRespectsLimitAcrossHalves(t *testing.T) {
	f := newSealedFixture(t)
	defer f.close()
	f.sealAll()

	const limit = 5
	rows, err := f.c.QueryRawProject(context.Background(), sealView, "true",
		[]string{"time", "metric_events"}, limit)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != limit {
		t.Errorf("limit %d returned %d rows; the limit must bound the union of both halves", limit, len(rows))
	}
}

// A top-K over a sealed view must be the k best of the UNION. The fixture's buckets ascend
// with time and metric_size ascends with the bucket, so the largest sizes are in the OLDEST
// buckets only if the data says so -- here they are in the newest, which seal first. That is
// the case that catches a top-K served from the live backing alone: it would return the
// unsealed tail's best, which are not the view's best at all.
func TestViewSealedTopKSpansBothHalves(t *testing.T) {
	ctx := context.Background()
	f := newSealedFixture(t)
	defer f.close()

	const k = 3
	want, err := f.c.TopK(ctx, sealView, "true", []string{"time", "metric_size"}, "metric_size", true, k)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != k {
		t.Fatalf("before sealing: top-%d returned %d rows", k, len(want))
	}

	f.sealAll()

	got, err := f.c.TopK(ctx, sealView, "true", []string{"time", "metric_size"}, "metric_size", true, k)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("after sealing: top-%d returned %d rows, want %d", k, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("after sealing: top-%d row %d = %q, want %q.\n"+
				"The k best of the union are not the k best of the live backing; sealing must "+
				"not change which rows a top-K returns.", k, i, got[i], want[i])
		}
	}
}

// An aggregate over a sealed view must reduce the union. AVG is the case that matters most:
// it is the one a per-half merge gets WRONG rather than merely short, because an average of
// two averages is not the average unless the halves happen to be the same size -- and the
// halves here are not, since sealing moves buckets across one at a time. MIN and MAX would
// survive a naive merge, so they are here to show the union is not merely non-empty.
func TestViewSealedAggregatesReduceTheUnion(t *testing.T) {
	ctx := context.Background()
	f := newSealedFixture(t)
	defer f.close()

	aggs := []AggSpec{
		{Func: AggCount, Arg: "*"},
		{Func: AggSum, Arg: "metric_size"},
		{Func: AggAvg, Arg: "metric_size"},
		{Func: AggMin, Arg: "metric_size"},
		{Func: AggMax, Arg: "metric_size"},
	}
	read := func() []AggRow {
		t.Helper()
		rows, err := f.c.AggregateTable(ctx, sealView, "true", nil, aggs)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}

	before := read()
	if len(before) != 1 || len(before[0].Values) != len(aggs) {
		t.Fatalf("before sealing: got %d rows", len(before))
	}
	f.sealAll()
	after := read()
	if len(after) != len(before) {
		t.Fatalf("after sealing: %d aggregate rows, want %d", len(after), len(before))
	}
	names := []string{"COUNT(*)", "SUM", "AVG", "MIN", "MAX"}
	for i := range before[0].Values {
		if after[0].Values[i] != before[0].Values[i] {
			t.Errorf("after sealing: %s = %s, want %s.\n"+
				"Sealing moves buckets into the view's archive; an aggregate must reduce both "+
				"halves, and AVG in particular cannot be recombined from per-half results.",
				names[i], after[0].Values[i], before[0].Values[i])
		}
	}
}

// Grouping must also span the halves: a GROUP BY whose groups straddle the seal boundary
// must return one row per group, not one per group per half.
func TestViewSealedGroupedAggregateDoesNotSplitGroups(t *testing.T) {
	ctx := context.Background()
	f := newSealedFixture(t)
	defer f.close()

	aggs := []AggSpec{{Func: AggCount, Arg: "*"}}
	groups := []string{"metric_events"} // every bucket has the same event count, so ONE group
	read := func() []AggRow {
		t.Helper()
		rows, err := f.c.AggregateTable(ctx, sealView, "true", groups, aggs)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}

	before := read()
	f.sealAll()
	after := read()
	if len(after) != len(before) {
		t.Fatalf("after sealing: %d groups, want %d (a group split across the halves)", len(after), len(before))
	}
	for i := range before {
		if after[i].Values[0] != before[i].Values[0] {
			t.Errorf("after sealing: group %v count = %s, want %s",
				after[i].Group, after[i].Values[0], before[i].Values[0])
		}
	}
}
