package dbrpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/db"
)

// TestQueryRawProjectRefsStreamStats verifies the scan-stats trailer round-trips: the server
// reports how the scan broke down (segments/records/rows), matching what actually streamed.
func TestQueryRawProjectRefsStreamStats(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{})
	defer cleanup()
	ctx := context.Background()

	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{ValueAttrs: []string{"ClusterId"}}); err != nil {
		t.Fatalf("CreateArchiveTable: %v", err)
	}
	const n = 200
	matchExpected := 0
	for i := 0; i < n; i++ {
		status := 2
		if i%4 == 0 {
			status = 4
			matchExpected++
		}
		ad := fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = %d\nOwner = \"u%d\"", i, i%3, status, i%10)
		if err := c.ArchiveAppend(ctx, "history", ad); err != nil {
			t.Fatalf("ArchiveAppend: %v", err)
		}
	}

	var stats collections.ScanStats
	rows := 0
	err := c.QueryRawProjectRefsStreamStats(ctx, "history", "JobStatus == 4",
		[]string{"ClusterId", "JobStatus"}, 0, func(string) bool { rows++; return true }, &stats)
	if err != nil {
		t.Fatalf("QueryRawProjectRefsStreamStats: %v", err)
	}
	if rows != matchExpected {
		t.Fatalf("streamed %d rows, want %d", rows, matchExpected)
	}
	if stats.RowsMatched != matchExpected {
		t.Errorf("stats.RowsMatched = %d, want %d", stats.RowsMatched, matchExpected)
	}
	if stats.RecordsVisited < n {
		t.Errorf("stats.RecordsVisited = %d, want >= %d", stats.RecordsVisited, n)
	}
	if stats.SegmentsScanned < 1 {
		t.Errorf("stats.SegmentsScanned = %d, want >= 1", stats.SegmentsScanned)
	}
	t.Logf("scan stats: %+v", stats)
}
