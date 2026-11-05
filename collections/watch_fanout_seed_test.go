package collections

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestWatchFanoutBaselineIsTheSnapshotsState: a parent change committed after the watcher's
// S_reg snapshot must still fan out to its children.
//
// The live fan-out emits a parent's children only where the parent's non-private signature
// DIFFERS from a baseline, so the baseline has to be the state at S_reg: everything past S_reg
// is delivered live and is diffed against it. The baseline used to be read much later, when the
// live phase began, with the whole of catch-up in between -- so a write landing in that window
// was already in the baseline by the time it was read, diffed against itself, and its children
// were never delivered. The parent's own event is not emitted either (it is structural), so the
// change vanished from the stream entirely.
//
// watchSnapshotHook runs in exactly that window, which makes the race a certainty rather than
// something to hunt for with -count.
func TestWatchFanoutBaselineIsTheSnapshotsState(t *testing.T) {
	c := New(Options{Shards: 1, WatchHistory: 256,
		ParentKeyFor: jobParentKey, IsStructural: jobStructural})
	defer c.Close()
	if err := c.Put([]byte("1.-1"), mustAd(t, `[DAGManJobId=42; Owner="alice"]`)); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"1.0", "1.1", "1.2"} {
		if err := c.Put([]byte(k), mustAd(t, `[ProcId=0]`)); err != nil {
			t.Fatal(err)
		}
	}

	// Committed after S_reg is read and before catch-up runs: live, not catch-up.
	var once sync.Once
	watchSnapshotHook = func(wc *Collection) {
		if wc == c {
			once.Do(func() {
				_ = c.Put([]byte("1.-1"), mustAd(t, `[DAGManJobId=43; Owner="alice"]`))
			})
		}
	}
	t.Cleanup(func() { watchSnapshotHook = nil })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	seq, err := c.Watch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Every child of the changed parent must arrive after Synced, carrying the new
	// inherited value. A sentinel bounds the read: the fan-out either precedes it or did
	// not happen.
	live := map[string]bool{}
	synced := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range seq {
			switch {
			case ev.Kind == WatchSynced:
				synced = true
			case ev.Kind == WatchUpsert && synced:
				if string(ev.Key) == "9.0" {
					return
				}
				live[string(ev.Key)] = true
			}
		}
	}()
	// Give the fan-out a chance to be wrong before the sentinel closes the stream.
	time.Sleep(50 * time.Millisecond)
	if err := c.Put([]byte("9.0"), mustAd(t, `[ProcId=0]`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("the watch never reached the sentinel")
	}

	for _, k := range []string{"1.0", "1.1", "1.2"} {
		if !live[k] {
			t.Errorf("%s was never delivered: the parent change committed after the "+
				"snapshot did not fan out, so a watcher never learns its inherited "+
				"attributes changed", k)
		}
	}
}
