package dbrpc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// archiveFixture is a catalog holding one mutable table and one archive, served on one
// DAEMON connection.
func archiveFixture(t *testing.T) *Client {
	t.Helper()
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.CreateTable("jobs"); err != nil {
		t.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", db.ArchiveConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AppendOld("ClusterId = 1"); err != nil {
		t.Fatal(err)
	}
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true, Privileged: true}) }()
	c := NewClient(cconn)
	t.Cleanup(func() { c.Close(); s.Close(); cat.Close() })
	return c
}

// TestArchiveRefusalClassified: an op that does not serve an archive says so, names what
// to use instead, and is classifiable without matching prose. It must NOT say the table
// does not exist -- that answer sent a caller looking for a table that was there all
// along, and it is the whole reason this refusal has its own prefix.
func TestArchiveRefusalClassified(t *testing.T) {
	c := archiveFixture(t)
	_, err := c.QueryTable(context.Background(), "history", "true", 0)
	if err == nil {
		t.Fatal("QueryTable on an archive succeeded; this stage does not route it")
	}
	if !errors.Is(err, ErrArchiveTable) {
		t.Fatalf("err = %v, want errors.Is(ErrArchiveTable)", err)
	}
	var se *ServerError
	if !errors.As(err, &se) {
		t.Fatalf("err = %T %v, want a *ServerError", err, err)
	}
	if strings.Contains(se.Msg, "no such table") {
		t.Fatalf("refusal still denies the table exists: %q", se.Msg)
	}
	if !strings.Contains(se.Msg, `"history"`) {
		t.Fatalf("refusal %q does not name the archive", se.Msg)
	}
	if !strings.Contains(se.Msg, "ArchiveQuery") {
		t.Fatalf("refusal %q does not name the op that does read an archive", se.Msg)
	}
	// A name that really is absent keeps the old answer, so a client that reads
	// "no such table" still learns what it has always meant.
	_, err = c.QueryTable(context.Background(), "absent", "true", 0)
	if err == nil || !strings.Contains(err.Error(), "no such table: absent") {
		t.Fatalf("missing table: err = %v, want \"no such table: absent\"", err)
	}
	if errors.Is(err, ErrArchiveTable) {
		t.Fatalf("a missing table must not classify as ErrArchiveTable: %v", err)
	}
}

// TestListTablesIncludesArchives: the op whose job is to say what exists lists archives
// too, once each, merged in sorted order with the mutable tables.
func TestListTablesIncludesArchives(t *testing.T) {
	c := archiveFixture(t)
	names, err := c.Tables(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, n := range names {
		seen[n]++
	}
	if seen["history"] != 1 {
		t.Fatalf("Tables() = %v, want exactly one \"history\"", names)
	}
	if seen["jobs"] != 1 {
		t.Fatalf("Tables() = %v, want exactly one \"jobs\"", names)
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("Tables() = %v, not sorted", names)
		}
	}
	// The archive listing still answers on its own, so a client can still tell the kinds
	// apart -- and one that unions the two lists must deduplicate rather than concatenate.
	archives, err := c.ArchiveTables(context.Background())
	if err != nil || len(archives) != 1 || archives[0] != "history" {
		t.Fatalf("ArchiveTables() = %v, %v; want [history]", archives, err)
	}
}
