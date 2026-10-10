package db

import (
	"bytes"
	"context"
	"fmt"
	"iter"
	"sort"
	"sync"
	"testing"
	"time"
)

// follower tails a watch the way a replica does: Reset builds into a shadow that goes live at
// Synced, the cursor is persisted from every event carrying one, and Resync reconnects with
// it. rows is what the replica holds.
type follower struct {
	mu      sync.Mutex
	rows    map[string]bool
	shadow  map[string]bool
	synced  int
	resets  int
	resyncs int
}

func startFollower(t *testing.T, watch func(ctx context.Context, cur []byte) (iter.Seq[WatchEvent], error)) *follower {
	t.Helper()
	f := &follower{rows: map[string]bool{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		var cur []byte
		for ctx.Err() == nil {
			seq, err := watch(ctx, cur)
			if err != nil {
				t.Error(err)
				return
			}
			for ev := range seq {
				f.mu.Lock()
				target := f.rows
				if f.shadow != nil {
					target = f.shadow
				}
				switch ev.Kind {
				case WatchReset:
					f.resets++
					f.shadow = map[string]bool{}
				case WatchUpsert:
					target[ev.Key] = true
				case WatchDelete:
					delete(target, ev.Key)
				case WatchSynced:
					f.synced++
					if f.shadow != nil {
						f.rows, f.shadow = f.shadow, nil
					}
				case WatchResync:
					f.resyncs++
				}
				if ev.Cursor != nil {
					cur = ev.Cursor
				}
				f.mu.Unlock()
			}
		}
	}()
	return f
}

func (f *follower) waitFor(t *testing.T, what string, cond func(f *follower) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		f.mu.Lock()
		ok := cond(f)
		state := fmt.Sprintf("rows=%d synced=%d resets=%d resyncs=%d", len(f.rows), f.synced, f.resets, f.resyncs)
		f.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s (%s)", what, state)
		}
		time.Sleep(time.Millisecond)
	}
}

func (f *follower) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.rows {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestWatchResetsOnCatalogRestore: Restore truncates each table and reloads it. A live
// watcher used to keep every row written after the snapshot (Truncate told it nothing, and
// the reload only re-upserted the snapshot's rows). It must Reset to the restored contents.
func TestWatchResetsOnCatalogRestore(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	jobs, _ := cat.CreateTable("jobs")
	fillTable(t, jobs, "j", 100)
	var snap bytes.Buffer
	if err := cat.Snapshot(&snap); err != nil {
		t.Fatal(err)
	}
	fillTable(t, jobs, "extra", 50)

	f := startFollower(t, jobs.Watch)
	f.waitFor(t, "initial sync", func(f *follower) bool { return f.synced == 1 && len(f.rows) == 150 })

	if err := cat.Restore(bytes.NewReader(snap.Bytes())); err != nil {
		t.Fatal(err)
	}
	f.waitFor(t, "a Reset to the restored contents", func(f *follower) bool {
		return f.resyncs >= 1 && f.shadow == nil && f.resets >= 2 && len(f.rows) == 100
	})
	for _, k := range f.keys() {
		if k[0] != 'j' {
			t.Fatalf("follower kept %q, written after the snapshot", k)
		}
	}
}

// TestWatchResetsOnTruncate: DB.Truncate (the admin "truncate") empties a live follower.
func TestWatchResetsOnTruncate(t *testing.T) {
	d, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	fillTable(t, d, "j", 100)
	f := startFollower(t, d.Watch)
	f.waitFor(t, "initial sync", func(f *follower) bool { return f.synced == 1 && len(f.rows) == 100 })

	d.Truncate()
	fillTable(t, d, "post", 3)
	f.waitFor(t, "a Reset to the post-truncate rows", func(f *follower) bool {
		return f.resyncs >= 1 && f.shadow == nil && len(f.rows) == 3
	})
	if got := f.keys(); got[0] != "post0" || got[2] != "post2" {
		t.Fatalf("follower holds %v, want post0..post2", got)
	}
}

// TestArchiveTableWatchResetsOnTruncate: ArchiveTable.Truncate (the history re-sync reset)
// forces a live follower through a Reset, and a pre-truncate cursor resumed later Resets.
func TestArchiveTableWatchResetsOnTruncate(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	hist, err := cat.CreateArchiveTable("history", ArchiveConfig{SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	appendClusters(t, hist, 0, 20)
	pre, _ := hist.WatchCursor()
	f := startFollower(t, hist.Watch)
	f.waitFor(t, "initial sync", func(f *follower) bool { return f.synced == 1 })

	hist.Truncate()
	f.waitFor(t, "a Reset after the truncate", func(f *follower) bool {
		return f.resyncs >= 1 && f.resets >= 2 && f.shadow == nil && f.synced == f.resyncs+1
	})
	appendClusters(t, hist, 100, 105)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ids, reset, _ := drainArchiveCatchup(t, watchArchive(t, hist, ctx, pre))
	if !reset {
		t.Fatal("a pre-truncate archive cursor must Reset")
	}
	wantIDs(t, "pre-truncate cursor", ids, 100, 105)
}
