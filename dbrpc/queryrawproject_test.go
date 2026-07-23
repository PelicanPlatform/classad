package dbrpc

import (
	"context"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestQueryRawProject verifies server-side projection: each returned ad carries
// only the requested attributes (matched case-insensitively) plus
// MyType/TargetType, and nothing else.
func TestQueryRawProject(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	ctx := context.Background()

	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.NewClassAd(ctx, "a", `MyType = "Machine"`+"\n"+`Name = "slot1"`+"\n"+`Cpus = 8`+"\n"+`Memory = 4096`+"\n"+`State = "Idle"`)
	_ = tx.NewClassAd(ctx, "b", `MyType = "Machine"`+"\n"+`Name = "slot2"`+"\n"+`Cpus = 4`+"\n"+`Memory = 2048`+"\n"+`State = "Claimed"`)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Project to Name + Cpus (lowercase on the wire to exercise case-insensitivity).
	rows, err := c.QueryRawProject(ctx, DefaultTable, "true", []string{"name", "cpus"}, 0)
	if err != nil {
		t.Fatalf("QueryRawProject: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d ads, want 2", len(rows))
	}
	for _, r := range rows {
		ad, err := classad.ParseOld(r)
		if err != nil {
			t.Fatalf("parse projected ad %q: %v", r, err)
		}
		if _, ok := ad.Lookup("Name"); !ok {
			t.Errorf("projected ad missing Name: %q", r)
		}
		if _, ok := ad.Lookup("Cpus"); !ok {
			t.Errorf("projected ad missing Cpus: %q", r)
		}
		if mt, _ := ad.EvaluateAttrString("MyType"); mt != "Machine" {
			t.Errorf("projected ad lost MyType (got %q): %q", mt, r)
		}
		if _, ok := ad.Lookup("Memory"); ok {
			t.Errorf("projected ad should not carry Memory: %q", r)
		}
		if _, ok := ad.Lookup("State"); ok {
			t.Errorf("projected ad should not carry State: %q", r)
		}
		// Belt-and-suspenders: the raw text must not even mention the dropped attrs.
		if strings.Contains(r, "Memory") || strings.Contains(r, "State") {
			t.Errorf("projected wire text carries a dropped attribute: %q", r)
		}
	}
}
