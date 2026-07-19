package dbrpc

import (
	"context"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

func TestMultiTable(t *testing.T) {
	cat, err := db.OpenCatalog("") // in-memory catalog
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.CreateTable("machines"); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.CreateTable("jobs"); err != nil {
		t.Fatal(err)
	}
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(sconn) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); cat.Close() }()

	// Writes route by table.
	tx, err := c.BeginTable(context.Background(), "machines")
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.NewClassAd(context.Background(), "slot1", "Name = \"slot1\"\nCpus = 8")
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rows, err := c.QueryTable(context.Background(), "machines", "true", 0); err != nil || len(rows) != 1 {
		t.Fatalf("machines rows = %d, %v; want 1", len(rows), err)
	}
	if rows, err := c.QueryTable(context.Background(), "jobs", "true", 0); err != nil || len(rows) != 0 {
		t.Fatalf("jobs rows = %d, %v; want 0", len(rows), err)
	}

	// List / create / drop via the client.
	if names, err := c.Tables(context.Background()); err != nil || len(names) != 2 || names[0] != "jobs" || names[1] != "machines" {
		t.Fatalf("Tables() = %v, %v; want [jobs machines]", names, err)
	}
	if err := c.CreateTable(context.Background(), "submitters"); err != nil {
		t.Fatal(err)
	}
	if names, _ := c.Tables(context.Background()); len(names) != 3 {
		t.Fatalf("after create Tables() = %v, want 3", names)
	}
	// A query on a missing table errors.
	if _, err := c.QueryTable(context.Background(), "nope", "true", 0); err == nil {
		t.Fatal("query on a nonexistent table should error")
	}
	if err := c.DropTable(context.Background(), "machines"); err != nil {
		t.Fatal(err)
	}
	if names, _ := c.Tables(context.Background()); len(names) != 2 || names[0] != "jobs" || names[1] != "submitters" {
		t.Fatalf("after drop Tables() = %v; want [jobs submitters]", names)
	}
}

// TestSingleServerRejectsTableOps confirms the single-DB server (NewServer) still
// serves the default table and refuses catalog mutations.
func TestSingleServerRejectsTableOps(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	if names, err := c.Tables(context.Background()); err != nil || len(names) != 1 || names[0] != DefaultTable {
		t.Fatalf("Tables() = %v, %v; want [ads]", names, err)
	}
	if err := c.CreateTable(context.Background(), "x"); err == nil {
		t.Fatal("single-DB server should reject CreateTable")
	}
}
