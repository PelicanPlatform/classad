package dbrpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// seedHistory appends n history-shaped ads to an archive table.
func seedHistory(t *testing.T, c *Client, name string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if err := c.ArchiveAppend(ctx, name, fmt.Sprintf(
			"ClusterId = %d\nProcId = %d\nOwner = \"u%d\"\nRequestMemory = %d\nRequestCpus = %d\nExitCode = %d",
			i/10, i%10, i%8, ((i%16)+1)*512, 1+i%8, i%2)); err != nil {
			t.Fatal(err)
		}
	}
}

// TestArchiveSchemaScanOffByDefault pins the default: maintenance must not start reading an
// entire history back on an upgrade. Nothing enables the accelerator unless asked.
func TestArchiveSchemaScanOffByDefault(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{Privileged: true})
	defer cleanup()
	ctx := context.Background()
	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{SegmentSize: 1 << 16}); err != nil {
		t.Fatal(err)
	}
	seedHistory(t, c, "history", 2000)

	diag, err := c.DiagnosticsTable(ctx, "history")
	if err != nil {
		t.Fatal(err)
	}
	if diag.SchemaScan.Enabled {
		t.Error("the columnar accelerator is enabled on an archive with no option set")
	}
}

// TestArchiveSchemaScanReported checks the other half: once built, the archive's diagnostics
// report it, so `.stats history` can say so. archiveDiagJSON left SchemaScan unset before, so
// a built accelerator was invisible on an archive.
func TestArchiveSchemaScanReported(t *testing.T) {
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
	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{SegmentSize: 1 << 16}); err != nil {
		t.Fatal(err)
	}
	seedHistory(t, c, "history", 3000)

	a, ok := cat.ArchiveTable("history")
	if !ok {
		t.Fatal("archive table missing from the catalog")
	}
	if !a.BuildAndEnableSchemaScan(2000, 4) {
		t.Skip("no sealed segments to sample in this fixture")
	}

	diag, err := c.DiagnosticsTable(ctx, "history")
	if err != nil {
		t.Fatal(err)
	}
	ss := diag.SchemaScan
	if !ss.Enabled {
		t.Fatal("archive diagnostics report the accelerator disabled after building it")
	}
	if ss.SchemaFields == 0 {
		t.Error("no schema fields reported for the archive")
	}
	if ss.SealedSegments == 0 || ss.CoveredSegments == 0 {
		t.Errorf("coverage %d/%d: a built archive accelerator should cover its sealed segments",
			ss.CoveredSegments, ss.SealedSegments)
	}
}

// TestArchiveSchemaScanCountsAgree is the correctness guard for routing archive counts through
// a columnar block: the answer must match the row path, over an archive's reverse-ordered,
// zone-mapped, multi-segment layout.
func TestArchiveSchemaScanCountsAgree(t *testing.T) {
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
	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{SegmentSize: 1 << 16}); err != nil {
		t.Fatal(err)
	}
	seedHistory(t, c, "history", 3000)
	a, _ := cat.ArchiveTable("history")

	// Counts via the ordinary aggregate path, before and after enabling the accelerator.
	count := func(constraint string) string {
		rows, err := a.Aggregate(constraint, nil, []db.AggSpec{{Func: db.AggCount, Arg: "*"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			return ""
		}
		return rows[0].Values[0]
	}
	exprs := []string{"RequestMemory >= 4096", "RequestCpus == 4", "ExitCode != 0",
		"RequestMemory >= 2048 && RequestMemory < 8192"}
	want := map[string]string{}
	for _, e := range exprs {
		want[e] = count(e)
	}
	if !a.BuildAndEnableSchemaScan(2000, 4) {
		t.Skip("no sealed segments to sample in this fixture")
	}
	for _, e := range exprs {
		if got := count(e); got != want[e] {
			t.Errorf("%s: %s with the accelerator, %s without", e, got, want[e])
		}
	}
}

// TestArchiveSchemaScanOnWhenConfigured is the other half of TestArchiveSchemaScanOffByDefault:
// with ArchiveSchemaScanHotTopN set, a maintenance pass must actually BUILD the accelerator on an
// archive. Without this, the option could be plumbed as far as the struct and silently do nothing
// -- which is how the mutable side's SchemaScanHotTopN sat at 0 and left the accelerator dark.
func TestArchiveSchemaScanOnWhenConfigured(t *testing.T) {
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
	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{SegmentSize: 1 << 16}); err != nil {
		t.Fatal(err)
	}
	seedHistory(t, c, "history", 3000)

	a, ok := cat.ArchiveTable("history")
	if !ok {
		t.Fatal("archive table missing")
	}
	if a.SchemaScanInfo().Enabled {
		t.Fatal("accelerator already enabled before any maintenance pass")
	}

	// The same call the scheduled pass makes, with the option set.
	s.maintainArchives(db.MaintainOptions{ArchiveSchemaScanHotTopN: 8, SampleMax: 2000})

	info := a.SchemaScanInfo()
	if !info.Enabled {
		t.Fatal("a maintenance pass with ArchiveSchemaScanHotTopN set did NOT enable the accelerator")
	}
	if info.SchemaFields == 0 {
		t.Error("no schema fields after the maintenance build")
	}
	if info.SealedSegments == 0 || info.CoveredSegments != info.SealedSegments {
		t.Errorf("coverage %d/%d after the maintenance build", info.CoveredSegments, info.SealedSegments)
	}
}
