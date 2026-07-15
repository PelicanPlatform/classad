package dbrpc

import (
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

func TestDiagnosticsAndAdmin(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()

	tx, _ := c.Begin()
	_ = tx.NewClassAd("1", "Owner = \"alice\"\nCpus = 4")
	_ = tx.NewClassAd("2", "Owner = \"bob\"\nCpus = 8")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Add indexes, then confirm diagnostics reflect them.
	if msg, err := c.Admin("index.add.categorical", "Owner"); err != nil || msg == "" {
		t.Fatalf("Admin add categorical = %q,%v", msg, err)
	}
	if _, err := c.Admin("index.add.value", "Cpus"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Admin("hot.add", "Owner", "Cpus"); err != nil {
		t.Fatal(err)
	}

	d, err := c.Diagnostics()
	if err != nil {
		t.Fatal(err)
	}
	if d.Stats.Ads != 2 {
		t.Fatalf("Stats.Ads = %d, want 2", d.Stats.Ads)
	}
	if !contains(d.CategoricalIndexes, "Owner") {
		t.Fatalf("categorical indexes = %v, want Owner", d.CategoricalIndexes)
	}
	if !contains(d.ValueIndexes, "Cpus") {
		t.Fatalf("value indexes = %v, want Cpus", d.ValueIndexes)
	}
	if !contains(d.Hot, "Owner") || !contains(d.Hot, "Cpus") {
		t.Fatalf("hot attrs = %v, want Owner and Cpus", d.Hot)
	}

	// Explain: a query on the indexed categorical attribute uses the index.
	ex, err := c.Explain(`Owner == "alice"`)
	if err != nil {
		t.Fatal(err)
	}
	if ex.Plan != "indexed" {
		t.Fatalf("Explain plan = %q, want indexed; probes=%+v", ex.Plan, ex.Probes)
	}
	if ex.IndexUsable != 1 || len(ex.Probes) != 1 || !ex.Probes[0].Indexed {
		t.Fatalf("Explain = %+v, want one indexed probe", ex)
	}

	// A query on an un-indexed attribute falls back to a scan.
	ex2, err := c.Explain("Memory > 1024")
	if err != nil {
		t.Fatal(err)
	}
	if ex2.Plan == "indexed" {
		t.Fatalf("Explain(Memory>1024) plan = %q, want a scan", ex2.Plan)
	}

	// Drop and reindex succeed.
	if _, err := c.Admin("index.drop", "Owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Admin("index.reindex"); err != nil {
		t.Fatal(err)
	}
}

// TestAdminRefusedReadOnly confirms management is refused on a read-only conn.
func TestAdminRefusedReadOnly(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{ReadOnly: true}) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); d.Close() }()

	if _, err := c.Admin("index.add.value", "Cpus"); err == nil {
		t.Fatal("Admin on a read-only connection should be refused")
	}
	// But diagnostics (read-only) still work.
	if _, err := c.Diagnostics(); err != nil {
		t.Fatalf("Diagnostics should work read-only: %v", err)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
