package dbrpc

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
)

// TestArchiveWatchOverRPC verifies WatchTable works on an append-only archive (history) table:
// a fresh watch replays the retained records as upserts, signals WatchSynced, then delivers a
// live append. This is the change stream the OpenSearch/Kafka history exporters consume.
func TestArchiveWatchOverRPC(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{})
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{ValueAttrs: []string{"ClusterId"}}); err != nil {
		t.Fatalf("CreateArchiveTable: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := c.ArchiveAppend(ctx, "history", fmt.Sprintf("ClusterId = %d\nJobStatus = 4", i)); err != nil {
			t.Fatalf("ArchiveAppend: %v", err)
		}
	}

	events, stop, err := c.WatchTable(ctx, "history", nil)
	if err != nil {
		t.Fatalf("WatchTable(history): %v", err)
	}
	defer stop()

	// Replay: 5 upserts then WatchSynced.
	upserts := 0
	deadline := time.After(3 * time.Second)
	for synced := false; !synced; {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("stream closed before WatchSynced")
			}
			switch db.WatchKind(ev.Kind) {
			case db.WatchUpsert:
				upserts++
				if !strings.Contains(ev.AdText, "ClusterId") {
					t.Errorf("upsert missing ClusterId: %q", ev.AdText)
				}
			case db.WatchSynced:
				synced = true
			}
		case <-deadline:
			t.Fatalf("timed out during replay after %d upserts", upserts)
		}
	}
	if upserts != 5 {
		t.Fatalf("replayed %d upserts, want 5", upserts)
	}

	// Live: a new append is delivered as an upsert.
	if err := c.ArchiveAppend(ctx, "history", "ClusterId = 99\nJobStatus = 4"); err != nil {
		t.Fatal(err)
	}
	deadline = time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("stream closed waiting for the live append")
			}
			if db.WatchKind(ev.Kind) == db.WatchUpsert && strings.Contains(ev.AdText, "99") {
				return // success
			}
		case <-deadline:
			t.Fatal("timed out waiting for the live append")
		}
	}
}
