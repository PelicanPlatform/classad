package dbrpc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
)

// TestAggregateColdColumnFirstQuery explains a reported asymmetry: MAX over a large archive took
// ~60s on the first run and ~1.6s on the second, having taken ~5s before the columnar accelerator
// was enabled at all.
//
// A HOT column is read straight from each block's uncompressed prefix -- no decompression, no cache.
// A COLD column is not: reading it calls blockCache.streams, which decompresses ALL THREE of the
// block's streams and caches the result under a 256 MiB bound. So the first query over a cold column
// pays decompression of essentially the whole dataset, the second is served from cache, and a working
// set past 256 MiB thrashes and pays it every time.
//
// Which columns are hot is chosen by query read demand -- and until the accelerated paths recorded
// any, a column that is only ever aggregated could not earn a slot. So the aggregated column was
// systematically the cold one.
func TestAggregateColdColumnFirstQuery(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a multi-segment archive")
	}
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{Privileged: true}) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); cat.Close() }()
	ctx := context.Background()

	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{SegmentSize: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	const n = 60000
	for i := 0; i < n; i++ {
		if err := c.ArchiveAppend(ctx, "history", wideHistoryAd(i)); err != nil {
			t.Fatal(err)
		}
	}
	a, ok := cat.ArchiveTable("history")
	if !ok {
		t.Fatal("archive missing")
	}
	maxOnce := func() time.Duration {
		start := time.Now()
		if _, err := c.ArchiveAggregate(ctx, "history", "true", nil,
			[]db.AggSpec{{Func: db.AggMax, Arg: "ProcId"}}); err != nil {
			t.Fatal(err)
		}
		return time.Since(start)
	}
	hotHas := func(attr string) bool {
		for _, f := range a.SchemaScanInfo().HotFields {
			if strings.EqualFold(f, attr) {
				return true
			}
		}
		return false
	}

	// Squeeze ProcId out of the hot tier: demand another column, and allow only one hot slot.
	for i := 0; i < 30; i++ {
		if _, err := c.ArchiveAggregate(ctx, "history", "JobStatus >= 0", nil,
			[]db.AggSpec{{Func: db.AggCount, Arg: "*"}}); err != nil {
			t.Fatal(err)
		}
	}
	if !a.BuildAndEnableSchemaScan(4000, 1) {
		t.Skip("no sealed segments to sample")
	}
	if hotHas("ProcId") {
		t.Skip("ProcId took the single hot slot; the cold-column path cannot be measured here")
	}
	coldFirst, coldSecond := maxOnce(), maxOnce()
	t.Logf("ProcId COLD: first=%v second=%v  (hot: %v)",
		coldFirst.Round(time.Millisecond), coldSecond.Round(time.Millisecond), a.SchemaScanInfo().HotFields)

	// Now let the aggregate itself earn ProcId a slot, and rebuild -- which also creates a FRESH
	// block cache, so the first query below is cold-cache too and the comparison is fair.
	for i := 0; i < 40; i++ {
		maxOnce()
	}
	if !a.ReschemaScan(4000, 1) {
		t.Fatal("ReschemaScan failed")
	}
	if !hotHas("ProcId") {
		t.Fatalf("ProcId still not hot after being aggregated repeatedly (hot: %v)",
			a.SchemaScanInfo().HotFields)
	}
	hotFirst, hotSecond := maxOnce(), maxOnce()
	t.Logf("ProcId HOT:  first=%v second=%v  (hot: %v)",
		hotFirst.Round(time.Millisecond), hotSecond.Round(time.Millisecond), a.SchemaScanInfo().HotFields)

	// The asymmetry is the finding: a cold column's first query is far slower than its second,
	// because it is paying decompression that the cache then holds. A hot column has no such gap.
	coldRatio := float64(coldFirst) / float64(coldSecond)
	hotRatio := float64(hotFirst) / float64(hotSecond)
	t.Logf("first/second ratio: cold=%.1fx hot=%.1fx", coldRatio, hotRatio)
	if coldRatio < 2 {
		t.Logf("NOTE: cold first/second ratio under 2x at this scale; the effect needs a working " +
			"set large enough for decompression to dominate")
	}
	if hotFirst > coldFirst {
		t.Errorf("hot column's first query (%v) slower than the cold column's (%v) -- unexpected",
			hotFirst, coldFirst)
	}
}
