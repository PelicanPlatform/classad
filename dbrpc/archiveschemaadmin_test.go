package dbrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// These cover schema.fit / schema.rebuild ON AN ARCHIVE, over a catalog server -- the shape a
// deployment actually runs. They exist because the actions shipped mutable-table-only and
// `.schema rebuild` on a history table failed as an unknown admin action: the earlier tests used
// a hand-picked single-table mutable fixture, so they passed while the real case was broken.

// archiveSchemaPair builds a catalog server with a populated archive whose segments seal, and
// returns the client plus the archive table for direct assertions.
func archiveSchemaPair(t *testing.T, n int) (*Client, *db.ArchiveTable, func()) {
	t.Helper()
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{Privileged: true}) }()
	c := NewClient(cconn)
	ctx := context.Background()
	// A small segment size so segments SEAL: only a sealed segment carries a columnar block, so
	// an 8 MiB default would leave nothing for a rebuild to cover and the assertions would be
	// vacuous.
	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{SegmentSize: 1 << 16}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := c.ArchiveAppend(ctx, "history", fmt.Sprintf(
			"ClusterId = %d\nProcId = %d\nOwner = \"u%d\"\nRequestMemory = %d\nRequestCpus = %d\nWallClock = %d.5",
			i/10, i%10, i%8, ((i%16)+1)*512, 1+i%8, i)); err != nil {
			t.Fatal(err)
		}
	}
	a, ok := cat.ArchiveTable("history")
	if !ok {
		t.Fatal("archive table missing from the catalog")
	}
	return c, a, func() { c.Close(); s.Close(); cat.Close() }
}

// TestArchiveSchemaRebuildAdmin is the regression: `.schema rebuild` on a history table returned
// `unknown archive admin action "schema.rebuild"`.
func TestArchiveSchemaRebuildAdmin(t *testing.T) {
	c, a, cleanup := archiveSchemaPair(t, 3000)
	defer cleanup()
	ctx := context.Background()

	// Works from cold: no accelerator yet, so the rebuild is what BUILDS one. An operator asking
	// for a schema on a table that has none must not be told the action does not exist.
	if a.SchemaScanInfo().Enabled {
		t.Fatal("fixture already has an accelerator; the cold-start path would go untested")
	}
	msg, err := c.AdminTable(ctx, "history", "schema.rebuild")
	if err != nil {
		t.Fatalf("schema.rebuild on an archive: %v", err)
	}
	if !strings.Contains(msg, "schema rebuilt") {
		t.Errorf("message = %q, want it to report the rebuild", msg)
	}
	info := a.SchemaScanInfo()
	if !info.Enabled {
		t.Fatal("accelerator still disabled after schema.rebuild on the archive")
	}
	if info.SchemaFields == 0 {
		t.Error("no schema fields after the rebuild")
	}
	if info.SealedSegments == 0 || info.CoveredSegments != info.SealedSegments {
		t.Errorf("coverage %d/%d: the rebuild should cover every sealed segment",
			info.CoveredSegments, info.SealedSegments)
	}

	// And again, now that one exists -- the re-derive path, which is the point of the action.
	if _, err := c.AdminTable(ctx, "history", "schema.rebuild", "2000", "4"); err != nil {
		t.Fatalf("second schema.rebuild with explicit args: %v", err)
	}
}

// TestArchiveSchemaFitAdmin covers the fit report on an archive, both before and after a schema
// exists, since "no schema to measure" and a real report are different outputs.
func TestArchiveSchemaFitAdmin(t *testing.T) {
	c, a, cleanup := archiveSchemaPair(t, 3000)
	defer cleanup()
	ctx := context.Background()

	msg, err := c.AdminTable(ctx, "history", "schema.fit")
	if err != nil {
		t.Fatalf("schema.fit on an archive with no accelerator: %v", err)
	}
	if !strings.Contains(msg, "not enabled") {
		t.Errorf("with no accelerator, message = %q, want it to say so", msg)
	}

	if !a.ReschemaScan(2000, 4) {
		t.Skip("no sealed segments to sample")
	}
	msg, err = c.AdminTable(ctx, "history", "schema.fit")
	if err != nil {
		t.Fatalf("schema.fit after a rebuild: %v", err)
	}
	var rep struct {
		Sampled int                 `json:"sampled"`
		Fields  []db.SchemaFieldFit `json:"fields"`
	}
	if err := json.Unmarshal([]byte(msg), &rep); err != nil {
		t.Fatalf("schema.fit did not return the JSON report: %v (%q)", err, msg)
	}
	if rep.Sampled == 0 || len(rep.Fields) == 0 {
		t.Errorf("empty fit report: %+v", rep)
	}
	for _, f := range rep.Fields {
		if f.Name == "" {
			t.Errorf("field with no name: %+v", f)
		}
	}
}

// TestArchiveSchemaAdminArgErrors pins that the argument checking is the same on both table types
// -- one implementation, so an operator gets the same message wherever they run it.
func TestArchiveSchemaAdminArgErrors(t *testing.T) {
	c, _, cleanup := archiveSchemaPair(t, 500)
	defer cleanup()
	ctx := context.Background()

	for _, tc := range []struct{ action, arg, want string }{
		{"schema.fit", "notanumber", "must be an integer"},
		{"schema.rebuild", "100", "no arguments or"},
	} {
		if _, err := c.AdminTable(ctx, "history", tc.action, tc.arg); err == nil {
			t.Errorf("%s %s: expected an error", tc.action, tc.arg)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %s: error %q, want it to mention %q", tc.action, tc.arg, err, tc.want)
		}
	}
}
