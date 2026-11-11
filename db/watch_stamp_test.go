package db

import (
	"context"
	"fmt"
	"iter"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// pullPastSynced opens a watch and reads it up to WatchSynced, returning the puller and
// the watermark the stream reported it had caught up to.
func pullPastSynced(t *testing.T, db *DB) (next func() (WatchEvent, bool), stop func(), synced uint64) {
	t.Helper()
	seq, err := db.Watch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	next, stop = iter.Pull(seq)
	for {
		ev, ok := next()
		if !ok {
			t.Fatal("watch ended before WatchSynced")
		}
		if ev.Kind == WatchSynced {
			return next, stop, ev.Stamp
		}
	}
}

// TestTwoReadersStampAChangeIdentically is why the stamp exists. Two servers reading the
// same DB must hand a client a version that means the same thing on each, which a counter
// kept per process cannot do.
func TestTwoReadersStampAChangeIdentically(t *testing.T) {
	db, err := OpenConfig(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	nextA, stopA, syncedA := pullPastSynced(t, db)
	defer stopA()
	nextB, stopB, _ := pullPastSynced(t, db)
	defer stopB()
	if syncedA == 0 {
		t.Fatal("WatchSynced carried no watermark")
	}

	const n = 15
	for i := range n {
		ad, err := classad.Parse(fmt.Sprintf("[ ClusterId = %d; JobStatus = 2 ]", i))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Put(fmt.Sprintf("%d.0", i), ad); err != nil {
			t.Fatal(err)
		}
	}

	collect := func(next func() (WatchEvent, bool)) map[string]uint64 {
		t.Helper()
		out := map[string]uint64{}
		for len(out) < n {
			ev, ok := next()
			if !ok {
				t.Fatalf("stream ended after %d of %d", len(out), n)
			}
			if ev.Kind == WatchUpsert {
				out[ev.Key] = ev.Stamp
			}
		}
		return out
	}
	sa, sb := collect(nextA), collect(nextB)
	for k, va := range sa {
		if va == 0 {
			t.Errorf("%q was delivered with no stamp", k)
		}
		if va <= syncedA {
			t.Errorf("%q stamped %d, not past the %d the catch-up reported", k, va, syncedA)
		}
		if vb := sb[k]; vb != va {
			t.Errorf("%q: one reader stamped it %d, the other %d", k, va, vb)
		}
	}
	if wm := db.WatchWatermark(); wm < syncedA {
		t.Errorf("watermark %d went backwards from the %d reported at WatchSynced", wm, syncedA)
	}
}
