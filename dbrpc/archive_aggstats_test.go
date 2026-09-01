package dbrpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/db"
)

// TestArchiveAggregateStatsOverRPC verifies the opArchiveAggregateStats trailer: a server-side
// aggregate returns the correct value AND a ScanStats reflecting the scan round-trips over the wire,
// so EXPLAIN ANALYZE of an aggregate is no longer blind. The aggregate here takes the per-record
// fall-through path (MAX over an attribute no columnar accelerator covers, on a non-columnarized
// archive), which is exactly the slow shape the breakdown exists to surface: a large
// RecordsReassembled.
func TestArchiveAggregateStatsOverRPC(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{})
	defer cleanup()
	ctx := context.Background()

	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{
		SegmentSize: 1 << 12, // small -> several sealed segments to scan
		ZoneAttrs:   []string{"ClusterId"},
	}); err != nil {
		t.Fatalf("CreateArchiveTable: %v", err)
	}

	const n = 600
	matched, wantMax := 0, int64(-1)
	for i := 0; i < n; i++ {
		proc := int64(i % 50)
		status := 2
		if i%5 == 0 {
			status = 4
			matched++
			if proc > wantMax {
				wantMax = proc
			}
		}
		if err := c.ArchiveAppend(ctx, "history",
			fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = %d", i, proc, status)); err != nil {
			t.Fatalf("ArchiveAppend: %v", err)
		}
	}

	var stats collections.ScanStats
	rows, err := c.ArchiveAggregateStats(ctx, "history", "JobStatus == 4",
		nil, []AggSpec{{Func: AggMax, Arg: "ProcId"}}, &stats)
	if err != nil {
		t.Fatalf("ArchiveAggregateStats: %v", err)
	}
	if len(rows) != 1 || len(rows[0].Values) != 1 {
		t.Fatalf("want one row with one value, got %+v", rows)
	}
	if rows[0].Values[0] != fmt.Sprintf("%d", wantMax) {
		t.Fatalf("MAX(ProcId) = %s, want %d", rows[0].Values[0], wantMax)
	}

	// The trailer must reflect the scan: it visited records and matched the JobStatus==4 set, and --
	// on this non-columnarized fall-through -- reassembled the records it read (the slow path an
	// aggregate takes when the accelerator cannot serve the attribute from columns).
	if stats.RecordsVisited == 0 {
		t.Error("stats.RecordsVisited = 0: the aggregate scan was not reported over the wire")
	}
	if stats.RowsMatched != matched {
		t.Errorf("stats.RowsMatched = %d, want %d", stats.RowsMatched, matched)
	}
	if stats.RecordsReassembled == 0 {
		t.Error("stats.RecordsReassembled = 0: the per-record fall-through should have reassembled records")
	}
	t.Logf("archive aggregate scan stats: %+v", stats)
}
