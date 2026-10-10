package collections

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// truncClient is a Watch consumer that follows the documented client model (docs/WATCH.md):
// a Reset builds into a shadow that goes live at Synced, the cursor is persisted from every
// event that carries one, and a Resync reconnects with the last persisted cursor. It records
// what a replica tailing the collection would hold, plus counters the tests assert on.
type truncClient struct {
	mu      sync.Mutex
	rows    map[string]bool
	shadow  map[string]bool
	cursor  []byte
	synced  int            // WatchSynced count
	resets  int            // WatchReset count
	resyncs int            // WatchResync count
	upserts map[string]int // per-key Upsert deliveries, catch-up and live
}

// startTruncClient runs a truncClient against c until the test ends.
func startTruncClient(t *testing.T, c *Collection) *truncClient {
	t.Helper()
	tc := &truncClient{rows: map[string]bool{}, upserts: map[string]int{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			tc.mu.Lock()
			cur := tc.cursor
			tc.mu.Unlock()
			seq, err := c.Watch(ctx, cur)
			if err != nil {
				t.Error(err)
				return
			}
			for ev := range seq {
				tc.apply(ev)
			}
		}
	}()
	return tc
}

func (tc *truncClient) apply(ev WatchEvent) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	target := tc.rows
	if tc.shadow != nil {
		target = tc.shadow
	}
	switch ev.Kind {
	case WatchReset:
		tc.resets++
		tc.shadow = map[string]bool{}
	case WatchUpsert:
		tc.upserts[string(ev.Key)]++
		target[string(ev.Key)] = true
	case WatchDelete:
		delete(target, string(ev.Key))
	case WatchSynced:
		tc.synced++
		if tc.shadow != nil {
			tc.rows, tc.shadow = tc.shadow, nil
		}
	case WatchResync:
		tc.resyncs++
	}
	if ev.Cursor != nil {
		tc.cursor = ev.Cursor
	}
}

// waitFor polls cond (under the client's lock) until it holds or the deadline passes.
func (tc *truncClient) waitFor(t *testing.T, what string, cond func(tc *truncClient) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		tc.mu.Lock()
		ok := cond(tc)
		state := fmt.Sprintf("rows=%d synced=%d resets=%d resyncs=%d", len(tc.rows), tc.synced, tc.resets, tc.resyncs)
		tc.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s (%s)", what, state)
		}
		time.Sleep(time.Millisecond)
	}
}

func (tc *truncClient) keys() []string {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	var out []string
	for k := range tc.rows {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// resetToEmpty reports that the client has been through a resync and a Reset since its first
// sync and is settled on an empty view. Truncate kicks shard by shard, so a client that
// reconnects between two shards' kicks is kicked again: extra resyncs are allowed.
func resetToEmpty(tc *truncClient) bool {
	return tc.resyncs >= 1 && tc.resets >= 2 && tc.shadow == nil && tc.synced == tc.resyncs+1 && len(tc.rows) == 0
}

// truncateFollowedLive is the live-watcher contract shared by both collection kinds: a
// synced watcher holding n rows is forced to resync by Truncate, comes back through a Reset
// to an empty view, and then receives a post-truncate write exactly once.
func truncateFollowedLive(t *testing.T, c *Collection, put func(key string, n int)) {
	const n = 200
	for i := range n {
		put(skey(i), i)
	}
	tc := startTruncClient(t, c)
	tc.waitFor(t, "initial sync", func(tc *truncClient) bool { return tc.synced == 1 && len(tc.rows) == n })

	c.Truncate()
	// Nothing else is written: the kick alone must wake the idle watcher.
	tc.waitFor(t, "a Reset to the empty collection", resetToEmpty)

	put("post", 1)
	put("post2", 2)
	tc.waitFor(t, "the post-truncate writes", func(tc *truncClient) bool { return tc.rows["post"] && tc.rows["post2"] })
	if got := tc.keys(); len(got) != 2 {
		t.Fatalf("watcher holds %v, want [post post2]", got)
	}
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.upserts["post"] != 1 {
		t.Fatalf("post-truncate write delivered %d times, want 1", tc.upserts["post"])
	}
}

// TestWatchTruncateLiveResets: a live watcher of a mutable collection used to keep every
// row a Truncate removed, because Truncate published nothing. It must Reset to empty.
func TestWatchTruncateLiveResets(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4, WatchHistory: 1024})
	truncateFollowedLive(t, c, func(key string, n int) {
		if err := c.Put([]byte(key), mustAd(t, fmt.Sprintf(`[N=%d]`, n))); err != nil {
			t.Fatal(err)
		}
	})
}

// TestWatchTruncateLiveResetsCoalesced: the coalesced live loop honors the kick too.
func TestWatchTruncateLiveResetsCoalesced(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4, WatchHistory: 1024, WatchCoalesce: time.Millisecond})
	truncateFollowedLive(t, c, func(key string, n int) {
		if err := c.Put([]byte(key), mustAd(t, fmt.Sprintf(`[N=%d]`, n))); err != nil {
			t.Fatal(err)
		}
	})
}

