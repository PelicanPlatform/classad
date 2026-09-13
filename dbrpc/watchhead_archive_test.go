package dbrpc

import (
	"context"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// TestWatchHeadResolvesArchives: an archive can be watched, so it must also be
// possible to ask where its head is.
//
// opWatch resolves a mutable table, a view backing or an archive -- "so one watch
// op serves them all" -- but opWatchHead resolved only mutable tables. The effect
// was that watching an archive from the current head, the "tail from now" path,
// was unreachable: WatchHead answered "no such table" for a table the very next
// op would happily stream. A client was left with a full replay of everything
// rotation still retains, which for a history archive is the expensive case the
// head cursor exists to avoid.
func TestWatchHeadResolvesArchives(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{})
	defer cleanup()
	ctx := context.Background()

	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{}); err != nil {
		t.Fatalf("CreateArchiveTable: %v", err)
	}
	if err := c.ArchiveAppend(ctx, "history", `ClusterId = 1`); err != nil {
		t.Fatalf("ArchiveAppend: %v", err)
	}

	head, err := c.WatchHead(ctx, "history")
	if err != nil {
		t.Fatalf("WatchHead on an archive: %v", err)
	}
	if len(head) == 0 {
		t.Fatal("WatchHead returned an empty cursor: an empty cursor means replay-from-the-start, " +
			"which is the opposite of what the caller asked for")
	}

	// And the cursor has to work where it is meant to be used: tailing from it
	// must deliver what is appended afterwards, and not replay what came before.
	ch, stop, err := c.WatchTable(ctx, "history", head)
	if err != nil {
		t.Fatalf("WatchTable from the head cursor: %v", err)
	}
	defer stop()

	if err := c.ArchiveAppend(ctx, "history", `ClusterId = 2`); err != nil {
		t.Fatalf("ArchiveAppend: %v", err)
	}
	for ev := range ch {
		if ev.Kind != 0 { // upsert
			continue
		}
		if got := ev.AdText; got == "" {
			t.Fatal("an upsert arrived with no ad text")
		}
		// The first record appended before the cursor must not be replayed.
		if strings.Contains(ev.AdText, "ClusterId = 1") {
			t.Errorf("tailing from the head replayed a record committed before it: %s", ev.AdText)
		}
		return
	}
	t.Fatal("the tail delivered nothing after an append")
}
