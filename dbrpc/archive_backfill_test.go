package dbrpc

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// backfillFixture builds a history archive whose added EnteredHistoryTime index reached only
// the newest segments, returning the archive and how many segments still lack it.
func backfillFixture(t *testing.T) (*Client, *Server, *db.ArchiveTable, func()) {
	t.Helper()
	// Privileged: index.add.value and index.reindex are DAEMON-level admin actions.
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = srv.ServeConnOpts(sconn, ServeOptions{Privileged: true}) }()
	c := NewClient(cconn)
	cleanup := func() { c.Close(); srv.Close(); cat.Close() }
	ctx := context.Background()
	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{
		SegmentSize:        1 << 12,
		ValueAttrs:         []string{"ClusterId"},
		IndexBackfillBytes: 16 << 10,
	}); err != nil {
		cleanup()
		t.Fatal(err)
	}
	for i := range 2000 {
		if err := c.ArchiveAppend(ctx, "history", fmt.Sprintf("ClusterId = %d\nEnteredHistoryTime = %d\nPad = %q",
			i, 1000+i, strings.Repeat("p", 24))); err != nil {
			cleanup()
			t.Fatal(err)
		}
	}
	if _, err := c.AdminTable(ctx, "history", "index.add.value", "EnteredHistoryTime"); err != nil {
		cleanup()
		t.Fatal(err)
	}
	a, ok := srv.cat.ArchiveTable("history")
	if !ok {
		cleanup()
		t.Fatal("archive missing from the catalog")
	}
	return c, srv, a, cleanup
}

func backfillPending(t *testing.T, c *Client) (int, int64) {
	t.Helper()
	d, err := c.DiagnosticsTable(context.Background(), "history")
	if err != nil {
		t.Fatal(err)
	}
	return d.BackfillPendingSegments, d.BackfillPendingBytes
}

// TestArchiveAdminReindexCoversWholeArchive is the operator's `.reindex history`. It used to
// run the horizon-bounded Reindex, which had nothing left to do inside the horizon and so
// returned at once while every older segment still lacked the index.
func TestArchiveAdminReindexCoversWholeArchive(t *testing.T) {
	c, _, a, cleanup := backfillFixture(t)
	defer cleanup()
	pending, pendingBytes := backfillPending(t, c)
	if pending == 0 || pendingBytes == 0 {
		t.Fatalf("fixture: diagnostics report nothing pending (%d, %d)", pending, pendingBytes)
	}
	msg, err := c.AdminTable(context.Background(), "history", "index.reindex")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, fmt.Sprintf("%d segment(s)", pending)) {
		t.Errorf("reindex reply %q does not report the %d segments it rebuilt", msg, pending)
	}
	if got, _ := backfillPending(t, c); got != 0 {
		t.Errorf("after index.reindex %d segments still pending", got)
	}
	ex, err := a.Explain(`EnteredHistoryTime == 1001`)
	if err != nil {
		t.Fatal(err)
	}
	if ex.UnindexedSegments != 0 {
		t.Errorf("after index.reindex: %s", ex.Reason)
	}
}

// TestArchiveMaintenanceBackfillsIndexes checks the maintenance pass carries the index set to
// older segments within its per-pass budget, and that a negative budget turns it off.
func TestArchiveMaintenanceBackfillsIndexes(t *testing.T) {
	c, srv, a, cleanup := backfillFixture(t)
	defer cleanup()
	pending, pendingBytes := backfillPending(t, c)
	if pending < 8 {
		t.Fatalf("fixture: %d segments pending", pending)
	}
	// Merging would fold the very segments being counted; keep it out of the way.
	noMerge := db.MergeOptions{TriggerSegments: 1 << 20, TargetSegments: 1 << 19}

	srv.maintainArchives(db.MaintainOptions{ArchiveMerge: noMerge, ArchiveIndexBackfillBytes: -1})
	if got, _ := backfillPending(t, c); got != pending {
		t.Fatalf("backfill turned off, yet pending went %d -> %d", pending, got)
	}

	budget := 2 * pendingBytes / int64(pending)
	passes := 0
	for {
		before, _ := backfillPending(t, c)
		if before == 0 {
			break
		}
		srv.maintainArchives(db.MaintainOptions{ArchiveMerge: noMerge, ArchiveIndexBackfillBytes: budget})
		passes++
		after, _ := backfillPending(t, c)
		if after >= before {
			t.Fatalf("pass %d made no progress (%d pending)", passes, after)
		}
		if before-after > 3 {
			t.Errorf("pass %d rebuilt %d segments on a budget of about 2", passes, before-after)
		}
		if passes > pending {
			t.Fatal("not converging")
		}
	}
	if ex, _ := a.Explain(`EnteredHistoryTime == 1001`); ex.UnindexedSegments != 0 {
		t.Errorf("after maintenance: %s", ex.Reason)
	}
	if st := a.OpStats().IndexBackfill; st.Count != int64(pending) {
		t.Errorf("IndexBackfill count %d, want %d", st.Count, pending)
	}
}
