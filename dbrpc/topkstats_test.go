package dbrpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/db"
)

// TestTopKStatsOverRPC verifies the opTopKStats trailer: the k rows come back correct AND a
// ScanStats reflecting the cutoff scan round-trips over the wire, so EXPLAIN ANALYZE of a top-K
// query is no longer blind.
func TestTopKStatsOverRPC(t *testing.T) {
	c, srv, cleanup := catServerPairWithServer(t)
	defer cleanup()
	ctx := context.Background()

	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{
		SegmentSize: 1 << 15, ValueAttrs: []string{"ClusterId"}, ZoneAttrs: []string{"ClusterId"},
	}); err != nil {
		t.Fatal(err)
	}
	const n = 3000
	matched := 0
	for i := 0; i < n; i++ {
		status := 2
		if i%5 == 0 {
			status = 4
			matched++
		}
		if err := c.ArchiveAppend(ctx, "history",
			fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = %d", i, i%7, status)); err != nil {
			t.Fatal(err)
		}
	}
	arch, ok := srv.cat.ArchiveTable("history")
	if !ok {
		t.Fatal("archive missing from catalog")
	}
	if !arch.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("schema-scan accelerator did not enable")
	}

	var stats collections.ScanStats
	rows, err := c.TopKStats(ctx, "history", "JobStatus == 4", []string{"ClusterId"}, "ClusterId", true, 1, &stats)
	if err != nil {
		t.Fatalf("TopKStats: %v", err)
	}
	if got := topKClusterIds(t, rows); len(got) != 1 || got[0] != 2995 {
		t.Fatalf("rows = %v, want [2995] (max ClusterId with JobStatus==4)", got)
	}
	// The trailer must reflect the cutoff scan: it visited records and matched the JobStatus==4 set.
	if stats.RecordsVisited == 0 {
		t.Error("stats.RecordsVisited = 0: the cutoff scan was not reported over the wire")
	}
	if stats.RowsMatched != matched {
		t.Errorf("stats.RowsMatched = %d, want %d", stats.RowsMatched, matched)
	}
	t.Logf("top-K cutoff scan stats: %+v", stats)
}
