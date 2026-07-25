package dbrpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// TestArchiveTruncateOverRPC verifies TruncateTable works on an append-only archive (routed
// to archiveAdmin, not the mutable-table admin), is DAEMON-gated, and leaves the archive
// usable for fresh appends afterward -- the runtime "empty history, then re-sync" path.
func TestArchiveTruncateOverRPC(t *testing.T) {
	ctx := context.Background()

	seed := func(c *Client) {
		if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{ValueAttrs: []string{"ClusterId"}}); err != nil {
			t.Fatalf("CreateArchiveTable: %v", err)
		}
		for i := 0; i < 50; i++ {
			if err := c.ArchiveAppend(ctx, "history", fmt.Sprintf("ClusterId = %d\nJobStatus = 4", i)); err != nil {
				t.Fatalf("ArchiveAppend: %v", err)
			}
		}
	}
	countAll := func(c *Client) int {
		got, err := c.ArchiveQuery(ctx, "history", "true", 0)
		if err != nil {
			t.Fatalf("ArchiveQuery: %v", err)
		}
		return len(got)
	}

	// Unprivileged: refused, and the archive is untouched.
	c, cleanup := catServerPair(t, ServeOptions{})
	seed(c)
	if _, err := c.TruncateTable(ctx, "history"); err == nil {
		t.Fatal("archive truncate should be refused without DAEMON privilege")
	}
	if n := countAll(c); n != 50 {
		t.Fatalf("unauthorized truncate changed the archive: %d records, want 50", n)
	}
	cleanup()

	// Privileged: empties the archive, which then accepts new appends.
	c, cleanup = catServerPair(t, ServeOptions{Privileged: true})
	defer cleanup()
	seed(c)
	if n := countAll(c); n != 50 {
		t.Fatalf("setup: %d records, want 50", n)
	}
	if _, err := c.TruncateTable(ctx, "history"); err != nil {
		t.Fatalf("privileged archive truncate: %v", err)
	}
	if n := countAll(c); n != 0 {
		t.Fatalf("after truncate: %d records, want 0", n)
	}
	// Re-ingest a couple records to prove the emptied archive is still live.
	for i := 0; i < 2; i++ {
		if err := c.ArchiveAppend(ctx, "history", fmt.Sprintf("ClusterId = %d\nJobStatus = 4", 900+i)); err != nil {
			t.Fatalf("post-truncate ArchiveAppend: %v", err)
		}
	}
	if n := countAll(c); n != 2 {
		t.Fatalf("after re-append: %d records, want 2", n)
	}
}