// TestAppendWatchTruncateLiveResets is TestWatchTruncateLiveResets for an append log.
func TestAppendWatchTruncateLiveResets(t *testing.T) {
	t.Parallel()
	c := New(Options{AppendOnly: true, SegmentSize: 1 << 12, WatchHistory: 4096})
	truncateFollowedLive(t, c, func(key string, n int) {
		if err := c.Put([]byte(key), mustAd(t, fmt.Sprintf(`[N=%d]`, n))); err != nil {
			t.Fatal(err)
		}
	})
}

// upsertKeys returns the keys of the Upsert events in evs, sorted.
func upsertKeys(evs []WatchEvent) []string {
	var out []string
	for _, ev := range evs {
		if ev.Kind == WatchUpsert {
			out = append(out, string(ev.Key))
		}
	}
	sort.Strings(out)
	return out
}

func hasReset(evs []WatchEvent) bool {
	return len(evs) > 0 && evs[0].Kind == WatchReset
}

// TestWatchTruncateResume: a cursor issued before a Truncate used to resume incrementally
// (it was within the delete journal's horizon) and so never learned the rows were gone. It
// must Reset; a cursor issued after the Truncate resumes incrementally, deletes included.
func TestWatchTruncateResume(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4, WatchHistory: 1024})
	for i := range 100 {
		_ = c.Put(wkey(i), mustAd(t, fmt.Sprintf(`[N=%d]`, i)))
	}
	_ = c.Delete(wkey(0)) // a journaled delete the truncate must not leave resumable
	pre, _ := c.WatchCursor()
	_, synced := collectCatchUp(t, c, nil) // a catch-up cursor, as a client would persist
	c.Truncate()
	post, _ := c.WatchCursor()
	_ = c.Put([]byte("a"), mustAd(t, `[N=1]`))
	_ = c.Put([]byte("b"), mustAd(t, `[N=2]`))
	_ = c.Delete([]byte("b"))

	for name, cur := range map[string][]byte{"WatchCursor": pre, "synced": synced} {
		evs, _ := collectCatchUp(t, c, cur)
		if !hasReset(evs) {
			t.Fatalf("pre-truncate %s cursor resumed without a Reset: %d events", name, len(evs))
		}
		if got := upsertKeys(evs); len(got) != 1 || got[0] != "a" {
			t.Fatalf("pre-truncate %s cursor replayed %v, want [a]", name, got)
		}
	}

	evs, _ := collectCatchUp(t, c, post)
	if hasReset(evs) {
		t.Fatal("a cursor taken after the Truncate must resume without a Reset")
	}
	if got := upsertKeys(evs); len(got) != 1 || got[0] != "a" {
		t.Fatalf("post-truncate cursor replayed %v, want [a]", got)
	}
	if countKind(evs, WatchDelete, "b") != 1 {
		t.Fatalf("post-truncate delete of b not replayed: %s", evString(evs))
	}
}

// TestAppendWatchTruncateResume: the append-log form of TestWatchTruncateResume. An append
// log's resume gate is its floor, which Truncate must raise to its own seq: not to the
// first post-truncate record, which would also Reset the post-truncate cursor.
func TestAppendWatchTruncateResume(t *testing.T) {
	t.Parallel()
	c := New(Options{AppendOnly: true, SegmentSize: 1 << 12, WatchHistory: 4096})
	appendN(t, c, 0, 50)
	pre, _ := c.WatchCursor()
	c.Truncate()
	post, _ := c.WatchCursor()
	appendN(t, c, 100, 110)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ns, reset, _ := drainCatchup(t, mustWatch(t, c, ctx, pre))
	if !reset {
		t.Fatal("a cursor issued before the Truncate must Reset")
	}
	wantRange(t, "pre-truncate cursor", ns, 100, 110)
	ns, reset, _ = drainCatchup(t, mustWatch(t, c, ctx, post))
	if reset {
		t.Fatal("a cursor taken after the Truncate must resume without a Reset")
	}
	wantRange(t, "post-truncate cursor", ns, 100, 110)
}

