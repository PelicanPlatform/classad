package db

import (
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// drainArchiveCatchup collects a db watch stream through WatchSynced: the ClusterIds upserted,
// whether a Reset came first, and the synced cursor.
func drainArchiveCatchup(t *testing.T, seq iter.Seq[WatchEvent]) (ids []int64, reset bool, cur []byte) {
	t.Helper()
	for ev := range seq {
		switch ev.Kind {
		case WatchReset:
			reset = true
		case WatchUpsert:
			v, _ := ev.Ad.EvaluateAttrInt("ClusterId")
			ids = append(ids, v)
		case WatchSynced:
			return ids, reset, ev.Cursor
		}
	}
	t.Fatal("watch ended before WatchSynced")
	return nil, false, nil
}

func appendClusters(t *testing.T, a *ArchiveTable, from, to int) {
	t.Helper()
	for i := from; i < to; i++ {
		if err := a.AppendOld(fmt.Sprintf("ClusterId = %d\nJobStatus = 4", i)); err != nil {
			t.Fatal(err)
		}
	}
}

func wantIDs(t *testing.T, what string, ids []int64, from, to int) {
	t.Helper()
	if len(ids) != to-from {
		t.Fatalf("%s: got %d records, want %d: %v", what, len(ids), to-from, ids)
	}
	for i, v := range ids {
		if v != int64(from+i) {
			t.Fatalf("%s: record %d is ClusterId %d, want %d", what, i, v, from+i)
		}
	}
}

func reopenArchive(t *testing.T, dir, name string) (*Catalog, *ArchiveTable) {
	t.Helper()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := cat.ArchiveTable(name)
	if !ok {
		t.Fatalf("archive %q missing after reopen", name)
	}
	return cat, a
}

func watchArchive(t *testing.T, a *ArchiveTable, ctx context.Context, cur []byte) iter.Seq[WatchEvent] {
	t.Helper()
	seq, err := a.Watch(ctx, cur)
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

// TestArchiveTableWatchResumesAcrossReopen: an ArchiveTable watch cursor survives a clean
// catalog Close/Open -- no Reset, exactly the records appended after it (before and after the
// restart, catch-up and live) -- while removing the clean-shutdown marker (as a crash does)
// forces a Reset.
func TestArchiveTableWatchResumesAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := cat.CreateArchiveTable("history", ArchiveConfig{SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	appendClusters(t, hist, 0, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ids, _, cur := drainArchiveCatchup(t, watchArchive(t, hist, ctx, nil))
	wantIDs(t, "initial replay", ids, 0, 100)
	appendClusters(t, hist, 100, 130)
	if err = cat.Close(); err != nil {
		t.Fatal(err)
	}

	cat, hist = reopenArchive(t, dir, "history")
	ids, reset, cur2 := drainArchiveCatchup(t, watchArchive(t, hist, ctx, cur))
	if reset {
		t.Fatal("resume across a clean reopen must not Reset")
	}
	wantIDs(t, "resume", ids, 100, 130)

	// Live delivery after the reopen, from the resumed cursor.
	wctx, wcancel := context.WithCancel(ctx)
	live := make(chan int64, 64)
	synced := make(chan bool, 1)
	go func() {
		defer close(live)
		for ev := range watchArchive(t, hist, wctx, cur2) {
			switch ev.Kind {
			case WatchReset:
				synced <- false
				return
			case WatchSynced:
				synced <- true
			case WatchUpsert:
				v, _ := ev.Ad.EvaluateAttrInt("ClusterId")
				live <- v
			}
		}
	}()
	if !<-synced {
		t.Fatal("live resume must not Reset")
	}
	appendClusters(t, hist, 130, 140)
	var got []int64
	timeout := time.After(5 * time.Second)
	for len(got) < 10 {
		select {
		case v := <-live:
			got = append(got, v)
		case <-timeout:
			t.Fatalf("live: got %v, want 130..139", got)
		}
	}
	wcancel()
	for v := range live {
		got = append(got, v)
	}
	wantIDs(t, "live after reopen", got, 130, 140)
	cur3, err := hist.WatchCursor()
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}

	// Crash stand-in: the marker a clean Close writes is gone, so the epoch rotates.
	if err := os.Remove(filepath.Join(dir, archivesSubdir, "history", "watch.epoch")); err != nil {
		t.Fatal(err)
	}
	cat, hist = reopenArchive(t, dir, "history")
	defer cat.Close()
	ids, reset, _ = drainArchiveCatchup(t, watchArchive(t, hist, ctx, cur3))
	if !reset {
		t.Fatal("without the clean-shutdown marker a resume must Reset")
	}
	wantIDs(t, "reset replay", ids, 0, 140)
}

// TestTableWatchResetsAcrossReopen: a mutable table keeps a per-process epoch, so a restart
// still Resets its watchers.
func TestTableWatchResetsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := cat.CreateTable("jobs")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ ClusterId = %d ]`, i))
		if err = jobs.Put(fmt.Sprintf("%d.0", i), ad); err != nil {
			t.Fatal(err)
		}
	}
	cur, err := jobs.WatchCursor()
	if err != nil {
		t.Fatal(err)
	}
	if err = cat.Close(); err != nil {
		t.Fatal(err)
	}
	cat, err = OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	jobs, ok := cat.Table("jobs")
	if !ok {
		t.Fatal("table missing after reopen")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seq, err := jobs.Watch(ctx, cur)
	if err != nil {
		t.Fatal(err)
	}
	ids, reset, _ := drainArchiveCatchup(t, seq)
	if !reset {
		t.Fatal("a mutable table must Reset across a reopen")
	}
	if len(ids) != 10 {
		t.Fatalf("reset replay yielded %d ads, want 10", len(ids))
	}
}
