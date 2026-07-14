package db

import (
	"os"
	"testing"
	"time"
)

func TestWatcherFDNotify(t *testing.T) {
	d, _ := Open("")
	defer d.Close()

	// A pipe stands in for the eventfd the C++ (DaemonCore) side would create and poll.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	watcher, err := NewWatcher(d, int(w.Fd()), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Stop()
	time.Sleep(20 * time.Millisecond) // let the watch subscribe before we commit

	tx := d.Begin()
	tx.NewClassAd("k", mustAd(t, "N = 1"))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// The wakeup byte must arrive on the read end.
	woke := make(chan struct{})
	go func() {
		var buf [8]byte
		if n, _ := r.Read(buf[:]); n >= 1 {
			close(woke)
		}
	}()
	select {
	case <-woke:
	case <-time.After(2 * time.Second):
		t.Fatal("no wakeup byte written to the notify fd")
	}

	// Drain (polling, since the watch goroutine enqueues asynchronously); a full-replay
	// watch leads with a WatchReset, then the Upsert of "k".
	if !drainFor(watcher, func(ev WatchEvent) bool { return ev.Kind == WatchUpsert && ev.Key == "k" }) {
		t.Fatal("did not see the Upsert of k")
	}

	// A delete produces a Delete event.
	tx = d.Begin()
	tx.DestroyClassAd("k")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if !drainFor(watcher, func(ev WatchEvent) bool { return ev.Kind == WatchDelete && ev.Key == "k" }) {
		t.Fatal("did not see the Delete of k")
	}
}

// drainFor polls the watcher until an event satisfies pred (true) or a timeout (false).
func drainFor(w *Watcher, pred func(WatchEvent) bool) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ev, ok := w.Next()
		if !ok {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if pred(ev) {
			return true
		}
	}
	return false
}
