package dbrpc

import (
	"context"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
)

// txnCount counts the server's live transactions.
func txnCount(s *Server) int {
	n := 0
	s.txns.Range(func(_, _ any) bool { n++; return true })
	return n
}

// TestServerAbortsTxnsOnDisconnect: a client that opens a transaction and then drops
// its connection (a transient reset, a crash) must not leak the server-side txn --
// the serve loop aborts it when the connection closes.
func TestServerAbortsTxnsOnDisconnect(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := NewServer(d)
	defer s.Close()
	cc, sc := netPipe()
	go func() { _ = s.ServeConn(sc) }()
	c := NewClient(cc)

	tx, err := c.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.NewClassAd(context.Background(), "k", `Name = "a"`); err != nil {
		t.Fatal(err)
	}
	if txnCount(s) != 1 {
		t.Fatalf("server txn count = %d before disconnect, want 1", txnCount(s))
	}

	// Drop the connection mid-transaction (no commit, no abort).
	_ = c.Close()

	// The serve loop's defer aborts the orphaned txn; poll until it drains.
	deadline := time.Now().Add(2 * time.Second)
	for txnCount(s) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("server txn count = %d 2s after disconnect, want 0 (txn leaked)", txnCount(s))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestReapIdleTxns: a transaction abandoned on a still-open connection is aborted by
// the idle reaper.
func TestReapIdleTxns(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := NewServer(d)
	defer s.Close()
	cc, sc := netPipe()
	go func() { _ = s.ServeConn(sc) }()
	c := NewClient(cc)
	defer c.Close()

	if _, err := c.Begin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if txnCount(s) != 1 {
		t.Fatalf("server txn count = %d, want 1", txnCount(s))
	}

	// maxIdle 0: every txn is at least "0 ago" idle, so all are reaped.
	if n := s.reapIdleTxns(0); n != 1 {
		t.Fatalf("reapIdleTxns reaped %d, want 1", n)
	}
	if txnCount(s) != 0 {
		t.Fatalf("server txn count = %d after reap, want 0", txnCount(s))
	}
}
