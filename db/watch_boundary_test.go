package db

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// TestWatchFromCursorDeleteExactlyOnce races a watch resumed from WatchCursor against a
// committed delete, many times, on a persistent store (whose msync widens the window
// between a delete advancing commitSeq and its publish). The delete must reach the watcher exactly once: a lost
// one leaves a replica holding a row the source no longer has until its next Reset; a
// duplicate breaks exactly-once per cursor. Bounded by wall time so it stays cheap
// under -race.
func TestWatchFromCursorDeleteExactlyOnce(t *testing.T) {
	t.Parallel()
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	budget := 2 * time.Second
	if testing.Short() {
		budget = 300 * time.Millisecond
	}
	commit := func(f func(*Txn)) {
		t.Helper()
		tx := d.Begin()
		f(tx)
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	bad := 0
	iters := 0
	for stop := time.Now().Add(budget); time.Now().Before(stop) && bad < 5; iters++ {
		commit(func(tx *Txn) { tx.NewClassAd("k", classad.New()) })
		cur, err := d.WatchCursor()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		seq, err := d.Watch(ctx, cur)
		if err != nil {
			t.Fatal(err)
		}
		var log []string
		deletes, resets := 0, 0
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Stop at mark only once live: catch-up visits shards in turn, so mark can come
			// before the shard holding k has been caught up.
			synced, marked := false, false
			for ev := range seq {
				log = append(log, fmt.Sprintf("%d:%s", ev.Kind, ev.Key))
				switch {
				case ev.Kind == WatchDelete && ev.Key == "k":
					deletes++
				case ev.Kind == WatchReset || ev.Kind == WatchResync:
					resets++
					return
				case ev.Kind == WatchSynced:
					synced = true
				case ev.Kind == WatchUpsert && ev.Key == "mark":
					marked = true
				}
				if synced && marked {
					return
				}
			}
		}()
		commit(func(tx *Txn) { tx.DestroyClassAd("k") })
		commit(func(tx *Txn) { tx.NewClassAd("mark", classad.New()) })
		wg.Wait()
		cancel()
		if resets == 0 && deletes != 1 {
			bad++
			t.Errorf("iteration %d: delete of k delivered %d times, want 1: %v", iters, deletes, log)
		}
		commit(func(tx *Txn) { tx.DestroyClassAd("mark") })
	}
	t.Logf("%d iterations", iters)
}
