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

// archiveHotFixture builds an archive with enough records to sample, and registers read
// demand on one attribute that is not hot at creation.
func archiveHotFixture(t *testing.T) (*db.ArchiveTable, *Server, func()) {
	t.Helper()
	c, srv, cleanup := catServerPairWithServer(t)
	if err := c.CreateArchiveTable(context.Background(), "history", db.ArchiveConfig{}); err != nil {
		cleanup()
		t.Fatal(err)
	}
	for i := 0; i < 400; i++ {
		if err := c.ArchiveAppend(context.Background(), "history",
			fmt.Sprintf("ClusterId = %d\nGlobalJobId = %q\nExitCode = %d", i, fmt.Sprintf("ap1#%d.0#17", i), i%3)); err != nil {
			cleanup()
			t.Fatal(err)
		}
	}
	for range 10 {
		if _, err := c.ArchiveQuery(context.Background(), "history", `GlobalJobId == "ap1#3.0#17"`, 0); err != nil {
			cleanup()
			t.Fatal(err)
		}
	}
	a, ok := srv.cat.ArchiveTable("history")
	if !ok {
		cleanup()
		t.Fatal("archive missing from the catalog")
	}
	if hasHotFold(a.HotAttrs(), "GlobalJobId") {
		cleanup()
		t.Fatalf("GlobalJobId hot before any maintenance: %v", a.HotAttrs())
	}
	return a, srv, cleanup
}

func hasHotFold(names []string, s string) bool {
	for _, n := range names {
		if strings.EqualFold(n, s) {
			return true
		}
	}
	return false
}

// TestArchiveMaintenanceRefreshesHotSet: the scheduled pass re-ranks an archive's hot
// header from accumulated read demand, on the shared HotTopN. Before this the hot set was
// whatever creation configured, forever -- the one piece of archive self-tuning that costs
// no backfill was also the one nothing ever ran.
func TestArchiveMaintenanceRefreshesHotSet(t *testing.T) {
	a, srv, cleanup := archiveHotFixture(t)
	defer cleanup()

	srv.maintainArchives(db.MaintainOptions{HotTopN: 8, ArchiveMergeDisabled: false})

	if !hasHotFold(a.HotAttrs(), "GlobalJobId") {
		t.Errorf("hot set after maintenance = %v, want the queried GlobalJobId", a.HotAttrs())
	}
}

// TestArchiveMaintenanceHotRefreshDisabled: the escape hatch leaves the configured set
// alone. Without it an archive whose hot set was chosen deliberately could not keep it.
func TestArchiveMaintenanceHotRefreshDisabled(t *testing.T) {
	a, srv, cleanup := archiveHotFixture(t)
	defer cleanup()

	srv.maintainArchives(db.MaintainOptions{HotTopN: 8, ArchiveHotRefreshDisabled: true})

	if hasHotFold(a.HotAttrs(), "GlobalJobId") {
		t.Errorf("hot set after a disabled refresh = %v, want it unchanged", a.HotAttrs())
	}
}

// TestArchiveMaintenanceHotRefreshNeedsTopN: HotTopN == 0 means "no hot-set target" for a
// mutable table and must mean the same for an archive.
func TestArchiveMaintenanceHotRefreshNeedsTopN(t *testing.T) {
	a, srv, cleanup := archiveHotFixture(t)
	defer cleanup()

	srv.maintainArchives(db.MaintainOptions{})

	if hasHotFold(a.HotAttrs(), "GlobalJobId") {
		t.Errorf("hot set with HotTopN unset = %v, want it unchanged", a.HotAttrs())
	}
}

// TestArchiveAdminHotVerbs: hot.refresh and hot.add reach an archive, matching the
// mutable-table verbs. The archive path used to reject them outright, so an operator had no
// way to change a history table's hot set at all.
func TestArchiveAdminHotVerbs(t *testing.T) {
	a, _, cleanup := archiveHotFixture(t)
	defer cleanup()

	msg, err := archiveAdmin(a, "hot.refresh", []string{"1000", "8"}, db.MaintainOptions{})
	if err != nil {
		t.Fatalf("hot.refresh: %v", err)
	}
	if !strings.Contains(msg, "refreshed hot set") || !strings.Contains(msg, "appended from now on") {
		t.Errorf("hot.refresh message %q should say what it did and how far it reaches", msg)
	}
	if !hasHotFold(a.HotAttrs(), "GlobalJobId") {
		t.Errorf("hot set after hot.refresh = %v, want the queried GlobalJobId", a.HotAttrs())
	}

	if _, err := archiveAdmin(a, "hot.add", []string{"Pad"}, db.MaintainOptions{}); err != nil {
		t.Fatalf("hot.add: %v", err)
	}
	if !hasHotFold(a.HotAttrs(), "Pad") {
		t.Errorf("hot set after hot.add = %v, want Pad", a.HotAttrs())
	}
	if _, err := archiveAdmin(a, "hot.add", nil, db.MaintainOptions{}); err == nil {
		t.Error("hot.add with no attributes should be refused")
	}
}

// TestArchiveAdminAnalyzeRefreshesHotSet: analyze is the manual counterpart of the
// scheduled pass, so it tunes rather than only reindexing -- and says so instead of
// reporting that an archive has no demand-driven tuning.
func TestArchiveAdminAnalyzeRefreshesHotSet(t *testing.T) {
	a, _, cleanup := archiveHotFixture(t)
	defer cleanup()

	msg, err := archiveAdmin(a, "analyze", nil, db.MaintainOptions{HotTopN: 8})
	if err != nil {
		t.Fatalf("archive analyze: %v", err)
	}
	if !hasHotFold(a.HotAttrs(), "GlobalJobId") {
		t.Errorf("hot set after analyze = %v, want the queried GlobalJobId", a.HotAttrs())
	}
	if strings.Contains(msg, "no demand-driven") {
		t.Errorf("analyze still reports an archive has no demand-driven tuning: %q", msg)
	}
	if !strings.Contains(msg, "hot set") {
		t.Errorf("analyze message %q should report the hot set it chose", msg)
	}
}

// TestArchiveAdminAnalyzeAutoTunesIndexes: with a threshold configured, analyze adds the
// index the accumulated demand justifies.
func TestArchiveAdminAnalyzeAutoTunesIndexes(t *testing.T) {
	a, _, cleanup := archiveHotFixture(t)
	defer cleanup()

	if cat, val := a.IndexedAttrs(); len(cat) != 0 || len(val) != 0 {
		t.Fatalf("archive starts with indexes %v/%v, want none", cat, val)
	}
	if _, err := archiveAdmin(a, "analyze", nil, db.MaintainOptions{
		HotTopN: 8, ArchiveIndexMinDemand: 5,
	}); err != nil {
		t.Fatalf("archive analyze: %v", err)
	}
	cat, val := a.IndexedAttrs()
	if !hasHotFold(cat, "GlobalJobId") && !hasHotFold(val, "GlobalJobId") {
		t.Errorf("indexes after analyze = %v/%v, want the queried GlobalJobId", cat, val)
	}
}
