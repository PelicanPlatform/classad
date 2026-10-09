package collections

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests pin the watch delivery invariant across the catch-up/live boundary and
// across resumes from live cursors:
//
//   - every committed event with seq in (cursor, S_reg] is delivered exactly once, in
//     catch-up; every event with seq > S_reg exactly once, live;
//   - a cursor carried by a live event never covers an event the watcher has not yet
//     been handed, so resuming from it loses nothing.
//
// A commit advances commitSeq under the shard lock but syncs and publishes after
// unlocking, so each test parks a commit in that window (Options.CommitSync runs
// inside it) or forces the watcher's snapshot to land in it.

// syncGate parks the first commit that reaches its durability sync until released;
// every later sync passes straight through.
type syncGate struct {
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func newSyncGate() *syncGate {
	return &syncGate{entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *syncGate) hook() {
	if g.armed.CompareAndSwap(true, false) {
		close(g.entered)
		<-g.release
	}
}

// watchStream runs a watch in the background and returns its events on a channel.
func watchStream(t *testing.T, c *Collection, ctx context.Context, cursor []byte) <-chan WatchEvent {
	t.Helper()
	seq, err := c.Watch(ctx, cursor)
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan WatchEvent, 1024)
	go func() {
		defer close(ch)
		for ev := range seq {
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// untilKey collects events up to and including the first data event for key stop.
func untilKey(t *testing.T, ch <-chan WatchEvent, stop string) []WatchEvent {
	t.Helper()
	var out []WatchEvent
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("watch ended before %q; got %s", stop, evString(out))
			}
			out = append(out, ev)
			if (ev.Kind == WatchUpsert || ev.Kind == WatchDelete) && string(ev.Key) == stop {
				return out
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q; got %s", stop, evString(out))
		}
	}
}

func evString(evs []WatchEvent) string {
	s := "["
	for i, ev := range evs {
		if i > 0 {
			s += " "
		}
		switch ev.Kind {
		case WatchUpsert:
			s += "U:" + string(ev.Key)
		case WatchDelete:
			s += "D:" + string(ev.Key)
		case WatchReset:
			s += "Reset"
		case WatchSynced:
			s += "Synced"
		case WatchResync:
			s += "Resync"
		}
	}
	return s + "]"
}

func countKind(evs []WatchEvent, kind WatchKind, key string) int {
	n := 0
	for _, ev := range evs {
		if ev.Kind == kind && string(ev.Key) == key {
			n++
		}
	}
	return n
}

// TestWatchCatchupDeleteInCommitWindow: a watcher whose snapshot lands after a delete
// advanced commitSeq but before the delete was journaled and published must still get
// the delete, from catch-up. Before the journal was written under the shard lock the
// delete was in neither phase: not journaled yet when catch-up read it, and at or below
// S_reg so the live phase dropped it.
func TestWatchCatchupDeleteInCommitWindow(t *testing.T) {
	t.Parallel()
	for _, viaTxn := range []bool{false, true} {
		t.Run(fmt.Sprintf("txn=%v", viaTxn), func(t *testing.T) {
			t.Parallel()
			g := newSyncGate()
			c := New(Options{Shards: 1, WatchHistory: 64, CommitSync: g.hook})
			_ = c.Put([]byte("k"), mustAd(t, `[N=1]`))
			cur, err := c.WatchCursor()
			if err != nil {
				t.Fatal(err)
			}

			g.armed.Store(true)
			done := make(chan struct{})
			go func() {
				defer close(done)
				if viaTxn {
					tx := c.Begin()
					tx.Delete([]byte("k"))
					tx.Commit()
				} else {
					c.Delete([]byte("k"))
				}
			}()
			<-g.entered // the delete has advanced commitSeq and is parked before publishing

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch := watchStream(t, c, ctx, cur)
			var catchup []WatchEvent
			for ev := range ch {
				catchup = append(catchup, ev)
				if ev.Kind == WatchSynced {
					break
				}
			}
			close(g.release)
			<-done
			_ = c.Put([]byte("mark"), mustAd(t, `[N=2]`))
			evs := slices.Concat(catchup, untilKey(t, ch, "mark"))

			if countKind(evs, WatchReset, "") != 0 {
				t.Fatalf("unexpected Reset: %s", evString(evs))
			}
			if n := countKind(evs, WatchDelete, "k"); n != 1 {
				t.Fatalf("delete of k delivered %d times, want exactly 1: %s", n, evString(evs))
			}
			if countKind(catchup, WatchDelete, "k") != 1 {
				t.Fatalf("delete at or below S_reg must come from catch-up: %s", evString(evs))
			}
		})
	}
}

// TestWatchCatchupDeleteAfterSnapshot: a delete that commits after the watcher took
// S_reg belongs to the live phase alone. Catch-up used to read the delete journal with
// no upper bound, so a delete journaled before catch-up ran was delivered twice.
func TestWatchCatchupDeleteAfterSnapshot(t *testing.T) {
	c := New(Options{Shards: 1, WatchHistory: 64})
	_ = c.Put([]byte("k"), mustAd(t, `[N=1]`))
	cur, err := c.WatchCursor()
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	watchSnapshotHook = func(wc *Collection) {
		if wc == c {
			once.Do(func() { c.Delete([]byte("k")) })
		}
	}
	t.Cleanup(func() { watchSnapshotHook = nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := watchStream(t, c, ctx, cur)
	var catchup []WatchEvent
	for ev := range ch {
		catchup = append(catchup, ev)
		if ev.Kind == WatchSynced {
			break
		}
	}
	_ = c.Put([]byte("mark"), mustAd(t, `[N=2]`))
	evs := slices.Concat(catchup, untilKey(t, ch, "mark"))
	if n := countKind(evs, WatchDelete, "k"); n != 1 {
		t.Fatalf("delete of k delivered %d times, want exactly 1: %s", n, evString(evs))
	}
	if countKind(catchup, WatchDelete, "k") != 0 {
		t.Fatalf("delete above S_reg must come from the live phase only: %s", evString(evs))
	}
}

// liveCursorOf returns the cursor carried by the first live data event for key.
func liveCursorOf(t *testing.T, ch <-chan WatchEvent, key string) []byte {
	t.Helper()
	evs := untilKey(t, ch, key)
	ev := evs[len(evs)-1]
	if ev.Cursor == nil {
		t.Fatalf("live event for %q has no cursor", key)
	}
	return ev.Cursor
}

// resumeKeys resumes from cursor and returns the keys of the data events delivered up
// to Synced, failing on a Reset (which would hide a skipped event behind a full replay).
func resumeKeys(t *testing.T, c *Collection, cursor []byte) map[string]WatchKind {
	t.Helper()
	evs, _ := collectCatchUp(t, c, cursor)
	got := map[string]WatchKind{}
	for _, ev := range evs {
		if ev.Kind == WatchReset {
			t.Fatalf("resume from a live cursor Reset: %s", evString(evs))
		}
		got[string(ev.Key)] = ev.Kind
	}
	return got
}

// TestWatchLiveCursorCoversWholeBatch: the events of one commit share a seq and are
// handed to a watcher one at a time, so the cursor on the first one must not claim the
// seq -- a client that persists it and restarts would skip the rest of the batch.
func TestWatchLiveCursorCoversWholeBatch(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 1, WatchHistory: 64})
	_ = c.Put([]byte("gone"), mustAd(t, `[N=0]`))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := watchStream(t, c, ctx, nil)
	untilSynced(t, ch)

	tx := c.Begin()
	tx.Put([]byte("a"), mustAd(t, `[N=1]`))
	tx.Put([]byte("b"), mustAd(t, `[N=2]`))
	tx.Delete([]byte("gone"))
	tx.Commit()

	// Whichever event of the batch arrives first, its cursor must still replay the others.
	evs := untilAnyData(t, ch)
	first := evs[len(evs)-1]
	got := resumeKeys(t, c, first.Cursor)
	for k, kind := range map[string]WatchKind{"a": WatchUpsert, "b": WatchUpsert, "gone": WatchDelete} {
		if k == string(first.Key) {
			continue
		}
		if g, ok := got[k]; !ok || g != kind {
			t.Fatalf("resume from the cursor on %q lost %q (kind %d): got %v", first.Key, k, kind, got)
		}
	}
}

// TestWatchLiveCursorOutOfOrderPublish: two commits to one shard can publish in the
// opposite order to their seqs (each publishes after its own sync). The later commit's
// cursor must not cover the earlier one until the earlier one has been delivered.
func TestWatchLiveCursorOutOfOrderPublish(t *testing.T) {
	t.Parallel()
	g := newSyncGate()
	c := New(Options{Shards: 1, WatchHistory: 64, CommitSync: g.hook})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := watchStream(t, c, ctx, nil)
	untilSynced(t, ch)

	g.armed.Store(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tx := c.Begin()
		tx.Put([]byte("early"), mustAd(t, `[N=1]`))
		tx.Commit() // parked in its sync after taking the lower seq
	}()
	<-g.entered
	tx := c.Begin()
	tx.Put([]byte("late"), mustAd(t, `[N=2]`))
	tx.Commit()

	cur := liveCursorOf(t, ch, "late")
	if k, ok := resumeKeys(t, c, cur)["early"]; !ok || k != WatchUpsert {
		t.Fatalf("resume from the cursor on %q skipped the earlier, unpublished commit", "late")
	}
	close(g.release)
	<-done
	evs := untilKey(t, ch, "early")
	if countKind(evs, WatchUpsert, "early") != 1 {
		t.Fatalf("early commit not delivered live exactly once: %s", evString(evs))
	}
}

func untilSynced(t *testing.T, ch <-chan WatchEvent) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("watch ended before Synced")
			}
			if ev.Kind == WatchSynced {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for Synced")
		}
	}
}

