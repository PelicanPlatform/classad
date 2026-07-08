package collections

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestWatchCoalesce verifies that WatchCoalesce collapses a burst of rapid
// changes to one key into a single settled event, collapses an upsert+delete
// within a window to a Delete, and still delivers distinct keys -- while the
// non-coalesced path (control) emits one event per change.
func TestWatchCoalesce(t *testing.T) {
	c := New(Options{Shards: 2, WatchHistory: 1024, WatchCoalesce: 40 * time.Millisecond})
	c.Put(wkey(0), mustAd(t, `[V=0]`))
	_, cursor := collectCatchUp(t, c, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seq, err := c.Watch(ctx, cursor)
	if err != nil {
		t.Fatal(err)
	}

	type rec struct {
		key       string
		kind      WatchKind
		v         int64
		hasCursor bool
	}
	var mu sync.Mutex
	var recs []rec
	done := make(chan struct{})
	synced := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range seq {
			switch ev.Kind {
			case WatchSynced:
				close(synced) // now live: subsequent changes go through coalescing
				continue
			case WatchResync:
				return
			}
			r := rec{key: string(ev.Key), kind: ev.Kind, hasCursor: ev.Cursor != nil}
			if ev.Ad != nil {
				if v, ok := ev.Ad.EvaluateAttrInt("V"); ok {
					r.v = v
				}
			}
			mu.Lock()
			recs = append(recs, r)
			mu.Unlock()
		}
	}()
	<-synced

	// Burst: ten rapid updates to k0, plus a distinct k1.
	for i := 1; i <= 10; i++ {
		c.Put(wkey(0), mustAd(t, fmt.Sprintf(`[V=%d]`, i)))
	}
	c.Put(wkey(1), mustAd(t, `[V=100]`))
	time.Sleep(150 * time.Millisecond)

	// An upsert then delete of k2 within a window collapses to a single Delete.
	c.Put(wkey(2), mustAd(t, `[V=1]`))
	c.Delete(wkey(2))
	time.Sleep(150 * time.Millisecond)

	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	var k0count int
	var lastK0 int64 = -1
	var k1seen, k2delete, anyCursor bool
	for _, r := range recs {
		if r.hasCursor {
			anyCursor = true
		}
		switch r.key {
		case skey(0):
			k0count++
			lastK0 = r.v
		case skey(1):
			k1seen = true
		case skey(2):
			if r.kind == WatchDelete {
				k2delete = true
			}
		}
	}
	if k0count == 0 || k0count > 3 {
		t.Errorf("k0: got %d events for 10 rapid updates, want coalesced (1-3)", k0count)
	}
	if lastK0 != 10 {
		t.Errorf("k0 final value = %d, want 10 (latest)", lastK0)
	}
	if !k1seen {
		t.Error("k1 upsert not delivered")
	}
	if !k2delete {
		t.Error("k2 upsert+delete did not collapse to a Delete")
	}
	if !anyCursor {
		t.Error("no coalesced event carried a cursor")
	}
}

// TestWatchNoCoalesceByDefault confirms that with WatchCoalesce unset, every
// change is delivered as its own live event (the default, un-batched behavior).
func TestWatchNoCoalesceByDefault(t *testing.T) {
	c := New(Options{Shards: 1, WatchHistory: 1024}) // no WatchCoalesce
	c.Put(wkey(0), mustAd(t, `[V=0]`))
	_, cursor := collectCatchUp(t, c, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seq, err := c.Watch(ctx, cursor)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan int, 1)
	synced := make(chan struct{})
	go func() {
		n := 0
		for ev := range seq {
			switch ev.Kind {
			case WatchSynced:
				close(synced)
			case WatchUpsert:
				n++
				if n == 5 {
					got <- n
					return
				}
			}
		}
	}()
	<-synced // ensure live before mutating, so each change is its own live event
	for i := 1; i <= 5; i++ {
		c.Put(wkey(0), mustAd(t, fmt.Sprintf(`[V=%d]`, i)))
	}
	select {
	case n := <-got:
		if n != 5 {
			t.Errorf("got %d upserts, want 5 (one per change)", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not receive 5 individual upserts without coalescing")
	}
}
