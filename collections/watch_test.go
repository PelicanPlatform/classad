package collections

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func wkey(i int) []byte { return []byte(fmt.Sprintf("k%d", i)) }
func skey(i int) string { return fmt.Sprintf("k%d", i) }

// collectCatchUp consumes a watch stream through WatchSynced, returning the catch-up
// events (including a leading Reset, if any) and the synced cursor. Breaking at
// Synced stops iteration cleanly (never enters the live phase).
func collectCatchUp(t *testing.T, c *Collection, cursor []byte) ([]WatchEvent, []byte) {
	t.Helper()
	seq, err := c.Watch(context.Background(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	var events []WatchEvent
	var synced []byte
	for ev := range seq {
		if ev.Kind == WatchSynced {
			synced = ev.Cursor
			break
		}
		events = append(events, ev)
	}
	if synced == nil {
		t.Fatal("watch stream ended without WatchSynced")
	}
	return events, synced
}

func TestWatchDisabled(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 2}) // no WatchHistory
	if _, err := c.Watch(context.Background(), nil); err == nil {
		t.Fatal("expected error when WatchHistory is 0")
	}
}

func TestWatchFullReplayFromEmpty(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4, WatchHistory: 1024})
	const n = 300
	for i := 0; i < n; i++ {
		if err := c.Put(wkey(i), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	events, cursor := collectCatchUp(t, c, nil)
	if len(events) == 0 || events[0].Kind != WatchReset {
		t.Fatalf("first event should be Reset, got %v", events)
	}
	got := map[string]bool{}
	for _, ev := range events[1:] {
		if ev.Kind != WatchUpsert {
			t.Fatalf("expected Upsert in full replay, got kind %d", ev.Kind)
		}
		got[string(ev.Key)] = true
	}
	if len(got) != n {
		t.Fatalf("full replay covered %d keys, want %d", len(got), n)
	}
	if cursor == nil {
		t.Fatal("no synced cursor")
	}
}

func TestWatchIncrementalResume(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4, WatchHistory: 1024})
	const n = 300
	for i := 0; i < n; i++ {
		if err := c.Put(wkey(i), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	_, cursor := collectCatchUp(t, c, nil) // initial full replay -> resume point

	// Changes after the cursor.
	_ = c.Put(wkey(0), mustAd(t, `[Id=0; V=1]`))  // update
	_ = c.Put(wkey(9000), mustAd(t, `[Id=9000]`)) // insert
	if !c.Delete(wkey(5)) {                       // delete
		t.Fatal("delete k5 failed")
	}

	events, _ := collectCatchUp(t, c, cursor)
	upserts, deletes := map[string]bool{}, map[string]bool{}
	for _, ev := range events {
		switch ev.Kind {
		case WatchReset:
			t.Fatal("unexpected Reset on an in-window incremental resume")
		case WatchUpsert:
			upserts[string(ev.Key)] = true
		case WatchDelete:
			deletes[string(ev.Key)] = true
		}
	}
	if !upserts[skey(0)] || !upserts[skey(9000)] {
		t.Fatalf("incremental resume missed changed upserts: %v", upserts)
	}
	if !deletes[skey(5)] {
		t.Fatalf("incremental resume missed the delete: %v", deletes)
	}
	// Our implementation is precise, so an unchanged key should not be re-delivered
	// (at-least-once permits it, but this guards against accidental full replay).
	if upserts[skey(2)] {
		t.Errorf("unchanged key k2 was re-delivered")
	}
}

// TestWatchDeleteThenReadd checks catch-up ordering: a key deleted then re-added
// since the cursor must end present (Delete emitted before Upsert).
func TestWatchDeleteThenReadd(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 1, WatchHistory: 1024})
	_ = c.Put(wkey(0), mustAd(t, `[Id=0]`))
	_, cursor := collectCatchUp(t, c, nil)

	if !c.Delete(wkey(0)) {
		t.Fatal("delete failed")
	}
	_ = c.Put(wkey(0), mustAd(t, `[Id=0; V=2]`)) // re-add

	// Apply events into a map like a client would; the net state must have k0 present.
	events, _ := collectCatchUp(t, c, cursor)
	state := map[string]bool{}
	for _, ev := range events {
		switch ev.Kind {
		case WatchUpsert:
			state[string(ev.Key)] = true
		case WatchDelete:
			delete(state, string(ev.Key))
		}
	}
	if !state[skey(0)] {
		t.Fatalf("after delete+readd, k0 should be present; events=%v", events)
	}
}

