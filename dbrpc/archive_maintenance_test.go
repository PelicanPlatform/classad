package dbrpc

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
)

// TestArchiveMaintenanceMerges is the regression for a gap that made every piece of archive
// maintenance dead code: StartMaintenance iterated only cat.Tables(), which excludes archive
// tables, so nothing ever merged or reindexed an archive. The failure mode is quiet -- the
// segment count simply climbs until the daemon cannot map them all at startup.
func TestArchiveMaintenanceMerges(t *testing.T) {
	c, srv, cleanup := catServerPairWithServer(t)
	defer cleanup()

	if err := c.CreateArchiveTable(context.Background(), "history", db.ArchiveConfig{
		SegmentSize: 1 << 12,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3000; i++ {
		if err := c.ArchiveAppend(context.Background(), "history",
			fmt.Sprintf("ClusterId = %d\nPad = %q", i, strings.Repeat("z", 60))); err != nil {
			t.Fatal(err)
		}
	}
	before, err := c.ArchiveQuery(context.Background(), "history", "true", 0)
	if err != nil {
		t.Fatal(err)
	}
	arch, ok := srv.cat.ArchiveTable("history")
	if !ok {
		t.Fatal("archive missing from the catalog")
	}
	segsBefore := arch.Stats().Segments
	if segsBefore < 10 {
		t.Fatalf("need enough segments to merge, got %d", segsBefore)
	}

	srv.maintainArchives(db.MaintainOptions{
		ArchiveMerge: db.MergeOptions{
			TargetSegments: 4, TriggerSegments: 8, MinMergeBytes: 1, KeepRecent: 2,
		},
	})

	after, err := c.ArchiveQuery(context.Background(), "history", "true", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("records after maintenance = %d, want %d", len(after), len(before))
	}
	// Records surviving is necessary but not sufficient: it would also hold if maintenance
	// did nothing at all, which is exactly the bug this covers.
	if segsAfter := arch.Stats().Segments; segsAfter >= segsBefore {
		t.Errorf("segments %d -> %d: the archive maintenance pass did not merge anything",
			segsBefore, segsAfter)
	}
	for i := range before {
		if after[i] != before[i] {
			t.Fatalf("record %d changed across maintenance", i)
		}
	}
}

// TestArchiveMaintenanceDisabled checks the off switch, since merging rewrites data and an
// operator may want it under their own control.
func TestArchiveMaintenanceDisabled(t *testing.T) {
	c, srv, cleanup := catServerPairWithServer(t)
	defer cleanup()
	if err := c.CreateArchiveTable(context.Background(), "history", db.ArchiveConfig{
		SegmentSize: 1 << 12,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		if err := c.ArchiveAppend(context.Background(), "history",
			fmt.Sprintf("ClusterId = %d\nPad = %q", i, strings.Repeat("y", 60))); err != nil {
			t.Fatal(err)
		}
	}
	archBefore, ok := srv.cat.ArchiveTable("history")
	if !ok {
		t.Fatal("archive missing from the catalog")
	}
	segsBefore := archBefore.Stats().Segments
	done := make(chan struct{})
	go func() {
		srv.maintainArchives(db.MaintainOptions{
			ArchiveMergeDisabled: true,
			ArchiveMerge:         db.MergeOptions{TargetSegments: 1, TriggerSegments: 2, MinMergeBytes: 1},
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("disabled maintenance did not return promptly")
	}
	arch, ok := srv.cat.ArchiveTable("history")
	if !ok {
		t.Fatal("archive missing from the catalog")
	}
	if got := arch.Stats().Segments; got != segsBefore {
		t.Errorf("segments %d -> %d while merging was disabled", segsBefore, got)
	}
}

// catServerPairWithServer is catServerPair keeping the Server, so a test can drive a
// maintenance pass directly instead of waiting on the scheduled one.
func catServerPairWithServer(t *testing.T) (*Client, *Server, func()) {
	t.Helper()
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{}) }()
	c := NewClient(cconn)
	return c, s, func() { c.Close(); s.Close(); cat.Close() }
}
