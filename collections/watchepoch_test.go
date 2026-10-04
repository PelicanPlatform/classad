package collections

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

func epochTestOpts(dir string) Options {
	return Options{AppendOnly: true, Dir: dir, SegmentSize: 1 << 12, WatchHistory: 4096}
}

func mustOpen(t *testing.T, opts Options) *Collection {
	t.Helper()
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func appendN(t *testing.T, c *Collection, from, to int) {
	t.Helper()
	for i := from; i < to; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N = %d ]`, i))
		if err := c.Put([]byte("k"), ad); err != nil {
			t.Fatal(err)
		}
	}
}

// wantRange fails unless ns is exactly from..to-1 in order: every record once, no extras.
func wantRange(t *testing.T, what string, ns []int64, from, to int) {
	t.Helper()
	if len(ns) != to-from {
		t.Fatalf("%s: got %d records, want %d (%d..%d): %v", what, len(ns), to-from, from, to-1, ns)
	}
	for i, v := range ns {
		if v != int64(from+i) {
			t.Fatalf("%s: record %d is N=%d, want %d", what, i, v, from+i)
		}
	}
}

func cursorEpoch(t *testing.T, cur []byte) uint64 {
	t.Helper()
	e, _, ok := decodeCursor(cur)
	if !ok {
		t.Fatal("undecodable cursor")
	}
	return e
}

// crashAbandon simulates the process dying: segments are unmapped (their written bytes stay
// in the files, as MAP_SHARED pages do after a process crash) but nothing Close does runs.
// The collection must not be used afterward.
func crashAbandon(c *Collection) {
	for _, sh := range c.shards {
		sh.mu.Lock()
		for _, seg := range sh.segs {
			if seg != nil {
				_ = seg.closeUnmap()
			}
		}
		sh.mu.Unlock()
	}
}

// tailAfter locates, in shard 0, the file and offset of the first record with seq > keep, so
// a test can drop everything from there on as an OS crash would drop unsynced pages.
func tailAfter(t *testing.T, c *Collection, keep uint64) (path string, off int64, later []string) {
	t.Helper()
	sh := c.shards[0]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	for _, seg := range sh.segs {
		if seg == nil {
			continue
		}
		if path != "" {
			later = append(later, seg.path)
			continue
		}
		for o := 0; o < seg.used; {
			total := recTotalLen(seg.data, uint32(o))
			if total == 0 {
				break
			}
			if recSeq(seg.data, uint32(o)) > keep {
				path, off = seg.path, int64(o)
				break
			}
			o += int(total)
		}
	}
	if path == "" {
		t.Fatalf("no record with seq > %d", keep)
	}
	return path, off, later
}

// dropTail zeroes path from off to EOF and removes the later segment files (and sidecars).
func dropTail(t *testing.T, path string, off int64, later []string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	if _, err := f.WriteAt(make([]byte, st.Size()-off), off); err != nil {
		t.Fatal(err)
	}
	f.Close()
	for _, p := range later {
		matches, _ := filepath.Glob(p + "*")
		for _, m := range matches {
			os.Remove(m)
		}
	}
}

// TestArchiveWatchResumesAcrossCleanReopen: a cursor issued before a clean Close resumes after
// Open with no Reset, delivering exactly the records appended after it -- those appended before
// the restart, then those appended after it, both via catch-up and live.
func TestArchiveWatchResumesAcrossCleanReopen(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, epochTestOpts(dir))
	appendN(t, c, 0, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ns, _, cur := drainCatchup(t, mustWatch(t, c, ctx, nil))
	wantRange(t, "initial replay", ns, 0, 100)
	headCur, _ := c.WatchCursor()
	appendN(t, c, 100, 150) // spans several 4 KiB segments
	if _, err := os.Stat(filepath.Join(dir, watchEpochFile)); !os.IsNotExist(err) {
		t.Fatalf("watch.epoch must be consumed while open (stat err %v)", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, watchEpochFile)); err != nil {
		t.Fatalf("clean Close must leave watch.epoch: %v", err)
	}

	c = mustOpen(t, epochTestOpts(dir))
	defer c.Close()
	if got, _ := c.WatchCursor(); cursorEpoch(t, got) != cursorEpoch(t, cur) {
		t.Fatal("epoch changed across a clean reopen")
	}
	ns, reset, cur2 := drainCatchup(t, mustWatch(t, c, ctx, cur))
	if reset {
		t.Fatal("resume across a clean reopen must not Reset")
	}
	wantRange(t, "resume (Synced cursor)", ns, 100, 150)
	ns, reset, _ = drainCatchup(t, mustWatch(t, c, ctx, headCur))
	if reset {
		t.Fatal("resume of a WatchCursor cursor across a clean reopen must not Reset")
	}
	wantRange(t, "resume (WatchCursor cursor)", ns, 100, 150)

	// Appended after the reopen: picked up by catch-up from the pre-restart chain of cursors.
	appendN(t, c, 150, 170)
	ns, reset, cur3 := drainCatchup(t, mustWatch(t, c, ctx, cur2))
	if reset {
		t.Fatal("resume after post-reopen appends must not Reset")
	}
	wantRange(t, "post-reopen catch-up", ns, 150, 170)

	// And live: a watcher resumed from cur3 receives new appends exactly once.
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	live := make(chan int64, 64)
	synced := make(chan bool, 1)
	go func() {
		defer close(live)
		for ev := range mustWatch(t, c, wctx, cur3) {
			switch ev.Kind {
			case WatchReset:
				synced <- false
				return
			case WatchSynced:
				synced <- true
			case WatchUpsert:
				v, _ := ev.Ad.EvaluateAttrInt("N")
				live <- v
			}
		}
	}()
	if !<-synced {
		t.Fatal("live resume must not Reset")
	}
	appendN(t, c, 170, 180)
	var got []int64
	timeout := time.After(5 * time.Second)
	for len(got) < 10 {
		select {
		case v := <-live:
			got = append(got, v)
		case <-timeout:
			t.Fatalf("live: got %v, want 170..179", got)
		}
	}
	wcancel()
	for v := range live { // nothing beyond the 10 (no duplicates)
		got = append(got, v)
	}
	wantRange(t, "live after reopen", got, 170, 180)
}

// TestArchiveWatchCrashNeverSkips: after a crash (no Close) whose recovery lost the unsynced
// tail, a cursor that covered the lost records must Reset -- even once the reopened log has
// grown past the cursor and reassigned the lost seqs to new records, which a resumed cursor
// would silently skip.
func TestArchiveWatchCrashNeverSkips(t *testing.T) {
	dir := t.TempDir()
	opts := epochTestOpts(dir)
	opts.SegmentSize = 1 << 20 // one segment: the dropped tail is a suffix of one file
	c := mustOpen(t, opts)
	appendN(t, c, 0, 100)
	if err := c.Close(); err != nil { // a clean marker exists from here
		t.Fatal(err)
	}
	c = mustOpen(t, opts)
	appendN(t, c, 100, 150)
	cur, _ := c.WatchCursor() // head = 150: covers records the crash will lose
	path, off, later := tailAfter(t, c, 120)
	crashAbandon(c)
	dropTail(t, path, off, later) // the OS lost seqs 121..150

	c = mustOpen(t, opts)
	defer c.Close()
	if n := c.Len(); n != 120 {
		t.Fatalf("recovered %d records, want 120 (tail drop did not take)", n)
	}
	appendN(t, c, 1000, 1040) // new records now hold seqs 121..160
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ns, reset, _ := drainCatchup(t, mustWatch(t, c, ctx, cur))
	if !reset {
		t.Fatalf("a cursor from before a crash must Reset, got a resume delivering %v", ns)
	}
	if len(ns) != 160 {
		t.Fatalf("reset replay yielded %d records, want 160", len(ns))
	}
}

// TestArchiveWatchCrashWithoutLossResets: a crash is detected by the missing marker alone, so
// even with every byte intact the epoch rotates (Reset, never a guess).
func TestArchiveWatchCrashWithoutLossResets(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, epochTestOpts(dir))
	appendN(t, c, 0, 50)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = mustOpen(t, epochTestOpts(dir))
	appendN(t, c, 50, 80)
	cur, _ := c.WatchCursor()
	crashAbandon(c)

	c = mustOpen(t, epochTestOpts(dir))
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ns, reset, _ := drainCatchup(t, mustWatch(t, c, ctx, cur))
	if !reset {
		t.Fatal("a cursor from a session that never closed cleanly must Reset")
	}
	wantRange(t, "reset replay", ns, 0, 80)
}

// TestArchiveWatchBadMarkerResets: a corrupt or missing marker rotates the epoch.
func TestArchiveWatchBadMarkerResets(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoil func(path string) error
	}{
		{"corrupt", func(p string) error {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			b[10] ^= 0xff
			return os.WriteFile(p, b, 0o644)
		}},
		{"truncated", func(p string) error { return os.Truncate(p, 12) }},
		{"removed", os.Remove},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			c := mustOpen(t, epochTestOpts(dir))
			appendN(t, c, 0, 40)
			cur, _ := c.WatchCursor()
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if err := tc.spoil(filepath.Join(dir, watchEpochFile)); err != nil {
				t.Fatal(err)
			}
			c = mustOpen(t, epochTestOpts(dir))
			defer c.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ns, reset, _ := drainCatchup(t, mustWatch(t, c, ctx, cur))
			if !reset {
				t.Fatal("a bad marker must rotate the epoch")
			}
			wantRange(t, "reset replay", ns, 0, 40)
		})
	}
}

// TestArchiveWatchHighWaterSurvivesEmptyLog: Truncate leaves no record carrying the highest
// seq, so recovery alone would restart numbering below a live cursor and the new records would
// fall under it. The marker's high-water mark keeps the numbering monotonic. (The directory
// snapshot also carries commitSeq, but it is best effort and skipped for time-travel and
// chained collections, so it is removed here to exercise recovery from the segments alone.)
func TestArchiveWatchHighWaterSurvivesEmptyLog(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, epochTestOpts(dir))
	appendN(t, c, 0, 50)
	cur, _ := c.WatchCursor()
	c.Truncate()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	snaps, _ := filepath.Glob(filepath.Join(dir, "*", dirSnapName))
	for _, p := range snaps {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	c = mustOpen(t, epochTestOpts(dir))
	defer c.Close()
	appendN(t, c, 100, 160) // more than the cursor's seq: would reach past it if reissued
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ns, reset, _ := drainCatchup(t, mustWatch(t, c, ctx, cur))
	if !reset {
		t.Fatal("cursor below the post-truncate floor must Reset")
	}
	wantRange(t, "after truncate+reopen", ns, 100, 160)
}

// TestArchiveWatchRotatedCursorResetsAfterReopen: the rotation-gap Reset still applies after a
// clean reopen, while a cursor at/above the floor resumes.
func TestArchiveWatchRotatedCursorResetsAfterReopen(t *testing.T) {
	dir := t.TempDir()
	opts := epochTestOpts(dir)
	opts.Retention = Retention{MaxSegments: 2}
	c := mustOpen(t, opts)
	appendN(t, c, 0, 20)
	early, _ := c.WatchCursor()
	appendN(t, c, 20, 400)
	if dropped, err := c.Rotate(0); err != nil || dropped == 0 {
		t.Fatalf("Rotate dropped %d (err %v), want > 0", dropped, err)
	}
	fresh, _ := c.WatchCursor()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = mustOpen(t, opts)
	defer c.Close()
	appendN(t, c, 400, 410)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ns, reset, _ := drainCatchup(t, mustWatch(t, c, ctx, early))
	if !reset {
		t.Fatal("cursor below the append floor must Reset after reopen")
	}
	if len(ns) == 0 || ns[0] == 0 || ns[len(ns)-1] != 409 {
		t.Fatalf("reset replay should run from the floor to 409, got %v", ns)
	}
	ns, reset, _ = drainCatchup(t, mustWatch(t, c, ctx, fresh))
	if reset {
		t.Fatal("cursor at the head before Close must resume after reopen")
	}
	wantRange(t, "fresh cursor", ns, 400, 410)
}

// TestWatchCursorAheadOfHeadResets: an older copy of a cleanly closed store has the same epoch
// but a shorter log; a cursor from the newer copy is ahead of its head and must Reset.
func TestWatchCursorAheadOfHeadResets(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, epochTestOpts(dir))
	appendN(t, c, 0, 50)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	backup := copyTree(t, dir)
	c = mustOpen(t, epochTestOpts(dir))
	appendN(t, c, 50, 100)
	cur, _ := c.WatchCursor()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c = mustOpen(t, epochTestOpts(backup))
	defer c.Close()
	if got, _ := c.WatchCursor(); cursorEpoch(t, got) != cursorEpoch(t, cur) {
		t.Fatal("test premise: the copy should carry the same epoch")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ns, reset, _ := drainCatchup(t, mustWatch(t, c, ctx, cur))
	if !reset {
		t.Fatal("a cursor ahead of the head must Reset")
	}
	wantRange(t, "reset replay", ns, 0, 50)
}

// TestMutableWatchResetsAcrossReopen: mutable collections keep a per-process epoch (their
// delete journal is not persisted), so a restart still Resets, and no marker is written.
func TestMutableWatchResetsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	opts := Options{Dir: dir, WatchHistory: 4096}
	c := mustOpen(t, opts)
	for i := 0; i < 20; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N = %d ]`, i))
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	cur, _ := c.WatchCursor()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, watchEpochFile)); !os.IsNotExist(err) {
		t.Fatalf("a mutable collection must not write watch.epoch (stat err %v)", err)
	}
	c = mustOpen(t, opts)
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ns, reset, _ := drainCatchup(t, mustWatch(t, c, ctx, cur))
	if !reset {
		t.Fatal("a mutable collection must Reset across a reopen")
	}
	if len(ns) != 20 {
		t.Fatalf("reset replay yielded %d ads, want 20", len(ns))
	}
}

func TestWatchEpochMarkerRoundTrip(t *testing.T) {
	m := watchEpochMark{epoch: 0xdeadbeef, seqs: []uint64{1, 2, 1 << 40}}
	got, ok := decodeWatchEpoch(encodeWatchEpoch(m))
	if !ok || got.epoch != m.epoch || len(got.seqs) != 3 || got.seqs[2] != 1<<40 {
		t.Fatalf("round trip: %+v ok=%v", got, ok)
	}
	b := encodeWatchEpoch(watchEpochMark{epoch: 0, seqs: []uint64{1}})
	if _, ok := decodeWatchEpoch(b); ok {
		t.Fatal("epoch 0 is reserved and must not decode")
	}
}
