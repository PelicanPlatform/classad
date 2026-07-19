package dbrpc

import (
	"context"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// idemServer wires a client to a fresh server over an in-memory pipe for one db.
func idemServer(t *testing.T, d *db.DB) (*Client, *Server, func()) {
	t.Helper()
	s := NewServer(d)
	cc, sc := netPipe()
	go func() { _ = s.ServeConn(sc) }()
	c := NewClient(cc)
	return c, s, func() { c.Close(); s.Close() }
}

// commitOne begins a txn, writes key=Payload, and commits it idempotently under idem.
func commitOne(t *testing.T, c *Client, key, payload, idem string) error {
	t.Helper()
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.NewClassAd(ctx, key, `Name = "`+key+`"`+"\n"+`Payload = "`+payload+`"`); err != nil {
		t.Fatal(err)
	}
	return tx.CommitIdempotent(ctx, idem)
}

func queryAll(t *testing.T, c *Client) []string {
	t.Helper()
	rows, err := c.Query(context.Background(), "true")
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestCommitIdempotentExactlyOnce: replaying the same unit of work (same idem key)
// with different data does NOT re-apply -- the marker short-circuits it -- while a
// different idem key applies normally.
func TestCommitIdempotentExactlyOnce(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c, _, cleanup := idemServer(t, d)
	defer cleanup()

	// First attempt lands.
	if err := commitOne(t, c, "counter", "first", "unit-1"); err != nil {
		t.Fatal(err)
	}
	// Replay of unit-1 with DIFFERENT data: succeeds but must not overwrite.
	if err := commitOne(t, c, "counter", "second", "unit-1"); err != nil {
		t.Fatalf("idempotent replay should succeed, got %v", err)
	}
	rows := queryAll(t, c)
	joined := strings.Join(rows, "\n")
	if len(rows) != 1 || !strings.Contains(joined, `"first"`) || strings.Contains(joined, `"second"`) {
		t.Fatalf("after replay want single ad Payload=first, got:\n%s", joined)
	}
	// A different unit of work applies normally.
	if err := commitOne(t, c, "counter", "third", "unit-2"); err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(queryAll(t, c), "\n"); !strings.Contains(joined, `"third"`) {
		t.Fatalf("distinct idem key should apply, got:\n%s", joined)
	}
}

// TestCommitIdempotentMarkerInvisible: the durable marker is stored but never
// appears in query results.
func TestCommitIdempotentMarkerInvisible(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c, _, cleanup := idemServer(t, d)
	defer cleanup()

	if err := commitOne(t, c, "a", "x", "unit-1"); err != nil {
		t.Fatal(err)
	}
	rows := queryAll(t, c)
	if len(rows) != 1 {
		t.Fatalf("query returned %d rows, want 1 (marker must be hidden): %v", len(rows), rows)
	}
	if strings.Contains(strings.Join(rows, "\n"), idemMarkerAttr) {
		t.Fatalf("query leaked the idempotency marker: %v", rows)
	}
}

// TestCommitIdempotentDurableAcrossRestart: the marker survives a server+db restart,
// so a replay after reconnecting to a restarted server is still deduplicated.
func TestCommitIdempotentDurableAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	d, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, _, cleanup := idemServer(t, d)
	if err := commitOne(t, c, "k", "first", "unit-1"); err != nil {
		t.Fatal(err)
	}
	cleanup()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart: reopen the same directory, replay unit-1 with different data.
	d2, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	c2, _, cleanup2 := idemServer(t, d2)
	defer cleanup2()
	if err := commitOne(t, c2, "k", "second", "unit-1"); err != nil {
		t.Fatalf("replay after restart should succeed, got %v", err)
	}
	if joined := strings.Join(queryAll(t, c2), "\n"); !strings.Contains(joined, `"first"`) || strings.Contains(joined, `"second"`) {
		t.Fatalf("marker did not survive restart (replay re-applied), got:\n%s", joined)
	}
}

// TestReapIdemMarkers: after the reaper expires a marker, replaying that unit of work
// applies again (idempotency lapses past its retention, as intended).
func TestReapIdemMarkers(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c, s, cleanup := idemServer(t, d)
	defer cleanup()

	if err := commitOne(t, c, "counter", "first", "unit-1"); err != nil {
		t.Fatal(err)
	}
	// Reap everything (maxAge 0 => every marker is already older than the cutoff).
	if n := s.reapIdemMarkers(0); n != 1 {
		t.Fatalf("reaped %d markers, want 1", n)
	}
	// With the marker gone, the same idem key applies again.
	if err := commitOne(t, c, "counter", "second", "unit-1"); err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(queryAll(t, c), "\n"); !strings.Contains(joined, `"second"`) {
		t.Fatalf("after reap, replay should re-apply, got:\n%s", joined)
	}
}
