package dbrpc

import (
	"context"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

func TestRPCDeleteWhere(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()

	tx, err := c.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.NewClassAd(context.Background(), "a", `Name = "a"`+"\n"+`State = "Idle"`)
	_ = tx.NewClassAd(context.Background(), "b", `Name = "b"`+"\n"+`State = "Claimed"`)
	_ = tx.NewClassAd(context.Background(), "c", `Name = "c"`+"\n"+`State = "Idle"`)
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Bulk delete-by-constraint in one server-side call.
	n, err := c.DeleteWhere(context.Background(), `State == "Idle"`)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("DeleteWhere removed %d, want 2", n)
	}
	rows, err := c.Query(context.Background(), "true")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("after delete %d ads remain, want 1 (the Claimed one)", len(rows))
	}
}

// TestRPCDeleteWhereReadOnly confirms the pushdown is a mutating op: a read-only
// connection refuses it.
func TestRPCDeleteWhereReadOnly(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{ReadOnly: true}) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); d.Close() }()

	if _, err := c.DeleteWhere(context.Background(), "true"); err == nil {
		t.Fatal("DeleteWhere on a read-only connection should be refused, got nil error")
	}
}