func untilAnyData(t *testing.T, ch <-chan WatchEvent) []WatchEvent {
	t.Helper()
	var out []WatchEvent
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("watch ended; got %s", evString(out))
			}
			out = append(out, ev)
			if ev.Kind == WatchUpsert || ev.Kind == WatchDelete {
				return out
			}
		case <-deadline:
			t.Fatalf("timed out; got %s", evString(out))
		}
	}
}

// TestWatchBoundaryStress races a watcher's registration against a delete and an
// upsert, many times, and requires each to be delivered exactly once -- in catch-up or
// live, never both, never neither. Bounded by wall time so it stays cheap under -race.
func TestWatchBoundaryStress(t *testing.T) {
	t.Parallel()
	budget := 3 * time.Second
	if testing.Short() {
		budget = 500 * time.Millisecond
	}
	for _, viaTxn := range []bool{false, true} {
		t.Run(fmt.Sprintf("txn=%v", viaTxn), func(t *testing.T) {
			t.Parallel()
			c := New(Options{Shards: 2, WatchHistory: 4096})
			stop := time.Now().Add(budget)
			iters, lost, dup := 0, 0, 0
			for time.Now().Before(stop) {
				iters++
				_ = c.Put([]byte("k"), mustAd(t, `[N=1]`))
				cur, _ := c.WatchCursor()
				ctx, cancel := context.WithCancel(context.Background())
				start := make(chan struct{})
				var seqErr error
				var events []WatchEvent
				var wg sync.WaitGroup
				wg.Add(1)
				go func() {
					defer wg.Done()
					seq, err := c.Watch(ctx, cur)
					if err != nil {
						seqErr = err
						return
					}
					close(start)
					// Stop at mark only once live: catch-up visits shards in turn, so mark
					// can come before the shard holding k or u has been caught up.
					synced, marked := false, false
					for ev := range seq {
						events = append(events, ev)
						switch {
						case ev.Kind == WatchResync:
							return
						case ev.Kind == WatchSynced:
							synced = true
						case ev.Kind == WatchUpsert && string(ev.Key) == "mark":
							marked = true
						}
						if synced && marked {
							return
						}
					}
				}()
				<-start
				if viaTxn {
					tx := c.Begin()
					tx.Delete([]byte("k"))
					tx.Put([]byte("u"), mustAd(t, fmt.Sprintf(`[I=%d]`, iters)))
					tx.Commit()
				} else {
					c.Delete([]byte("k"))
					_ = c.Put([]byte("u"), mustAd(t, fmt.Sprintf(`[I=%d]`, iters)))
				}
				_ = c.Put([]byte("mark"), mustAd(t, `[N=3]`))
				wg.Wait()
				cancel()
				if seqErr != nil {
					t.Fatal(seqErr)
				}
				if countKind(events, WatchReset, "") > 0 || countKind(events, WatchResync, "") > 0 {
					t.Fatalf("iteration %d: unexpected Reset/Resync: %s", iters, evString(events))
				}
				for _, kk := range []struct {
					kind WatchKind
					key  string
				}{{WatchDelete, "k"}, {WatchUpsert, "u"}} {
					switch n := countKind(events, kk.kind, kk.key); {
					case n == 0:
						lost++
						t.Errorf("iteration %d: event for %q lost: %s", iters, kk.key, evString(events))
					case n > 1:
						dup++
						t.Errorf("iteration %d: event for %q delivered %d times: %s", iters, kk.key, n, evString(events))
					}
				}
				if lost+dup > 5 {
					break
				}
				c.Delete([]byte("mark"))
			}
			t.Logf("%d iterations, %d lost, %d duplicated", iters, lost, dup)
		})
	}
}
