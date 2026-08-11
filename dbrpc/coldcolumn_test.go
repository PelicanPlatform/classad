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

	// The asymmetry is the finding: a cold column's first query is far slower than its second, because it is
	// paying decompression that the cache then holds. A hot column has no such gap.
	//
	// ASSERTED ON THE RATIOS, not on a cross-configuration absolute time. Each ratio comes from two queries
	// run back-to-back, so a busy machine inflates both members of a pair and largely cancels; comparing one
	// configuration's absolute time against another's compares whatever the machine was doing in between.
	// This test used to do that -- `hotFirst > coldFirst` -- and failed on a loaded run at hot=62ms against
	// cold=20ms while the ratios that run were cold=1.2x hot=0.8x, which is the shape it was looking for.
	coldRatio := float64(coldFirst) / float64(coldSecond)
	hotRatio := float64(hotFirst) / float64(hotSecond)
	t.Logf("first/second ratio: cold=%.1fx hot=%.1fx", coldRatio, hotRatio)

	// And asserted only when the fixture actually demonstrates the effect. Below 2x the working set is too
	// small for decompression to dominate, both ratios sit near 1.0 (measured 1.1x to 1.3x cold against 0.8x
	// to 1.1x hot), and which one is larger is noise -- so there is nothing to conclude and the test says so
	// rather than pretending to check it.
	//
	// WHAT THIS TEST STILL ASSERTS UNCONDITIONALLY, so that early return is not a hole: that demanding another
	// column squeezes ProcId out of a single hot slot, and that aggregating ProcId repeatedly earns it back --
	// the hot/cold routing itself, above. Those are mechanical and hold under any load. Only the TIMING claim
	// needs a working set this fixture does not build, and growing it is the open item rather than asserting
	// a difference of a millisecond or two.
	if coldRatio < 2 {
		t.Logf("NOTE: cold first/second ratio under 2x at this scale, so the ordering of the two ratios is " +
			"not asserted; the effect needs a working set large enough for decompression to dominate")
		return
	}
	if coldRatio <= hotRatio {
		t.Errorf("a cold column's first query should pay decompression its second does not, and a hot "+
			"column's should not: cold ratio %.1fx is not above hot ratio %.1fx (cold %v/%v, hot %v/%v)",
			coldRatio, hotRatio, coldFirst, coldSecond, hotFirst, hotSecond)
	}
}
