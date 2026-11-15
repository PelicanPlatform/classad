package collections

import (
	"context"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// A stamp is the comparable version a consumer republishes (a Kubernetes resourceVersion,
// say). An event that arrives with 0 is not "unstamped" to such a consumer: it is a version
// that moved BACKWARDS, and it is indistinguishable from a genuinely early change. Every live
// path has to carry it, so these tests walk the paths that build a WatchEvent themselves
// rather than forwarding one.

// TestWatchFilterKeepsTheStampOnASynthesizedDelete covers the filter path, which turns an
// upsert that stopped matching into a Delete. That Delete is a new event value, so the stamp
// has to be copied onto it from the change that caused it.
func TestWatchFilterKeepsTheStampOnASynthesizedDelete(t *testing.T) {
	match, _ := classad.Parse(`[ M = 1 ]`)
	nomatch, _ := classad.Parse(`[ M = 0 ]`)
	const stamp = 12345
	evs := seqOf(
		WatchEvent{Kind: WatchSynced},
		WatchEvent{Kind: WatchUpsert, Key: []byte("k"), Ad: match, Stamp: stamp - 1},
		WatchEvent{Kind: WatchUpsert, Key: []byte("k"), Ad: nomatch, Stamp: stamp},
	)
	var got []WatchEvent
	for ev := range WatchFilter(evs, matchM) {
		got = append(got, ev)
	}
	if len(got) != 3 || got[2].Kind != WatchDelete {
		t.Fatalf("expected Synced, Upsert, synthesized Delete; got %d events (last kind %d)", len(got), got[len(got)-1].Kind)
	}
	if got[2].Stamp != stamp {
		t.Errorf("synthesized delete stamp = %d, want %d (the stamp of the change that caused it)", got[2].Stamp, stamp)
	}
}

// TestWatchCoalesceKeepsStamps covers the coalescing live path, which rebuilds each event
// from the raw record rather than forwarding the one it was handed.
func TestWatchCoalesceKeepsStamps(t *testing.T) {
	c, err := Open(Options{Shards: 1, WatchHistory: 1024, WatchCoalesce: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seq, err := c.Watch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	next, stop := drainTo(t, seq)
	defer stop()

	if err := c.Put([]byte("a"), clockAd(t, 1)); err != nil {
		t.Fatal(err)
	}
	for {
		ev, ok := next()
		if !ok {
			t.Fatal("coalesced stream ended before the live upsert")
		}
		if ev.Kind != WatchUpsert {
			continue
		}
		if ev.Stamp == 0 {
			t.Fatal("coalesced live upsert carries stamp 0; a consumer republishing it as a version would go backwards")
		}
		if wm := c.WatchWatermark(); ev.Stamp > wm {
			t.Errorf("coalesced stamp %d is above the watermark %d", ev.Stamp, wm)
		}
		return
	}
}
