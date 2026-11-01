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