// TestArchiveWatchTruncateResumeAcrossReopen: the append floor is rebuilt by Open, so a
// pre-truncate cursor still Resets after a clean restart -- whether the log was left empty
// or written to again before Close -- and a later cursor still resumes.
func TestArchiveWatchTruncateResumeAcrossReopen(t *testing.T) {
	t.Parallel()
	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		c := mustOpen(t, epochTestOpts(dir))
		appendN(t, c, 0, 50)
		pre, _ := c.WatchCursor()
		c.Truncate()
		post, _ := c.WatchCursor()
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		c = mustOpen(t, epochTestOpts(dir))
		defer c.Close()
		if got, _ := c.WatchCursor(); cursorEpoch(t, got) != cursorEpoch(t, pre) {
			t.Fatal("test premise: a clean reopen keeps the epoch")
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ns, reset, _ := drainCatchup(t, mustWatch(t, c, ctx, pre))
		if !reset || len(ns) != 0 {
			t.Fatalf("pre-truncate cursor after reopen: reset=%v records=%v, want a Reset to empty", reset, ns)
		}
		appendN(t, c, 100, 110)
		ns, reset, _ = drainCatchup(t, mustWatch(t, c, ctx, pre))
		if !reset {
			t.Fatal("pre-truncate cursor must still Reset once records are appended")
		}
		wantRange(t, "pre-truncate cursor", ns, 100, 110)
		ns, reset, _ = drainCatchup(t, mustWatch(t, c, ctx, post))
		if reset {
			t.Fatal("a cursor taken after the Truncate must resume after reopen")
		}
		wantRange(t, "post-truncate cursor", ns, 100, 110)
	})
	t.Run("refilled", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		c := mustOpen(t, epochTestOpts(dir))
		appendN(t, c, 0, 50)
		pre, _ := c.WatchCursor()
		c.Truncate()
		appendN(t, c, 60, 70)
		later, _ := c.WatchCursor()
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		c = mustOpen(t, epochTestOpts(dir))
		defer c.Close()
		appendN(t, c, 100, 105)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ns, reset, _ := drainCatchup(t, mustWatch(t, c, ctx, pre))
		if !reset {
			t.Fatal("pre-truncate cursor must Reset after reopen")
		}
		if len(ns) != 15 || ns[0] != 60 || ns[14] != 104 {
			t.Fatalf("reset replay = %v, want 60..69 then 100..104", ns)
		}
		ns, reset, _ = drainCatchup(t, mustWatch(t, c, ctx, later))
		if reset {
			t.Fatal("a cursor past the post-truncate records must resume after reopen")
		}
		wantRange(t, "later cursor", ns, 100, 105)
	})
}

// TestWatchTruncateDuringCatchup: a watcher that took S_reg before the Truncate but runs its
// catch-up after it reads the emptied shards and hands out a Synced cursor below the
// truncate. The kick must still reach it (it registered first), and that cursor must Reset.
func TestWatchTruncateDuringCatchup(t *testing.T) {
	for _, appendOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("appendOnly=%v", appendOnly), func(t *testing.T) {
			opts := Options{Shards: 2, WatchHistory: 64}
			if appendOnly {
				opts = Options{AppendOnly: true, SegmentSize: 1 << 12, WatchHistory: 64}
			}
			c := New(opts)
			for i := range 20 {
				_ = c.Put(wkey(i), mustAd(t, fmt.Sprintf(`[N=%d]`, i)))
			}
			var once sync.Once
			watchSnapshotHook = func(wc *Collection) {
				if wc == c {
					once.Do(c.Truncate)
				}
			}
			t.Cleanup(func() { watchSnapshotHook = nil })

			tc := startTruncClient(t, c)
			tc.waitFor(t, "a Reset to the empty collection", resetToEmpty)
			_ = c.Put([]byte("post"), mustAd(t, `[N=1]`))
			tc.waitFor(t, "the post-truncate write", func(tc *truncClient) bool { return tc.rows["post"] })
		})
	}
}

// TestWatchTruncateConcurrent races watchers subscribing and resuming against repeated
// Truncates (writers serialized with Truncate, as the db layer does), then checks that a
// follower converges to exactly the collection's contents. Run with -race.
func TestWatchTruncateConcurrent(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4, WatchHistory: 64, WatchBuffer: 16})
	var wmu sync.Mutex // the db layer's lock: writers vs Truncate
	write := func(i int) {
		wmu.Lock()
		defer wmu.Unlock()
		_ = c.Put(wkey(i), mustAd(t, fmt.Sprintf(`[N=%d]`, i)))
	}
	followers := []*truncClient{startTruncClient(t, c), startTruncClient(t, c)}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for g := range 3 { // short-lived watchers churning registrations
		wg.Add(1)
		go func() {
			defer wg.Done()
			var cur []byte
			for ctx.Err() == nil {
				wctx, wcancel := context.WithTimeout(ctx, time.Millisecond*time.Duration(1+g))
				seq, _ := c.Watch(wctx, cur)
				for ev := range seq {
					if ev.Cursor != nil {
						cur = ev.Cursor
					}
				}
				wcancel()
			}
		}()
	}
	for round := range 30 {
		for i := range 50 {
			write(round*50 + i)
		}
		if round%3 == 2 {
			wmu.Lock()
			c.Truncate()
			wmu.Unlock()
		}
	}
	cancel()
	wg.Wait()
	write(1 << 20) // a final marker every follower must reach

	want := map[string]bool{}
	c.ForEachAd(func(key string, _ *classad.ClassAd) bool { want[key] = true; return true })
	for i, tc := range followers {
		tc.waitFor(t, fmt.Sprintf("follower %d to converge", i), func(tc *truncClient) bool {
			if tc.shadow != nil || len(tc.rows) != len(want) {
				return false
			}
			for k := range want {
				if !tc.rows[k] {
					return false
				}
			}
			return true
		})
	}
}
