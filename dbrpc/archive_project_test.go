package dbrpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// TestArchiveRawProjectOverRPC verifies the server-side projection op (QueryRawProject) works
// on an append-only archive (history) table -- the same op that serves mutable tables -- so a
// narrow SELECT over history ships only the requested attributes. It checks newest-first
// order, the pushed-down LIMIT, a constraint, and that unselected attributes are absent.
func TestArchiveRawProjectOverRPC(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{})
	defer cleanup()
	ctx := context.Background()

	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{ValueAttrs: []string{"ClusterId"}}); err != nil {
		t.Fatalf("CreateArchiveTable: %v", err)
	}
	// Wide-ish records: only Owner + ClusterId are projected; JobStatus/Cmd must not appear.
	for i := 0; i < 100; i++ {
		ad := fmt.Sprintf("ClusterId = %d\nOwner = \"u%d\"\nJobStatus = 4\nCmd = \"/bin/run%d\"", i, i%10, i)
		if err := c.ArchiveAppend(ctx, "history", ad); err != nil {
			t.Fatalf("ArchiveAppend: %v", err)
		}
	}

	// Newest-first + LIMIT 3: the last three appended (ClusterId 99, 98, 97).
	rows, err := c.QueryRawProject(ctx, "history", "true", []string{"Owner", "ClusterId"}, 3)
	if err != nil {
		t.Fatalf("QueryRawProject: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("projected archive query returned %d rows, want 3", len(rows))
	}
	wantCluster := []int64{99, 98, 97}
	for i, r := range rows {
		ad, perr := classad.ParseOld(r)
		if perr != nil {
			t.Fatalf("parse projected ad %q: %v", r, perr)
		}
		cid, ok := ad.EvaluateAttrInt("ClusterId")
		if !ok {
			t.Fatalf("row %d missing ClusterId: %q", i, r)
		}
		if cid != wantCluster[i] {
			t.Errorf("row %d ClusterId = %d, want %d (newest-first order wrong)", i, cid, wantCluster[i])
		}
		if _, ok := ad.Lookup("Owner"); !ok {
			t.Errorf("row %d missing the projected Owner: %q", i, r)
		}
		// Unselected attributes must not cross the wire.
		if _, ok := ad.Lookup("JobStatus"); ok {
			t.Errorf("row %d carries unselected JobStatus: %q", i, r)
		}
		if _, ok := ad.Lookup("Cmd"); ok {
			t.Errorf("row %d carries unselected Cmd: %q", i, r)
		}
	}

	// A constraint is applied server-side: exactly one record has ClusterId == 42.
	one, err := c.QueryRawProject(ctx, "history", "ClusterId == 42", []string{"Owner"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 {
		t.Fatalf("ClusterId==42 projected query matched %d, want 1", len(one))
	}
}
