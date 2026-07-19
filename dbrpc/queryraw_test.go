package dbrpc

import (
	"context"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// TestRPCQueryRawPrivileged: a privileged connection (as the collector uses) gets
// the full ad in old-ClassAd wire text, private attributes included.
func TestRPCQueryRawPrivileged(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cc, sc := netPipe()
	go func() { _ = s.ServeConnOpts(sc, ServeOptions{IncludePrivate: true}) }()
	c := NewClient(cc)
	defer func() { c.Close(); s.Close(); d.Close() }()

	tx, err := c.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.NewClassAd(context.Background(), "a", "MyType = \"Machine\"\nState = \"Idle\"\nCapability = \"secret-claim\"")
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	rows, err := c.QueryRaw(context.Background(), "true")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("QueryRaw returned %d rows, want 1", len(rows))
	}
	text := rows[0]
	for _, want := range []string{"MyType", "Machine", "State", "Capability", "secret-claim"} {
		if !strings.Contains(text, want) {
			t.Fatalf("privileged QueryRaw text missing %q:\n%s", want, text)
		}
	}
}

// TestRPCQueryRawStripsPrivate: a non-privileged connection gets the same ad in
// wire text but with private attributes removed -- while public attributes and
// the type tag survive.
func TestRPCQueryRawStripsPrivate(t *testing.T) {
	c, cleanup := testPair(t) // default ServeOptions: not privileged
	defer cleanup()

	tx, err := c.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.NewClassAd(context.Background(), "a", "MyType = \"Machine\"\nState = \"Idle\"\nCapability = \"secret-claim\"")
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	rows, err := c.QueryRaw(context.Background(), "true")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("QueryRaw returned %d rows, want 1", len(rows))
	}
	text := rows[0]
	if strings.Contains(text, "Capability") || strings.Contains(text, "secret-claim") {
		t.Fatalf("non-privileged QueryRaw leaked a private attribute:\n%s", text)
	}
	for _, want := range []string{"State", "Idle", "Machine"} {
		if !strings.Contains(text, want) {
			t.Fatalf("non-privileged QueryRaw dropped public content %q:\n%s", want, text)
		}
	}
}
