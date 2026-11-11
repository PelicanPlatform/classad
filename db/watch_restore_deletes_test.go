package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestWatchRetainSeqsResumesDeletesAcrossReopen is the db-level contract htcondordb needs:
// with a retention window configured, a watcher holding a cursor from before the process
// restarted is caught up incrementally -- including the keys that went away while it was
// gone -- instead of being told to start over.
//
// The two halves run the identical history. WatchRetainSeqs is the only difference.
func TestWatchRetainSeqsResumesDeletesAcrossReopen(t *testing.T) {
	const deletes = 6

	run := func(t *testing.T, retainSeqs uint64) (gone map[string]bool, reset bool) {
		t.Helper()
		cfg := Config{Dir: t.TempDir(), WatchRetainSeqs: retainSeqs}
		db, err := OpenConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 40 {
			ad, err := classad.Parse(fmt.Sprintf("[ ClusterId = %d; JobStatus = 2 ]", i))
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Put(fmt.Sprintf("%d.0", i), ad); err != nil {
				t.Fatal(err)
			}
		}
		seq, err := db.Watch(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _, cursor := drainArchiveCatchup(t, seq)

		for i := range deletes {
			ok, err := db.Delete(fmt.Sprintf("%d.0", i))
			if err != nil || !ok {
				t.Fatalf("Delete(%d.0) = %v, %v", i, ok, err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		db, err = OpenConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		seq, err = db.Watch(context.Background(), cursor)
		if err != nil {
			t.Fatal(err)
		}
		gone = map[string]bool{}
		for ev := range seq {
			switch ev.Kind {
			case WatchReset:
				reset = true
			case WatchDelete:
				gone[ev.Key] = true
			case WatchSynced:
				return gone, reset
			}
		}
		t.Fatal("watch ended before WatchSynced")
		return nil, false
	}

	t.Run("window-off", func(t *testing.T) {
		if _, reset := run(t, 0); !reset {
			t.Fatal("a cursor from before the reopen resumed with no window configured; " +
				"the window case below would prove nothing")
		}
	})

	t.Run("window-set", func(t *testing.T) {
		gone, reset := run(t, 1<<20)
		if reset {
			t.Fatal("resume across the reopen fell back to a full replay")
		}
		if len(gone) != deletes {
			t.Errorf("got %d delete events, want %d", len(gone), deletes)
		}
		for i := range deletes {
			if k := fmt.Sprintf("%d.0", i); !gone[k] {
				t.Errorf("no delete event for %q", k)
			}
		}
	})
}
