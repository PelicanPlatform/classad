package dbrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
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

// TestArchiveMaintenanceMergeDisabledStillTunes: ArchiveMergeDisabled is a merge knob, and
// the rest of the pass must keep running under it. It used to return from the whole
// function, so every step added after it -- hot-set refresh, auto-tune, schema scan, group
// derivation, demand checkpoint -- was switched off by a knob that never mentioned them.
func TestArchiveMaintenanceMergeDisabledStillTunes(t *testing.T) {
	a, srv, cleanup := archiveHotFixture(t)
	defer cleanup()

	srv.maintainArchives(db.MaintainOptions{HotTopN: 8, ArchiveMergeDisabled: true})

	if !hasHotFold(a.HotAttrs(), "GlobalJobId") {
		t.Errorf("hot set after a merge-disabled pass = %v, want the queried GlobalJobId: "+
			"disabling the merge must not disable the rest of the pass", a.HotAttrs())
	}
}

// groupArchiveFixture builds an archive whose ads carry a recurring attribute group, with the
// stability gate set to two derivations so a short test can reach it. It returns the catalog
// directory so a test can read the derivation checkpoint the gate reads.
func groupArchiveFixture(t *testing.T, records, stabilityRuns int) (*db.ArchiveTable, *Server, string, func()) {
	t.Helper()
	dir := t.TempDir()
	cat, err := db.OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServerCatalog(cat)
	cleanup := func() { s.Close(); cat.Close() }
	a, err := cat.CreateArchiveTable("history", db.ArchiveConfig{
		SegmentSize: 1 << 14, GroupSchemaCount: 4, GroupStabilityRuns: stabilityRuns,
	})
	if err != nil {
		cleanup()
		t.Fatal(err)
	}
	for i := range records {
		ad := classad.New()
		ad.Set("ClusterId", int64(i))
		ad.Set("Owner", fmt.Sprintf("u%d", i%5))
		if i%4 == 0 {
			ad.Set("GA", int64(i))
			ad.Set("GB", int64(i*2))
			ad.Set("GC", int64(i*3))
		}
		if err := a.Append(ad); err != nil {
			cleanup()
			t.Fatal(err)
		}
	}
	if err := a.Flush(); err != nil {
		cleanup()
		t.Fatal(err)
	}
	return a, s, dir, cleanup
}

// TestArchiveMaintenanceDerivesGroupsBeforeSchemaScan: the group derivation appends to the
// history the schema scan's stability gate reads, so it has to run FIRST within a pass -- as
// DB.Maintain does it. Deriving afterwards means the gate never sees the current pass and a
// qualifying group waits one extra review interval, which on an archive is a day.
//
// With the gate at two derivations, two passes must be enough.
func TestArchiveMaintenanceDerivesGroupsBeforeSchemaScan(t *testing.T) {
	a, srv, _, cleanup := groupArchiveFixture(t, 800, 2)
	defer cleanup()

	for range 2 {
		srv.maintainArchives(db.MaintainOptions{ArchiveSchemaScanHotTopN: 8})
	}

	info := a.SchemaScanInfo()
	if !info.Enabled {
		t.Fatal("the accelerator did not enable, so this test measures nothing")
	}
	if info.GroupSchemas == 0 {
		t.Errorf("no group schemas committed after %d passes at a 2-derivation gate: "+
			"the derivation ran after the schema scan, so the gate never saw this pass", 2)
	}
}

// TestArchiveMaintenanceSamplesOnceForTheWholePass: one pass, one sample bound. The pass
// defaulted SampleMax for some steps and handed the raw (possibly unset) value to the group
// derivation and the index tuner, which substitute collections' own larger fallback -- two
// sample sizes inside one pass, the larger of them on the steps nobody chose it for.
//
// Measured on the derivation checkpoint, which is the only one of the two that records what
// it looked at. The fixture holds more records than the pass's bound and fewer than the
// library's, so the two are distinguishable.
func TestArchiveMaintenanceSamplesOnceForTheWholePass(t *testing.T) {
	const records = 3000
	a, srv, dir, cleanup := groupArchiveFixture(t, records, 2)
	defer cleanup()
	if a.Count() != records {
		t.Fatalf("fixture holds %d records, want %d", a.Count(), records)
	}

	srv.maintainArchives(db.MaintainOptions{ArchiveSchemaScanHotTopN: 8})

	sampled := lastGroupDerivationSampled(t, dir)
	if sampled == 0 {
		t.Fatal("no group derivation was checkpointed, so this test measures nothing")
	}
	if sampled > diagSampleMax {
		t.Errorf("the group derivation sampled %d records; the pass's bound is %d, so it is "+
			"running on a different sample size than the rest of the pass", sampled, diagSampleMax)
	}
}

// lastGroupDerivationSampled reads how many records the most recent group derivation under
// dir looked at. The checkpoint is the file the stability gate itself reads.
func lastGroupDerivationSampled(t *testing.T, dir string) int {
	t.Helper()
	var found string
	if err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "groupschemas.json" {
			found = p
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if found == "" {
		t.Fatal("no groupschemas.json under the catalog directory")
	}
	data, err := os.ReadFile(found)
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		History []struct {
			Sampled int `json:"sampled"`
		} `json:"history"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.History) == 0 {
		return 0
	}
	return rec.History[len(rec.History)-1].Sampled
}
