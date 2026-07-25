package dbrpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// TestArchiveDiagnosticsOverRPC verifies `.stats`-level diagnostics work for an archive
// (history) table through opDiag -- rich storage/codec/op stats plus retention -- the same
// path a mutable table uses, so archive stats are symmetric instead of just a row count.
func TestArchiveDiagnosticsOverRPC(t *testing.T) {
	ctx := context.Background()
	c, cleanup := catServerPair(t, ServeOptions{Privileged: true})
	defer cleanup()
	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{
		SegmentSize: 1 << 12, // small -> several segments
		ValueAttrs:  []string{"ClusterId"},
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 400; i++ {
		ad := fmt.Sprintf("ClusterId = %d\nOwner = \"u%d\"\nJobStatus = 4", i, i%10)
		if err := c.ArchiveAppend(ctx, "history", ad); err != nil {
			t.Fatal(err)
		}
	}

	d, err := c.DiagnosticsTable(ctx, "history")
	if err != nil {
		t.Fatalf("DiagnosticsTable(history): %v", err)
	}
	if !d.Archive {
		t.Error("diagnostics.Archive = false, want true for a history table")
	}
	if d.Stats.Ads != 400 {
		t.Errorf("Stats.Ads = %d, want 400", d.Stats.Ads)
	}
	if d.Stats.Segments < 2 {
		t.Errorf("Stats.Segments = %d, want several (rich storage stats missing)", d.Stats.Segments)
	}
	if d.Stats.ArenaBytes == 0 || d.Stats.UsedBytes == 0 {
		t.Errorf("byte accounting empty: arena=%d used=%d", d.Stats.ArenaBytes, d.Stats.UsedBytes)
	}
	if d.OpStats.ShardWriteHold.Count == 0 {
		t.Error("OpStats not populated for archive writes")
	}
	if d.Retention == nil {
		t.Error("Retention missing from archive diagnostics")
	}
}

// TestArchiveRetentionAdminOverRPC verifies setting retention and manual rotation through the
// admin RPC path, and that the retention persists into the diagnostics.
func TestArchiveRetentionAdminOverRPC(t *testing.T) {
	ctx := context.Background()
	c, cleanup := catServerPair(t, ServeOptions{Privileged: true})
	defer cleanup()
	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{SegmentSize: 1 << 12}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 400; i++ {
		if err := c.ArchiveAppend(ctx, "history", fmt.Sprintf("ClusterId = %d", i)); err != nil {
			t.Fatal(err)
		}
	}

	// Set retention: keep at most 2 segments.
	if _, err := c.AdminTable(ctx, "history", "retention.set", "2", "0"); err != nil {
		t.Fatalf("retention.set: %v", err)
	}
	d, err := c.DiagnosticsTable(ctx, "history")
	if err != nil {
		t.Fatal(err)
	}
	if d.Retention == nil || d.Retention.MaxSegments != 2 {
		t.Fatalf("after retention.set, Retention = %+v, want MaxSegments=2", d.Retention)
	}
	segsBefore := d.Stats.Segments

	// Manual rotate should now drop segments beyond the bound.
	msg, err := c.AdminTable(ctx, "history", "rotate")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if msg == "" {
		t.Error("rotate returned an empty message")
	}
	d2, err := c.DiagnosticsTable(ctx, "history")
	if err != nil {
		t.Fatal(err)
	}
	if d2.Stats.Segments >= segsBefore {
		t.Errorf("rotate did not drop segments: before %d, after %d", segsBefore, d2.Stats.Segments)
	}
	if d2.Stats.Segments > 2 {
		t.Errorf("after rotate to MaxSegments=2, %d segments remain", d2.Stats.Segments)
	}

	// A byte-size suffix parses.
	if _, err := c.AdminTable(ctx, "history", "retention.set", "0", "1MiB"); err != nil {
		t.Fatalf("retention.set with MiB suffix: %v", err)
	}
	d3, _ := c.DiagnosticsTable(ctx, "history")
	if d3.Retention == nil || d3.Retention.MaxBytes != 1<<20 {
		t.Fatalf("MaxBytes = %v, want 1MiB (1048576)", d3.Retention)
	}
}
