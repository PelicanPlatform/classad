package collections

import (
	"context"
	"fmt"
	"iter"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// clockAd is a small ad the watch-clock tests write.
func clockAd(t *testing.T, i int) *classad.ClassAd {
	t.Helper()
	return mkAd(t, fmt.Sprintf("[ ClusterId = %d; JobStatus = 2 ]", i))
}

// drainTo reads a watch stream up to and including WatchSynced, discarding catch-up.
func drainTo(t *testing.T, seq iter.Seq[WatchEvent]) (next func() (WatchEvent, bool), stop func()) {
	t.Helper()
	next, stop = iter.Pull(seq)
	for {
		ev, ok := next()
		if !ok {
			t.Fatal("watch ended before WatchSynced")
		}
		if ev.Kind == WatchSynced {
			return next, stop
		}
	}
}

// liveStamps reads n live upserts and returns key -> stamp.
func liveStamps(t *testing.T, next func() (WatchEvent, bool), n int) map[string]uint64 {
	t.Helper()
	out := map[string]uint64{}
	for len(out) < n {
		ev, ok := next()
		if !ok {
			t.Fatalf("stream ended after %d of %d live events", len(out), n)
		}
		if ev.Kind == WatchUpsert {
			out[string(ev.Key)] = ev.Stamp
		}
	}
	return out
}

// TestTwoWatchersAgreeOnStamps is the property a second reader of the same collection
// depends on: both must name the same change with the same number, or two servers built on
// this feed cannot hand a client a version that means the same thing on each.
func TestTwoWatchersAgreeOnStamps(t *testing.T) {
	c := New(Options{Shards: 4, WatchHistory: 256})
	defer c.Close()

	a, err := c.Watch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	nextA, stopA := drainTo(t, a)
	defer stopA()
	b, err := c.Watch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	nextB, stopB := drainTo(t, b)
	defer stopB()

	const n = 20
	for i := range n {
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), clockAd(t, i)); err != nil {
			t.Fatal(err)
		}
	}

	sa := liveStamps(t, nextA, n)
	sb := liveStamps(t, nextB, n)
	if len(sa) != n {
		t.Fatalf("watcher A saw %d distinct keys, want %d", len(sa), n)
	}
	for k, va := range sa {
		if va == 0 {
			t.Errorf("key %q was delivered with a zero stamp", k)
		}
		if vb := sb[k]; vb != va {
			t.Errorf("key %q: watcher A stamped it %d, watcher B %d", k, va, vb)
		}
	}
}

// TestStampsIncreaseWithinAShard pins the one ordering guarantee a single shard makes.
// Across shards stamps are comparable but not delivery-ordered, which is why
// WatchWatermark exists; within one shard they must strictly increase.
func TestStampsIncreaseWithinAShard(t *testing.T) {
	c := New(Options{Shards: 1, WatchHistory: 256})
	defer c.Close()
	seq, err := c.Watch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	next, stop := drainTo(t, seq)
	defer stop()

	const n = 50
	for i := range n {
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), clockAd(t, i)); err != nil {
			t.Fatal(err)
		}
	}
	prev := uint64(0)
	for i := range n {
		ev, ok := next()
		if !ok {
			t.Fatalf("stream ended after %d events", i)
		}
		if ev.Kind != WatchUpsert {
			continue
		}
		if ev.Stamp <= prev {
			t.Fatalf("event %d stamped %d, not past the previous %d", i, ev.Stamp, prev)
		}
		prev = ev.Stamp
	}
}

// TestIdleShardDoesNotPinTheWatermark: the watermark is a minimum across shards, so a
// shard that never sees a write would hold the whole collection at zero unless it is
// advanced. Most of these shards never get one.
func TestIdleShardDoesNotPinTheWatermark(t *testing.T) {
	c := New(Options{Shards: 16, WatchHistory: 256})
	defer c.Close()
	seq, err := c.Watch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	next, stop := drainTo(t, seq)
	defer stop()

	if err := c.Put([]byte("only.0"), clockAd(t, 1)); err != nil {
		t.Fatal(err)
	}
	if ev, ok := next(); !ok || ev.Kind != WatchUpsert {
		t.Fatalf("expected the live upsert, got %+v (ok=%v)", ev, ok)
	}
	if wm := c.WatchWatermark(); wm == 0 {
		t.Fatal("watermark is 0 with 15 of 16 shards idle; an idle shard is pinning it")
	}
}

