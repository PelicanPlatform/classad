package dbrpc

import (
	"context"
	"sort"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// TestRPCQueryKeys: the keys-by-constraint op returns the real storage keys of matching rows --
// even rows that carry no "Key" attribute (the whole point: UPDATE/DELETE can address them).
func TestRPCQueryKeys(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	ctx := context.Background()

	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// None of these carry a "Key" attribute; the storage key is supplied separately.
	_ = tx.NewClassAd(ctx, "0O.-1", `MyType = "Owner"`+"\n"+`State = "Idle"`)
	_ = tx.NewClassAd(ctx, "0O.-2", `MyType = "Owner"`+"\n"+`State = "Idle"`)
	_ = tx.NewClassAd(ctx, "1.0", `MyType = "Job"`+"\n"+`State = "Claimed"`)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := c.QueryKeysTable(ctx, DefaultTable, `MyType == "Owner"`)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{"0O.-1", "0O.-2"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("QueryKeysTable = %v, want %v (real storage keys of the Key-less Owner rows)", got, want)
	}

	// A constraint matching nothing yields no keys (not an error).
	none, err := c.QueryKeysTable(ctx, DefaultTable, `MyType == "Nonesuch"`)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("no-match query returned %v, want empty", none)
	}

	// A bad constraint is a clean error, not a panic/hang.
	if _, err := c.QueryKeysTable(ctx, DefaultTable, `State ==`); err == nil {
		t.Fatal("a malformed constraint should error")
	}
}

// TestRPCQueryKeysReadOnly confirms keys-by-constraint is a read (allowed on a read-only conn).
func TestRPCQueryKeysReadOnly(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	// Seed a Key-less row via the store directly.
	ad, err := classad.ParseOld(`MyType = "Owner"`)
	if err != nil {
		t.Fatal(err)
	}
	tx := d.Begin()
	tx.NewClassAd("k1", ad)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{ReadOnly: true}) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); d.Close() }()

	keys, err := c.QueryKeysTable(context.Background(), DefaultTable, `MyType == "Owner"`)
	if err != nil {
		t.Fatalf("QueryKeys on a read-only connection should be allowed: %v", err)
	}
	if len(keys) != 1 || keys[0] != "k1" {
		t.Fatalf("QueryKeys = %v, want [k1]", keys)
	}
}
