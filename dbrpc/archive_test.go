package dbrpc

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// catServerPair wires a client to a catalog-backed server over a persistent catalog.
func catServerPair(t *testing.T, opts ServeOptions) (*Client, func()) {
	t.Helper()
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, opts) }()
	c := NewClient(cconn)
	return c, func() { c.Close(); s.Close(); cat.Close() }
}

// TestArchiveOverRPC covers create, append, newest-first + LIMIT query, and list of an
// archive (history) table through the client API.
func TestArchiveOverRPC(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{})
	defer cleanup()

	if err := c.CreateArchiveTable("history", db.ArchiveConfig{ValueAttrs: []string{"ClusterId"}}); err != nil {
		t.Fatalf("CreateArchiveTable: %v", err)
	}
	for i := 0; i < 100; i++ {
		if err := c.ArchiveAppend("history", fmt.Sprintf("ClusterId = %d\nJobStatus = 4", i)); err != nil {
			t.Fatalf("ArchiveAppend: %v", err)
		}
	}

	// Newest-first, last 3.
	got, err := c.ArchiveQuery("history", "true", 3)
	if err != nil {
		t.Fatalf("ArchiveQuery: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ArchiveQuery(limit 3) returned %d, want 3", len(got))
	}
	// Constrained query.
	one, err := c.ArchiveQuery("history", "ClusterId == 42", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 {
		t.Fatalf("ClusterId==42 matched %d, want 1", len(one))
	}

	names, err := c.ArchiveTables()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "history" {
		t.Fatalf("ArchiveTables = %v, want [history]", names)
	}

	// Append is refused on a read-only connection.
	rc, rcleanup := catServerPair(t, ServeOptions{ReadOnly: true})
	defer rcleanup()
	if err := rc.ArchiveAppend("history", "ClusterId = 1"); err == nil {
		t.Error("archive append should be refused on a read-only connection")
	}
}
