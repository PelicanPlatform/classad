package dbrpc

import (
	"context"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// adminPair returns a client on a DAEMON-privileged connection (admin actions require it).
func adminPair(t *testing.T) (*Client, func()) {
	t.Helper()
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{Privileged: true}) }()
	c := NewClient(cconn)
	return c, func() { c.Close(); s.Close(); d.Close() }
}

func TestDiagnosticsAndAdmin(t *testing.T) {
	c, cleanup := adminPair(t)
	defer cleanup()

	tx, _ := c.Begin(context.Background())
	_ = tx.NewClassAd(context.Background(), "1", "Owner = \"alice\"\nCpus = 4")
	_ = tx.NewClassAd(context.Background(), "2", "Owner = \"bob\"\nCpus = 8")
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Add indexes, then confirm diagnostics reflect them.
	if msg, err := c.Admin(context.Background(), "index.add.categorical", "Owner"); err != nil || msg == "" {
		t.Fatalf("Admin add categorical = %q,%v", msg, err)
	}
	if _, err := c.Admin(context.Background(), "index.add.value", "Cpus"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Admin(context.Background(), "hot.add", "Owner", "Cpus"); err != nil {
		t.Fatal(err)
	}

	d, err := c.Diagnostics(context.Background())
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

	// Build the segment indexes over the existing ads so selectivity stats exist.
	if _, err := c.Admin(context.Background(), "index.reindex"); err != nil {
		t.Fatal(err)
	}

	// Explain: a query on the indexed categorical attribute uses the index.
	ex, err := c.Explain(context.Background(), `Owner == "alice"`)
	if err != nil {
		t.Fatal(err)
	}
	if ex.Plan != "indexed" {
		t.Fatalf("Explain plan = %q, want indexed; probes=%+v", ex.Plan, ex.Probes)
	}
	if ex.IndexUsable != 1 || len(ex.Probes) != 1 || !ex.Probes[0].Indexed {
		t.Fatalf("Explain = %+v, want one indexed probe", ex)
	}
	// Owner == "alice" matches 1 of the 2 ads -> ~50% selectivity estimate.
	p := ex.Probes[0]
	if !p.HasSelectivity || p.EstCandidates != 1 || ex.TotalAds != 2 {
		t.Fatalf("selectivity = %+v (total %d), want ~1 candidate of 2", p, ex.TotalAds)
	}

	// A query on an un-indexed attribute falls back to a scan.
	ex2, err := c.Explain(context.Background(), "Memory > 1024")
	if err != nil {
		t.Fatal(err)
	}
	if ex2.Plan == "indexed" {
		t.Fatalf("Explain(Memory>1024) plan = %q, want a scan", ex2.Plan)
	}

	// Drop and reindex succeed.
	if _, err := c.Admin(context.Background(), "index.drop", "Owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Admin(context.Background(), "index.reindex"); err != nil {
		t.Fatal(err)
	}

	// Rewrite (re-encode with the hot set) and compact succeed, preserving data.
	if msg, err := c.Admin(context.Background(), "rewrite"); err != nil || msg == "" {
		t.Fatalf("Admin rewrite = %q,%v", msg, err)
	}
	if _, err := c.Admin(context.Background(), "compact"); err != nil {
		t.Fatal(err)
	}
	if rows, err := c.Query(context.Background(), "Owner == \"alice\""); err != nil || len(rows) != 1 {
		t.Fatalf("after rewrite/compact Query = %v,%v want 1 row", rows, err)
	}
}

// TestAdminRefusedUnprivileged is the regression for the admin-authz gap: a WRITE-level
// session (not read-only, but not DAEMON-privileged) can read and write ads but must NOT
// be able to retune or restructure the store. Every maintenance/optimization action is
// refused, while ordinary data writes and read-only diagnostics still work.
func TestAdminRefusedUnprivileged(t *testing.T) {
	c, cleanup := testPair(t) // ServeConn default: WRITE-level, not Privileged
	defer cleanup()

	// A normal data write succeeds on this connection.
	tx, _ := c.Begin(context.Background())
	if err := tx.NewClassAd(context.Background(), "1", "Owner = \"alice\""); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Every admin action is refused for a non-DAEMON writer.
	for _, tc := range []struct {
		action string
		args   []string
	}{
		{"index.add.categorical", []string{"Owner"}},
		{"index.add.value", []string{"Cpus"}},
		{"index.drop", []string{"Owner"}},
		{"index.reindex", nil},
		{"compact", nil},
		{"rewrite", nil},
		{"codec.retrain", nil},
		{"hot.add", []string{"Owner"}},
		{"hot.refresh", []string{"100", "8"}},
		{"encrypt.set", []string{"Owner"}},
		{"truncate", nil},
		{"backup.key", nil},
	} {
		if _, err := c.Admin(context.Background(), tc.action, tc.args...); err == nil {
			t.Errorf("admin %q on a WRITE-level connection should be refused", tc.action)
		}
	}

	// Read-only diagnostics remain available to a non-privileged session.
	if _, err := c.Diagnostics(context.Background()); err != nil {
		t.Fatalf("Diagnostics should work for a WRITE-level session: %v", err)
	}
	// And the data write actually landed (proving the connection is functional, not blanket-denied).
	if rows, err := c.Query(context.Background(), `Owner == "alice"`); err != nil || len(rows) != 1 {
		t.Fatalf("data write/read should work: rows=%v err=%v", rows, err)
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

	if _, err := c.Admin(context.Background(), "index.add.value", "Cpus"); err == nil {
		t.Fatal("Admin on a read-only connection should be refused")
	}
	// But diagnostics (read-only) still work.
	if _, err := c.Diagnostics(context.Background()); err != nil {
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