// TestChangesAfterAWatermarkAreStampedAboveIt is the safety property the watermark sells:
// once it has named a version, nothing committed later may be stamped at or below it, or a
// consumer that resumed at that version would never learn of the change.
//
// Note what the watermark does NOT mean: it covers every change handed to a watcher's
// buffer, not every change the application has read out of it. Events already queued are
// legitimately at or below it.
func TestChangesAfterAWatermarkAreStampedAboveIt(t *testing.T) {
	c := New(Options{Shards: 4, WatchHistory: 256})
	defer c.Close()
	seq, err := c.Watch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	next, stop := drainTo(t, seq)
	defer stop()

	for round := range 10 {
		wm := c.WatchWatermark()
		key := fmt.Sprintf("after-%d.0", round)
		if err := c.Put([]byte(key), clockAd(t, round)); err != nil {
			t.Fatal(err)
		}
		got := liveStamps(t, next, 1)
		if got[key] <= wm {
			t.Fatalf("%q was committed after a watermark of %d but stamped %d", key, wm, got[key])
		}
	}
}

// TestWatermarkIsNonDecreasing: a consumer publishes it as a version, so it may never go
// backwards -- including across the idle advance, which is the one place it moves without
// anything being written.
func TestWatermarkIsNonDecreasing(t *testing.T) {
	c := New(Options{Shards: 8, WatchHistory: 256})
	defer c.Close()
	seq, err := c.Watch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	next, stop := drainTo(t, seq)
	defer stop()

	prev := c.WatchWatermark()
	for i := range 40 {
		if i%3 == 0 { // some rounds write, some only sample
			if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), clockAd(t, i)); err != nil {
				t.Fatal(err)
			}
			liveStamps(t, next, 1)
		}
		wm := c.WatchWatermark()
		if wm < prev {
			t.Fatalf("round %d: watermark went backwards, %d then %d", i, prev, wm)
		}
		prev = wm
	}
}

// TestWatermarkCatchesUpWithWhatWasDelivered is a liveness property, not a safety one.
// The watermark is a minimum across shards, so it may sit below a stamp another shard has
// already handed out -- a consumer resuming there just sees those changes again. What it
// may not do is stay behind: once the clock moves past the burst, every idle shard
// advances and the watermark covers everything.
//
// A burst is exactly where this is interesting. Stamps forced past one another inside a
// single millisecond run AHEAD of the clock, so the shards that took none of them sit at
// an earlier reading until it catches up.
func TestWatermarkCatchesUpWithWhatWasDelivered(t *testing.T) {
	c := New(Options{Shards: 4, WatchHistory: 256})
	defer c.Close()
	seq, err := c.Watch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	next, stop := drainTo(t, seq)
	defer stop()

	const n = 30
	for i := range n {
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), clockAd(t, i)); err != nil {
			t.Fatal(err)
		}
	}
	var high uint64
	for _, st := range liveStamps(t, next, n) {
		if st > high {
			high = st
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		wm := c.WatchWatermark()
		if wm >= high {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("watermark stuck at %d, below the highest stamp delivered (%d), for 5s", wm, high)
		}
	}
}

// TestWatermarkExcludesACommitStillPublishing is the watermark's safety property observed
// where it is actually visible: inside a commit's publish, with its seq still pending.
// Sampling between writes cannot see this, because a synchronous write has already
// finished publishing by the time it returns.
func TestWatermarkExcludesACommitStillPublishing(t *testing.T) {
	c := New(Options{Shards: 1, WatchHistory: 256})
	defer c.Close()
	seq, err := c.Watch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	next, stop := drainTo(t, seq)
	defer stop()

	var samples []uint64
	publishMidHook = func() { samples = append(samples, c.WatchWatermark()) }
	defer func() { publishMidHook = nil }()

	tx := c.Begin()
	const n = 5
	for i := range n {
		tx.Put([]byte(fmt.Sprintf("tx-%d.0", i)), clockAd(t, i))
	}
	if r := tx.Commit(); r.Committed != n {
		t.Fatalf("commit applied %d writes, want %d", r.Committed, n)
	}
	if len(samples) == 0 {
		t.Fatal("the publish hook never ran; nothing was sampled mid-commit")
	}

	got := liveStamps(t, next, n)
	var stamp uint64
	for _, st := range got {
		stamp = st // one commit, so every event carries the same stamp
	}
	if stamp == 0 {
		t.Fatal("the committed events carry no stamp")
	}
	for i, wm := range samples {
		if wm >= stamp {
			t.Errorf("sample %d taken while the commit was still publishing read %d, at or "+
				"past that commit's own stamp %d", i, wm, stamp)
		}
	}
}
