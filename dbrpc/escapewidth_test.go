package dbrpc

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
)

// TestHotColumnNarrowWidthEscapes covers the case a report surfaced: `.schema` showed
//
//	ProcId   int (u)   1   HOT
//
// a HOT column, one byte wide -- while MAX(ProcId) was 15974, which does not fit in a byte. The
// width is fitted to a high percentile of the SAMPLE, so if almost every job has a small ProcId the
// column is one byte and the rare large ones ESCAPE to the cold tail.
//
// An escaped value is read via blockCache.streams, which decompresses ALL THREE of a block's
// streams. So the cost is not proportional to the escape RATE: one escapee in a block pulls in that
// whole block's streams. Sprinkle 0.1% of records with large values and essentially every block has
// one, so the first query decompresses the entire dataset and the second is served from cache --
// which is exactly the 60s-then-1.6s asymmetry reported, on a column that `.schema` calls hot.
//
// MAX is the worst case for this: the values it needs are precisely the escaped ones.
func TestHotColumnNarrowWidthEscapes(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a multi-segment archive")
	}
	// procId mimics a real history: nearly every job has a small ProcId, a few clusters are huge.
	build := func(t *testing.T, procId func(int) int) (*Client, *db.ArchiveTable, func()) {
		cat, err := db.OpenCatalog(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		s := NewServerCatalog(cat)
		cconn, sconn := netPipe()
		go func() { _ = s.ServeConnOpts(sconn, ServeOptions{Privileged: true}) }()
		c := NewClient(cconn)
		ctx := context.Background()
		if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{SegmentSize: 1 << 20}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 60000; i++ {
			ad := strings.Replace(wideHistoryAd(i),
				fmt.Sprintf("ProcId = %d\n", i%10000),
				fmt.Sprintf("ProcId = %d\n", procId(i)), 1)
			if err := c.ArchiveAppend(ctx, "history", ad); err != nil {
				t.Fatal(err)
			}
		}
		a, ok := cat.ArchiveTable("history")
		if !ok {
			t.Fatal("archive missing")
		}
		if !a.BuildAndEnableSchemaScan(4000, 8) {
			t.Skip("no sealed segments to sample")
		}
		return c, a, func() { c.Close(); s.Close(); cat.Close() }
	}
	describe := func(t *testing.T, c *Client, a *db.ArchiveTable, label string) {
		ctx := context.Background()
		var width int
		var hot bool
		for _, f := range a.SchemaScanInfo().Schema {
			if strings.EqualFold(f.Name, "ProcId") {
				width, hot = f.Width, f.Hot
			}
		}
		var escaped float64
		fit, _ := a.SchemaFit(4000)
		for _, f := range fit {
			if strings.EqualFold(f.Name, "ProcId") {
				escaped = f.Escaped
			}
		}
		maxOnce := func() (string, time.Duration) {
			start := time.Now()
			rows, err := c.ArchiveAggregate(ctx, "history", "true", nil,
				[]db.AggSpec{{Func: db.AggMax, Arg: "ProcId"}})
			if err != nil {
				t.Fatal(err)
			}
			return rows[0].Values[0], time.Since(start)
		}
		v1, d1 := maxOnce()
		_, d2 := maxOnce()
		t.Logf("%-22s ProcId hot=%v width=%d escaped=%.2f%%  MAX=%s  first=%v second=%v (%.1fx)",
			label, hot, width, escaped*100, v1,
			d1.Round(time.Millisecond), d2.Round(time.Millisecond), float64(d1)/float64(d2))
	}

	// Skewed: 99.9% of jobs have ProcId < 10, one in a thousand is large. This is the reported shape.
	t.Run("skewed", func(t *testing.T) {
		c, a, cleanup := build(t, func(i int) int {
			if i%1000 == 0 {
				return 15974
			}
			return i % 10
		})
		defer cleanup()
		describe(t, c, a, "skewed (rare large)")
	})

	// Uniform: every job's ProcId is large, so the fitted width covers them and nothing escapes.
	t.Run("uniform", func(t *testing.T) {
		c, a, cleanup := build(t, func(i int) int { return 1000 + i%15000 })
		defer cleanup()
		describe(t, c, a, "uniform (all large)")
	})
}
