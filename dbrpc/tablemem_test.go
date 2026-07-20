package dbrpc

import (
	"context"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// TestTableMemoryOps covers the wire ops for RAM-only tables: creating one, converting an
// existing persistent table (DAEMON-gated), and the WRITE-level refusal of convert.
func TestTableMemoryOps(t *testing.T) {
	cat, err := db.OpenCatalog(t.TempDir()) // persistent catalog
	if err != nil {
		t.Fatal(err)
	}
	s := NewServerCatalog(cat)
	ctx := context.Background()

	// Privileged (DAEMON) connection.
	dcc, dsc := netPipe()
	go func() { _ = s.ServeConnOpts(dsc, ServeOptions{Privileged: true}) }()
	dc := NewClient(dcc)
	// Ordinary WRITE connection (not privileged, not read-only).
	wcc, wsc := netPipe()
	go func() { _ = s.ServeConn(wsc) }()
	wc := NewClient(wcc)
	defer func() { dc.Close(); wc.Close(); s.Close(); cat.Close() }()

	// CreateTableInMemory yields a RAM-only table even on a persistent catalog.
	if err := dc.CreateTableInMemory(ctx, "ephemeral"); err != nil {
		t.Fatalf("CreateTableInMemory: %v", err)
	}
	if d, ok := cat.Table("ephemeral"); !ok || !d.InMemory() {
		t.Fatalf("ephemeral not created in-memory (ok=%v)", ok)
	}

	// A persistent table with data, then converted to RAM-only.
	if err := dc.CreateTable(ctx, "jobs"); err != nil {
		t.Fatal(err)
	}
	if d, _ := cat.Table("jobs"); d.InMemory() {
		t.Fatal("jobs should start persistent")
	}
	tx, err := wc.BeginTable(ctx, "jobs")
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.NewClassAd(ctx, "1", `Owner = "alice"`)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// A WRITE-level session may NOT convert (DAEMON-gated).
	if err := wc.ConvertTableToMemory(ctx, "jobs"); err == nil {
		t.Fatal("ConvertTableToMemory on a WRITE connection should be refused")
	}
	// The DAEMON session may, and the data is preserved.
	if err := dc.ConvertTableToMemory(ctx, "jobs"); err != nil {
		t.Fatalf("ConvertTableToMemory (privileged): %v", err)
	}
	if d, _ := cat.Table("jobs"); !d.InMemory() {
		t.Fatal("jobs should be in-memory after convert")
	}
	rows, err := dc.QueryTable(ctx, "jobs", "true", 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("after convert QueryTable = %v (n=%d), want 1 row", err, len(rows))
	}
}
