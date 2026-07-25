package collections

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

func mustWatch(t *testing.T, c *Collection, ctx context.Context, cursor []byte) func(func(WatchEvent) bool) {
	t.Helper()
	s, err := c.Watch(ctx, cursor)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// drainCatchup collects a Watch stream's catch-up phase: every event up to and including
// WatchSynced. Returns the upsert N values (in order), whether a Reset was seen first,
// and the synced cursor.
func drainCatchup(t *testing.T, seq func(func(WatchEvent) bool)) (ns []int64, sawReset bool, cursor []byte) {
	t.Helper()
	for ev := range seq {
		switch ev.Kind {
		case WatchReset:
			sawReset = true
		case WatchUpsert:
			v, _ := ev.Ad.EvaluateAttrInt("N")
			ns = append(ns, v)
		case WatchSynced:
			cursor = ev.Cursor
			return
		}
	}
	return
}

// TestAppendOnlyWatch verifies the Collection append-stream watch: full replay yields
// every retained record oldest-first, an incremental resume yields only newer records,
// and a live append is delivered.
func TestAppendOnlyWatch(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{AppendOnly: true, Dir: dir, SegmentSize: 1 << 12, WatchHistory: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const n = 150
	for i := 0; i < n; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N = %d ]`, i))
		if err := c.Put([]byte("k"), ad); err != nil {
			t.Fatal(err)
		}
	}

	// Full replay (nil cursor): Reset, then all N in append order (0..n-1), then Synced.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ns, sawReset, cursor := drainCatchup(t, mustWatch(t, c, ctx, nil))
	if !sawReset {
		t.Error("full replay should begin with WatchReset")
	}
	if len(ns) != n {
		t.Fatalf("full replay yielded %d upserts, want %d", len(ns), n)
	}
	for i, v := range ns {
		if v != int64(i) {
			t.Fatalf("full replay out of append order at %d: got N=%d", i, v)
		}
	}
	if cursor == nil {
		t.Fatal("WatchSynced must carry a cursor")
	}

	// Incremental resume from the synced cursor: no new records yet -> empty catch-up.
	ns2, reset2, cursor2 := drainCatchup(t, mustWatch(t, c, ctx, cursor))
	if reset2 {
		t.Error("incremental resume should not Reset")
	}
	if len(ns2) != 0 {
		t.Fatalf("incremental resume with no new data yielded %d, want 0", len(ns2))
	}

	// Append 10 more, resume again: only the new 10 (150..159).
	for i := n; i < n+10; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N = %d ]`, i))
		if err := c.Put([]byte("k"), ad); err != nil {
			t.Fatal(err)
		}
	}
	ns3, reset3, _ := drainCatchup(t, mustWatch(t, c, ctx, cursor2))
	if reset3 {
		t.Error("incremental resume after appends should not Reset")
	}
	if len(ns3) != 10 || ns3[0] != int64(n) || ns3[9] != int64(n+9) {
		t.Fatalf("incremental resume yielded %v, want %d..%d", ns3, n, n+9)
	}
}

// TestAppendOnlyWatchLive verifies a live append is pushed to an already-synced watcher.
func TestAppendOnlyWatchLive(t *testing.T) {
	c := New(Options{AppendOnly: true, SegmentSize: 1 << 12, WatchHistory: 4096})
	ad, _ := classad.Parse(`[ N = 0 ]`)
	c.Put([]byte("k"), ad)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan int64, 8)
	go func() {
		synced := false
		for ev := range mustWatch(t, c, ctx, nil) {
			switch ev.Kind {
			case WatchSynced:
				synced = true
			case WatchUpsert:
				if synced { // only report live (post-catch-up) upserts
					v, _ := ev.Ad.EvaluateAttrInt("N")
					events <- v
				}
			}
		}
	}()
	// Give the watcher time to reach the live phase, then append.
	time.Sleep(50 * time.Millisecond)
	ad2, _ := classad.Parse(`[ N = 999 ]`)
	c.Put([]byte("k"), ad2)

	select {
	case v := <-events:
		if v != 999 {
			t.Fatalf("live event N=%d, want 999", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("live append was not delivered to the watcher")
	}
}

// TestAppendOnlyWatchResetAfterRotation verifies a cursor pointing below the retained
// floor (its records rotated away) triggers a full replay from the current floor.
func TestAppendOnlyWatchResetAfterRotation(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{
		AppendOnly:   true,
		Dir:          dir,
		SegmentSize:  1 << 12,
		WatchHistory: 4096,
		Retention:    Retention{MaxSegments: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Append a batch, snapshot a cursor, then append lots more and rotate so the early
	// records are dropped.
	for i := 0; i < 20; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N = %d ]`, i))
		c.Put([]byte("k"), ad)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, earlyCursor := drainCatchup(t, mustWatch(t, c, ctx, nil))
	if earlyCursor == nil {
		t.Fatal("no cursor from initial sync")
	}
	for i := 20; i < 400; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N = %d ]`, i))
		c.Put([]byte("k"), ad)
	}
	dropped, err := c.Rotate(0)
	if err != nil {
		t.Fatal(err)
	}
	if dropped == 0 {
		t.Fatal("expected rotation to drop segments")
	}
	// Resuming from the now-stale cursor must Reset (history gap) and replay from floor.
	ns, sawReset, _ := drainCatchup(t, mustWatch(t, c, ctx, earlyCursor))
	if !sawReset {
		t.Error("cursor below the retained floor must trigger WatchReset")
	}
	if len(ns) == 0 {
		t.Fatal("reset replay yielded nothing")
	}
	// The replay starts at the floor (early records are gone), so the smallest N is > 0.
	if ns[0] == 0 {
		t.Errorf("reset replay still includes N=0; rotated records should be gone")
	}
}
