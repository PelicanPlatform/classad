package dbrpc

import (
	"context"
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

	if err := c.CreateArchiveTable(context.Background(), "history", db.ArchiveConfig{ValueAttrs: []string{"ClusterId"}}); err != nil {
		t.Fatalf("CreateArchiveTable: %v", err)
	}
	for i := 0; i < 100; i++ {
		if err := c.ArchiveAppend(context.Background(), "history", fmt.Sprintf("ClusterId = %d\nJobStatus = 4", i)); err != nil {
			t.Fatalf("ArchiveAppend: %v", err)
		}
	}

	// Newest-first, last 3.
	got, err := c.ArchiveQuery(context.Background(), "history", "true", 3)
	if err != nil {
		t.Fatalf("ArchiveQuery: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ArchiveQuery(limit 3) returned %d, want 3", len(got))
	}
	// Constrained query.
	one, err := c.ArchiveQuery(context.Background(), "history", "ClusterId == 42", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 {
		t.Fatalf("ClusterId==42 matched %d, want 1", len(one))
	}

	names, err := c.ArchiveTables(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "history" {
		t.Fatalf("ArchiveTables = %v, want [history]", names)
	}

	// Append is refused on a read-only connection.
	rc, rcleanup := catServerPair(t, ServeOptions{ReadOnly: true})
	defer rcleanup()
	if err := rc.ArchiveAppend(context.Background(), "history", "ClusterId = 1"); err == nil {
		t.Error("archive append should be refused on a read-only connection")
	}
}

// TestArchiveAdminOverRPC covers archive maintenance through the admin RPC path: retrain
// the dictionary, add a value index, and reindex a history table, verifying data stays
// correct and that the actions are DAEMON-gated.
func TestArchiveAdminOverRPC(t *testing.T) {
	ctx := context.Background()
	c, cleanup := catServerPair(t, ServeOptions{Privileged: true})
	defer cleanup()
	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{ValueAttrs: []string{"ClusterId"}}); err != nil {
		t.Fatal(err)
	}
	// Dictionary-friendly, repetitive records so retrain can train.
	for i := 0; i < 400; i++ {
		ad := fmt.Sprintf("ClusterId = %d\nOwner = \"user_%d\"\nCmd = \"/usr/bin/long_repeated_command_path\"\nJobStatus = 4", i, i%20)
		if err := c.ArchiveAppend(ctx, "history", ad); err != nil {
			t.Fatal(err)
		}
	}
	countMatches := func(constraint string) int {
		rows, err := c.ArchiveQuery(ctx, "history", constraint, 0)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	before := countMatches("ClusterId >= 200")

	// Retrain the dictionary (recompresses the archive in place).
	msg, err := c.AdminTable(ctx, "history", "codec.retrain", "1000")
	if err != nil {
		t.Fatalf("archive codec.retrain: %v", err)
	}
	if msg == "" {
		t.Error("retrain returned an empty message")
	}
	if after := countMatches("ClusterId >= 200"); after != before {
		t.Errorf("retrain changed result count: before %d, after %d", before, after)
	}

	// Add a value index on Owner-adjacent attribute, then reindex.
	if _, err := c.AdminTable(ctx, "history", "index.add.value", "ClusterId"); err != nil {
		t.Fatalf("archive index.add.value: %v", err)
	}
	if _, err := c.AdminTable(ctx, "history", "index.reindex"); err != nil {
		t.Fatalf("archive index.reindex: %v", err)
	}
	if after := countMatches("ClusterId >= 200"); after != before {
		t.Errorf("reindex changed result count: before %d, after %d", before, after)
	}
	// A specific record is still findable.
	if n := countMatches("ClusterId == 321"); n != 1 {
		t.Errorf("ClusterId==321 matched %d after maintenance, want 1", n)
	}

	// Unknown archive admin action errors.
	if _, err := c.AdminTable(ctx, "history", "encrypt.set", "X"); err == nil {
		t.Error("encrypt.set should be rejected for an archive table")
	}

	// Admin is DAEMON-gated: a non-privileged connection is refused.
	rc, rcleanup := catServerPair(t, ServeOptions{})
	defer rcleanup()
	if err := rc.CreateArchiveTable(ctx, "history", db.ArchiveConfig{}); err != nil {
		// created on its own catalog; ignore
	}
	if _, err := rc.AdminTable(ctx, "history", "codec.retrain"); err == nil {
		t.Error("archive admin should require DAEMON authorization")
	}
}

// TestArchiveAggregateOverRPC covers the server-side archive GROUP BY through the client
// API: COUNT(*), COUNT with a WHERE constraint, and GROUP BY Owner. Each grouped result is
// checked against a client-side count over the same rows fetched via ArchiveQuery, proving
// the server aggregate agrees with counting locally.
func TestArchiveAggregateOverRPC(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{})
	defer cleanup()

	if err := c.CreateArchiveTable(context.Background(), "history", db.ArchiveConfig{
		ValueAttrs: []string{"ClusterId"},
		ZoneAttrs:  []string{"ClusterId"},
	}); err != nil {
		t.Fatalf("CreateArchiveTable: %v", err)
	}
	owners := []string{"alice", "bob", "alice", "carol", "bob", "alice"}
	cpus := []int{4, 16, 8, 1, 4, 2}
	for i, o := range owners {
		if err := c.ArchiveAppend(context.Background(), "history",
			fmt.Sprintf("ClusterId = %d\nOwner = %q\nCpus = %d", i, o, cpus[i])); err != nil {
			t.Fatalf("ArchiveAppend: %v", err)
		}
	}

	// COUNT(*) over the whole table, checked against ArchiveQuery's row count.
	all, err := c.ArchiveQuery(context.Background(), "history", "true", 0)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := c.ArchiveAggregate(context.Background(), "history", "true", nil, []AggSpec{{Func: AggCount, Arg: "*"}})
	if err != nil {
		t.Fatalf("ArchiveAggregate COUNT(*): %v", err)
	}
	if len(rows) != 1 || rows[0].Values[0] != fmt.Sprint(len(all)) {
		t.Fatalf("COUNT(*) = %+v, want %d", rows, len(all))
	}

	// COUNT with a WHERE constraint, checked against the constrained ArchiveQuery.
	matched, err := c.ArchiveQuery(context.Background(), "history", "ClusterId >= 3", 0)
	if err != nil {
		t.Fatal(err)
	}
	rows, err = c.ArchiveAggregate(context.Background(), "history", "ClusterId >= 3", nil, []AggSpec{{Func: AggCount, Arg: "*"}})
	if err != nil {
		t.Fatalf("ArchiveAggregate constrained COUNT: %v", err)
	}
	if len(rows) != 1 || rows[0].Values[0] != fmt.Sprint(len(matched)) {
		t.Fatalf("COUNT(*) WHERE ClusterId>=3 = %+v, want %d", rows, len(matched))
	}

	// GROUP BY Owner COUNT(*): assert against a client-side count over every row.
	wantCounts := map[string]int{}
	for _, o := range owners {
		wantCounts[o]++
	}
	rows, err = c.ArchiveAggregate(context.Background(), "history", "true", []string{"Owner"}, []AggSpec{{Func: AggCount, Arg: "*"}})
	if err != nil {
		t.Fatalf("ArchiveAggregate GROUP BY Owner: %v", err)
	}
	if len(rows) != len(wantCounts) {
		t.Fatalf("GROUP BY Owner produced %d groups, want %d: %+v", len(rows), len(wantCounts), rows)
	}
	for _, r := range rows {
		if got := r.Values[0]; got != fmt.Sprint(wantCounts[r.Group[0]]) {
			t.Errorf("group %q count = %s, want %d", r.Group[0], got, wantCounts[r.Group[0]])
		}
	}
}
