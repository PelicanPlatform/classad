package dbrpc

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// TestExplainArchiveTable is the regression for `.explain` on a history table reporting
// "no such table" while queries against that same table worked: the handler resolved only
// the mutable catalog. An archive is a different table type, not an absent one.
func TestExplainArchiveTable(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{Privileged: true})
	defer cleanup()
	ctx := context.Background()

	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{ValueAttrs: []string{"ProcId"}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if err := c.ArchiveAppend(ctx, "history", fmt.Sprintf(
			"ClusterId = %d\nProcId = %d\nOwner = \"u%d\"", i, i%16, i%4)); err != nil {
			t.Fatal(err)
		}
	}

	// The plain case the user hits: explain a match-all over the archive.
	ex, err := c.ExplainTable(ctx, "history", "true")
	if err != nil {
		t.Fatalf("explain on an archive: %v", err)
	}
	if ex.Plan == "" {
		t.Errorf("explain returned no plan: %+v", ex)
	}
	if ex.TotalAds != 50 {
		t.Errorf("TotalAds = %d, want 50", ex.TotalAds)
	}

	// A constraint over a value-indexed attribute should be reported as index-usable, which
	// is the whole reason to run explain on a history table.
	ex, err = c.ExplainTable(ctx, "history", "ProcId == 3")
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Probes) == 0 {
		t.Errorf("no probes reported for an indexed attribute: %+v", ex)
	}

	// A malformed constraint is an error about the constraint, not about the table.
	if _, err := c.ExplainTable(ctx, "history", "ProcId =="); err == nil {
		t.Error("expected an error for a malformed constraint")
	} else if strings.Contains(err.Error(), "no such table") {
		t.Errorf("malformed constraint reported as a missing table: %v", err)
	}

	// A genuinely absent table still says so.
	if _, err := c.ExplainTable(ctx, "nope", "true"); err == nil ||
		!strings.Contains(err.Error(), "no such table") {
		t.Errorf("err = %v, want a no-such-table error", err)
	}
}
