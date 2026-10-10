package dbrpc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
)

// nextWatchEvent returns the next event on a WatchTable stream, or ok=false once it closes.
func nextWatchEvent(t *testing.T, events <-chan WatchEvent) (WatchEvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-events:
		return ev, ok
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a watch event")
		return WatchEvent{}, false
	}
}

// syncWatch reads a WatchTable stream through WatchSynced, returning whether it began with a
// Reset, how many upserts the catch-up carried, and the synced cursor.
func syncWatch(t *testing.T, events <-chan WatchEvent) (reset bool, upserts int, cur []byte) {
	t.Helper()
	for first := true; ; first = false {
		ev, ok := nextWatchEvent(t, events)
		if !ok {
			t.Fatal("stream closed before WatchSynced")
		}
		switch db.WatchKind(ev.Kind) {
		case db.WatchReset:
			reset = reset || first
		case db.WatchUpsert:
			upserts++
		case db.WatchSynced:
			return reset, upserts, ev.Cursor
		}
	}
}

// expectResyncThenEnd asserts the server tells a live watcher to resync and ends its stream.
func expectResyncThenEnd(t *testing.T, events <-chan WatchEvent) {
	t.Helper()
	ev, ok := nextWatchEvent(t, events)
	if !ok || db.WatchKind(ev.Kind) != db.WatchResync {
		t.Fatalf("after truncate got kind %d (open=%v), want WatchResync", ev.Kind, ok)
	}
	if _, ok := nextWatchEvent(t, events); ok {
		t.Fatal("stream should end after WatchResync")
	}
}

// TestWatchTableResetsOnAdminTruncate: the admin "truncate" action used to leave a WatchTable
// client (a replica) holding every removed row. Over a real connection, the live watch must
// be told to resync, and its cursor must then Reset to the (empty) table.
func TestWatchTableResetsOnAdminTruncate(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{Privileged: true})
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := c.CreateTable(ctx, "jobs"); err != nil {
		t.Fatal(err)
	}
	write := func(keys ...string) {
		tx, err := c.BeginTable(ctx, "jobs")
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range keys {
			if err := tx.NewClassAd(ctx, k, fmt.Sprintf("Name = %q", k)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var keys []string
	for i := range 20 {
		keys = append(keys, fmt.Sprintf("j%d", i))
	}
	write(keys...)

	events, stop, err := c.WatchTable(ctx, "jobs", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	// The client persists the cursor of the last event it processed: here, Synced's.
	_, n, cur := syncWatch(t, events)
	if n != 20 {
		t.Fatalf("initial sync carried %d upserts, want 20", n)
	}

	if _, err := c.AdminTable(ctx, "jobs", "truncate"); err != nil {
		t.Fatal(err)
	}
	expectResyncThenEnd(t, events)

	events2, stop2, err := c.WatchTable(ctx, "jobs", cur)
	if err != nil {
		t.Fatal(err)
	}
	defer stop2()
	reset, n, _ := syncWatch(t, events2)
	if !reset || n != 0 {
		t.Fatalf("resume after truncate: reset=%v upserts=%d, want a Reset to an empty table", reset, n)
	}
	write("post")
	ev, ok := nextWatchEvent(t, events2)
	if !ok || db.WatchKind(ev.Kind) != db.WatchUpsert || ev.Key != "post" {
		t.Fatalf("live event after truncate = %+v (open=%v), want an upsert of post", ev, ok)
	}
}

// TestArchiveWatchResetsOnAdminTruncate is the archive-table form: the history re-sync reset
// ("truncate") must not leave a WatchTable client tailing the removed records.
func TestArchiveWatchResetsOnAdminTruncate(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{Privileged: true})
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{ValueAttrs: []string{"ClusterId"}}); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		if err := c.ArchiveAppend(ctx, "history", fmt.Sprintf("ClusterId = %d\nJobStatus = 4", i)); err != nil {
			t.Fatal(err)
		}
	}
	events, stop, err := c.WatchTable(ctx, "history", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	_, n, cur := syncWatch(t, events)
	if n != 5 {
		t.Fatalf("initial sync carried %d upserts, want 5", n)
	}

	if _, err := c.AdminTable(ctx, "history", "truncate"); err != nil {
		t.Fatal(err)
	}
	expectResyncThenEnd(t, events)
	if err := c.ArchiveAppend(ctx, "history", "ClusterId = 99\nJobStatus = 4"); err != nil {
		t.Fatal(err)
	}

	events2, stop2, err := c.WatchTable(ctx, "history", cur)
	if err != nil {
		t.Fatal(err)
	}
	defer stop2()
	reset, n, _ := syncWatch(t, events2)
	if !reset || n != 1 {
		t.Fatalf("resume after truncate: reset=%v upserts=%d, want a Reset replaying only the new record", reset, n)
	}
}
