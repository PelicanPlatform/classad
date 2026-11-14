package db

import (
	"context"
	"iter"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// TestArchiveWatchCarriesStampAndReason is a parity test against the mutable table's watch.
// ArchiveTable.Watch says it converts events "so the two are wire-identical", and it builds
// its own WatchEvent rather than sharing the mutable path's -- so every field added to the
// event has to be added in two places. Stamp was added in one: an archive subscriber got 0
// on every event while a table subscriber got the real clock, and nothing failed.
//
// The archive is the table a change-feed collector normally follows, so this is the path
// that matters most for a consumer republishing the stamp as its own version.
func TestArchiveWatchCarriesStampAndReason(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a, err := cat.CreateArchiveTable("events", ArchiveConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A first subscribe (nil cursor) Resets because there is nothing to resume from. That
	// is not a loss, and it must say so.
	seq, err := a.Watch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	next, stop := iter.Pull(seq)
	defer stop()
	var sawReset bool
	for {
		ev, ok := next()
		if !ok {
			t.Fatal("archive watch ended before WatchSynced")
		}
		if ev.Kind == WatchReset {
			sawReset = true
			if ev.Reason != WatchResetInitial {
				t.Errorf("first subscribe to an archive: reset reason = %v, want initial", ev.Reason)
			}
		}
		if ev.Kind == WatchSynced {
			if !sawReset {
				t.Fatal("first subscribe did not Reset, so the reason was never exercised")
			}
			break
		}
	}

	ad, err := classad.Parse(`[ N = 1 ]`)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Append(ad); err != nil {
		t.Fatal(err)
	}

	done := make(chan WatchEvent, 1)
	go func() {
		for {
			ev, ok := next()
			if !ok {
				close(done)
				return
			}
			if ev.Kind == WatchUpsert {
				done <- ev
				return
			}
		}
	}()
	select {
	case ev, ok := <-done:
		if !ok {
			t.Fatal("archive watch ended before the live append arrived")
		}
		if ev.Stamp == 0 {
			t.Fatal("live archive upsert carries stamp 0; the mutable table's watch stamps the same change")
		}
		if wm := a.WatchWatermark(); wm != 0 && ev.Stamp > wm {
			t.Errorf("archive stamp %d is above its watermark %d", ev.Stamp, wm)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no live event from the archive watch")
	}
}
