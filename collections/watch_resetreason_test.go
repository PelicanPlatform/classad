package collections

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// drainReason runs a watch's catch-up and reports the reason on the Reset it began with,
// or WatchResetNone if it resumed incrementally. It stops at WatchSynced, as drainCatchup
// does, so the live phase never runs.
func drainReason(t *testing.T, seq func(func(WatchEvent) bool)) WatchResetReason {
	t.Helper()
	reason := WatchResetNone
	saw := false
	for ev := range seq {
		switch ev.Kind {
		case WatchReset:
			reason, saw = ev.Reason, true
			if reason == WatchResetNone {
				t.Error("a Reset carries no reason; every path that sets full must name one")
			}
		case WatchSynced:
			if !saw && ev.Reason != WatchResetNone {
				t.Errorf("WatchSynced carries reason %v; only a Reset should", ev.Reason)
			}
			return reason
		}
	}
	return reason
}

func putN(t *testing.T, c *Collection, key string, n int) {
	t.Helper()
	ad, err := classad.Parse(fmt.Sprintf(`[ N = %d ]`, n))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put([]byte(key), ad); err != nil {
		t.Fatal(err)
	}
}

// TestWatchResetReasons pins the reason each distinct cause of a Reset reports. They are
// one event kind today, which is why a subscriber cannot tell a retention loss (size the
// window, or the collector is lagging) from an unclean restart (look at the crash) from a
// cursor ahead of the head (a restored backup -- the alarming one).
func TestWatchResetReasons(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	t.Run("initial", func(t *testing.T) {
		c, err := Open(Options{Shards: 1, WatchHistory: 1024})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		putN(t, c, "a", 1)
		// A first subscribe has no cursor. The snapshot that follows is the point, not a
		// loss, and a consumer that alerts on Reset must not page on this one.
		if got := drainReason(t, mustWatch(t, c, ctx, nil)); got != WatchResetInitial {
			t.Errorf("nil cursor: reason = %v, want initial", got)
		}
	})

	t.Run("cursor-invalid", func(t *testing.T) {
		c, err := Open(Options{Shards: 1, WatchHistory: 1024})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		putN(t, c, "a", 1)
		// Too short to decode: not this store's cursor at all.
		if got := drainReason(t, mustWatch(t, c, ctx, []byte{1, 2, 3})); got != WatchResetCursorInvalid {
			t.Errorf("undecodable cursor: reason = %v, want cursor-invalid", got)
		}
		// Decodes, but names a different shard count.
		wrongShards := encodeCursor(c.hub.epoch, []uint64{0, 0, 0, 0})
		if got := drainReason(t, mustWatch(t, c, ctx, wrongShards)); got != WatchResetCursorInvalid {
			t.Errorf("wrong shard count: reason = %v, want cursor-invalid", got)
		}
	})

	t.Run("epoch", func(t *testing.T) {
		// A mutable collection with no retention window rotates its epoch on every restart
		// (watchepoch.go), so a cursor from the previous process names a store generation
		// that no longer exists.
		dir := t.TempDir()
		c, err := Open(Options{Dir: dir, Shards: 1, WatchHistory: 1024})
		if err != nil {
			t.Fatal(err)
		}
		putN(t, c, "a", 1)
		_, _, cursor := drainCatchup(t, mustWatch(t, c, ctx, nil))
		if cursor == nil {
			t.Fatal("no cursor from the initial sync")
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		c2, err := Open(Options{Dir: dir, Shards: 1, WatchHistory: 1024})
		if err != nil {
			t.Fatal(err)
		}
		defer c2.Close()
		if got := drainReason(t, mustWatch(t, c2, ctx, cursor)); got != WatchResetEpoch {
			t.Errorf("cursor from a previous epoch: reason = %v, want epoch", got)
		}
	})

	t.Run("retention-append", func(t *testing.T) {
		// An append-only cursor below the floor: the records between it and the floor were
		// rotated out. They are GONE, which is what makes this different from every other
		// reason -- the replay that follows does not contain them.
		c, err := Open(Options{
			AppendOnly:   true,
			Dir:          t.TempDir(),
			SegmentSize:  1 << 12,
			WatchHistory: 4096,
			Retention:    Retention{MaxSegments: 2},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		for i := range 20 {
			putN(t, c, "k", i)
		}
		_, _, cursor := drainCatchup(t, mustWatch(t, c, ctx, nil))
		if cursor == nil {
			t.Fatal("no cursor from the initial sync")
		}
		for i := 20; i < 400; i++ {
			putN(t, c, "k", i)
		}
		dropped, err := c.Rotate(0)
		if err != nil {
			t.Fatal(err)
		}
		if dropped == 0 {
			t.Fatal("rotation dropped nothing, so the cursor is still within the floor")
		}
		if got := drainReason(t, mustWatch(t, c, ctx, cursor)); got != WatchResetRetention {
			t.Errorf("cursor below the append floor: reason = %v, want retention", got)
		}
	})

	t.Run("retention-deletes", func(t *testing.T) {
		// The mutable half: a Truncate takes the delete markers with it, so storage can no
		// longer stand in for the journal and a cursor from before it cannot be told which
		// keys went away.
		c, err := Open(Options{
			Dir:          t.TempDir(),
			Shards:       1,
			WatchHistory: 1024,
			WatchRetain:  time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		putN(t, c, "a", 1)
		_, _, cursor := drainCatchup(t, mustWatch(t, c, ctx, nil))
		if cursor == nil {
			t.Fatal("no cursor from the initial sync")
		}
		c.Truncate()
		putN(t, c, "b", 2)
		if got := drainReason(t, mustWatch(t, c, ctx, cursor)); got != WatchResetRetention {
			t.Errorf("cursor from before a Truncate: reason = %v, want retention", got)
		}
	})

	t.Run("ahead", func(t *testing.T) {
		// This store's own epoch, with a sequence it has never issued: what an older copy
		// of a cleanly-closed store looks like when it is reopened and a cursor from the
		// newer one is replayed into it. Those sequences will be handed to different
		// records, so resuming would silently skip them.
		c, err := Open(Options{Shards: 1, WatchHistory: 1024})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		putN(t, c, "a", 1)
		ahead := encodeCursor(c.hub.epoch, []uint64{1 << 40})
		if got := drainReason(t, mustWatch(t, c, ctx, ahead)); got != WatchResetAhead {
			t.Errorf("cursor ahead of the head: reason = %v, want ahead", got)
		}
	})

	t.Run("no-reset-on-a-clean-resume", func(t *testing.T) {
		// The negative: an incremental resume reports no reason, because there was no
		// Reset. Without this the whole suite would pass on a Watch that Reset every time.
		c, err := Open(Options{Shards: 1, WatchHistory: 1024})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		putN(t, c, "a", 1)
		_, _, cursor := drainCatchup(t, mustWatch(t, c, ctx, nil))
		if cursor == nil {
			t.Fatal("no cursor from the initial sync")
		}
		putN(t, c, "b", 2)
		if got := drainReason(t, mustWatch(t, c, ctx, cursor)); got != WatchResetNone {
			t.Errorf("a resumable cursor Reset with reason %v; it should not have Reset at all", got)
		}
	})
}

// TestWatchResetReasonStrings pins the wire-visible spelling. These cross a process
// boundary (db -> replicate -> the changefeed's JSON), so a consumer matches on the text.
func TestWatchResetReasonStrings(t *testing.T) {
	for r, want := range map[WatchResetReason]string{
		WatchResetNone:          "none",
		WatchResetInitial:       "initial",
		WatchResetEpoch:         "epoch",
		WatchResetRetention:     "retention",
		WatchResetAhead:         "ahead",
		WatchResetCursorInvalid: "cursor-invalid",
	} {
		if got := r.String(); got != want {
			t.Errorf("WatchResetReason(%d).String() = %q, want %q", r, got, want)
		}
	}
}
