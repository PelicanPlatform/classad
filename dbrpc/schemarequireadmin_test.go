package dbrpc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// schema.require over the wire, on an archive: the knob is useless if the only way to reach it is
// a recompile, so the admin verbs are part of the feature rather than a convenience on top of it.
func TestArchiveSchemaRequireAdmin(t *testing.T) {
	c, a, cleanup := archiveSchemaPair(t, 3000)
	defer cleanup()
	ctx := context.Background()

	msg, err := c.AdminTable(ctx, "history", "schema.require.add", "WallClock")
	if err != nil {
		t.Fatalf("schema.require.add: %v", err)
	}
	// Changing the set is only half the operation; the response has to say what makes it real,
	// because nothing else will and the rebuild costs the whole table.
	if !strings.Contains(msg, "schema.rebuild") {
		t.Errorf("message = %q, want it to name the rebuild the change needs", msg)
	}
	if got := a.SchemaRequireAttrs(); len(got) != 1 || got[0] != "wallclock" {
		t.Fatalf("required set = %v, want [wallclock]", got)
	}

	msg, err = c.AdminTable(ctx, "history", "schema.require")
	if err != nil {
		t.Fatalf("schema.require: %v", err)
	}
	var rep struct {
		Require []string               `json:"require"`
		Skipped []db.SchemaRequireSkip `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(msg), &rep); err != nil {
		t.Fatalf("schema.require did not return JSON: %v (%q)", err, msg)
	}
	if len(rep.Require) != 1 || rep.Require[0] != "wallclock" {
		t.Errorf("listed set = %v, want [wallclock]", rep.Require)
	}

	// Re-adding the same attribute is a no-op and must say so rather than implying a rebuild is
	// owed when nothing changed.
	msg, err = c.AdminTable(ctx, "history", "schema.require.add", "wallclock")
	if err != nil {
		t.Fatalf("idempotent schema.require.add: %v", err)
	}
	if !strings.Contains(msg, "unchanged") {
		t.Errorf("message = %q, want it to report no change", msg)
	}

	if _, err := c.AdminTable(ctx, "history", "schema.require.drop", "WallClock"); err != nil {
		t.Fatalf("schema.require.drop: %v", err)
	}
	if got := a.SchemaRequireAttrs(); len(got) != 0 {
		t.Errorf("required set after drop = %v, want empty", got)
	}

	if _, err := c.AdminTable(ctx, "history", "schema.require.add"); err == nil {
		t.Error("schema.require.add with no attribute was accepted")
	}
}

// The same verbs on a mutable table, via the single-table server. One implementation serves both,
// and the only way to keep it that way is to exercise both.
func TestTableSchemaRequireAdmin(t *testing.T) {
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{Privileged: true}) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); d.Close() }()
	ctx := context.Background()

	if _, err := c.Admin(ctx, "schema.require.add", "ResidentSetSize"); err != nil {
		t.Fatalf("schema.require.add on a mutable table: %v", err)
	}
	if got := d.SchemaRequireAttrs(); len(got) != 1 || got[0] != "residentsetsize" {
		t.Fatalf("required set = %v, want [residentsetsize]", got)
	}
	msg, err := c.Admin(ctx, "schema.require")
	if err != nil {
		t.Fatalf("schema.require: %v", err)
	}
	if !strings.Contains(msg, "residentsetsize") {
		t.Errorf("listing = %q, want it to name the required attribute", msg)
	}
}