// TestWatchResetOnTrimmedDelete: once the delete journal trims past a cursor, that
// cursor can no longer resume incrementally and must get a full replay.
func TestWatchResetOnTrimmedDelete(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 1, WatchHistory: 4}) // tiny journal
	for i := 0; i < 100; i++ {
		_ = c.Put(wkey(i), mustAd(t, fmt.Sprintf(`[Id=%d]`, i)))
	}
	_, oldCursor := collectCatchUp(t, c, nil)

	// Delete far more than 2*cap keys so the journal trims and the horizon advances
	// past oldCursor.
	for i := 0; i < 40; i++ {
		c.Delete(wkey(i))
	}
	events, _ := collectCatchUp(t, c, oldCursor)
	if len(events) == 0 || events[0].Kind != WatchReset {
		t.Fatalf("resume past the delete horizon should Reset; got %v",
			func() []WatchKind {
				ks := make([]WatchKind, len(events))
				for i, e := range events {
					ks[i] = e.Kind
				}
				return ks
			}())
	}
}

func TestWatchEpochMismatch(t *testing.T) {
	t.Parallel()
	c1 := New(Options{Shards: 2, WatchHistory: 64})
	_ = c1.Put(wkey(0), mustAd(t, `[Id=0]`))
	_, cursor := collectCatchUp(t, c1, nil)

	// A different collection has a different epoch; the cursor is not valid there.
	c2 := New(Options{Shards: 2, WatchHistory: 64})
	_ = c2.Put(wkey(1), mustAd(t, `[Id=1]`))
	events, _ := collectCatchUp(t, c2, cursor)
	if len(events) == 0 || events[0].Kind != WatchReset {
		t.Fatal("cursor from a different store generation should force Reset")
	}
}

// TestWatchLive streams live changes after the initial sync.
func TestWatchLive(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 2, WatchHistory: 1024})
	_ = c.Put(wkey(0), mustAd(t, `[Id=0]`))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seq, err := c.Watch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan WatchEvent, 128)
	go func() {
		for ev := range seq {
			events <- ev
		}
		close(events)
	}()
	// Drain catch-up to Synced.
	for ev := range events {
		if ev.Kind == WatchSynced {
			break
		}
	}
	// Live changes.
	_ = c.Put(wkey(1), mustAd(t, `[Id=1]`))
	if !c.Delete(wkey(0)) {
		t.Fatal("delete failed")
	}
	got := map[string]WatchKind{}
	deadline := time.After(3 * time.Second)
	for len(got) < 2 {
		select {
		case ev := <-events:
			if ev.Kind == WatchUpsert || ev.Kind == WatchDelete {
				got[string(ev.Key)] = ev.Kind
				if ev.Cursor == nil {
					t.Error("live event missing cursor")
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for live events, got %v", got)
		}
	}
	if got[skey(1)] != WatchUpsert {
		t.Errorf("expected live Upsert for k1, got %d", got[skey(1)])
	}
	if got[skey(0)] != WatchDelete {
		t.Errorf("expected live Delete for k0, got %d", got[skey(0)])
	}
}

// TestWatchConcurrent runs a live watcher alongside concurrent writers; every change
// must reach the watcher (or a Resync is signaled). Run with -race.
func TestWatchConcurrent(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4, WatchHistory: 4096, WatchBuffer: 8192})
	const n = 500
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seq, err := c.Watch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	var mu sync.Mutex
	resync := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range seq {
			if ev.Kind == WatchResync {
				mu.Lock()
				resync = true
				mu.Unlock()
				return
			}
			if ev.Kind == WatchUpsert {
				mu.Lock()
				seen[string(ev.Key)] = true
				mu.Unlock()
			}
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < n; i += 4 {
				_ = c.Put(wkey(i), mustAd(t, fmt.Sprintf(`[Id=%d]`, i)))
			}
		}(w)
	}
	wg.Wait()

	// Give the watcher a moment to drain, then stop it.
	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		got, r := len(seen), resync
		mu.Unlock()
		if r || got >= n {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("watcher saw %d/%d keys and no resync", got, n)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done
}
