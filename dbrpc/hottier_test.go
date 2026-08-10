package dbrpc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
)

// TestAggregateHotVsColdColumn asks what the columnar aggregate costs when the aggregated
// attribute is NOT in the hot tier.
//
// The hot tier holds the top-N numeric fields BY QUERY READ DEMAND, uncompressed. Everything else
// is a cold column group that has to be decompressed to read. The tier is chosen when the schema is
// built -- so an attribute you only ever aggregate, having never been filtered on, can easily be
// cold, and a schema REBUILD re-derives the tier from demand accumulated since while a REWRITE
// preserves the old one. That asymmetry would explain a rebuild making a query fast and a rewrite
// leaving it slow.
func TestAggregateHotVsColdColumn(t *testing.T) {
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
	for i := 0; i < 12000; i++ {
		if err := c.ArchiveAppend(ctx, "history", wideHistoryAd(i)); err != nil {
			t.Fatal(err)
		}
	}
	a, ok := cat.ArchiveTable("history")
	if !ok {
		t.Fatal("archive missing")
	}

	timeMax := func() time.Duration {
		best := time.Hour
		for i := 0; i < 3; i++ { // best of three: ignore page-cache cold starts
			start := time.Now()
			if _, err := c.ArchiveAggregate(ctx, "history", "true", nil,
				[]db.AggSpec{{Func: db.AggMax, Arg: "ProcId"}}); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best
	}
	hotHas := func(attr string) bool {
		for _, f := range a.SchemaScanInfo().HotFields {
			if strings.EqualFold(f, attr) {
				return true
			}
		}
		return false
	}

	// Build the schema with demand on a DIFFERENT numeric attribute, so ProcId is cold. Only a
	// handful of hot slots exist, so the demanded ones win them.
	for i := 0; i < 25; i++ {
		if _, err := c.ArchiveAggregate(ctx, "history", "RequestMemory >= 0", nil,
			[]db.AggSpec{{Func: db.AggCount, Arg: "*"}}); err != nil {
			t.Fatal(err)
		}
	}
	if !a.BuildAndEnableSchemaScan(4000, 2) { // only 2 hot slots
		t.Skip("no sealed segments to sample")
	}
	coldHot := hotHas("ProcId")
	coldTime := timeMax()
	t.Logf("ProcId hot=%-5v  MAX best-of-3 = %v   (hot fields: %v)",
		coldHot, coldTime.Round(time.Millisecond), a.SchemaScanInfo().HotFields)

	// Now re-derive with ProcId demanded, so it takes a hot slot.
	// Demand driven by the very query an operator runs, which is the point: aggregating ProcId has
	// to be what teaches the hot tier that ProcId is read.
	for i := 0; i < 60; i++ {
		if _, err := c.ArchiveAggregate(ctx, "history", "true", nil,
			[]db.AggSpec{{Func: db.AggMax, Arg: "ProcId"}}); err != nil {
			t.Fatal(err)
		}
	}
	if !a.ReschemaScan(4000, 2) {
		t.Fatal("ReschemaScan failed")
	}
	warmHot := hotHas("ProcId")
	warmTime := timeMax()
	t.Logf("ProcId hot=%-5v  MAX best-of-3 = %v   (hot fields: %v)",
		warmHot, warmTime.Round(time.Millisecond), a.SchemaScanInfo().HotFields)

	// The behaviour this pins: repeatedly aggregating a column, then rebuilding, must promote it.
	// Before the columnar paths recorded demand, no amount of aggregating could -- so the column a
	// workload cared about most stayed cold, and which columns were hot came down to a tie-break.
	if !warmHot {
		t.Errorf("aggregating ProcId %d times then rebuilding did NOT promote it to the hot tier "+
			"(hot: %v) -- the accelerated path is not feeding the demand signal that chooses the tier",
			60, a.SchemaScanInfo().HotFields)
	}
	t.Logf("cold->hot timing: %v -> %v", coldTime.Round(time.Millisecond), warmTime.Round(time.Millisecond))
}
