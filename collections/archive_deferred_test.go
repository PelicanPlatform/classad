//go:build unix

package collections

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/PelicanPlatform/classad/classad"
)

// Deferred archive appends (Archive.AppendDeferred + Flush). The contract under test:
//
//   - everything appended before a Flush that returned nil survives a machine crash;
//   - what survives is a prefix of the appends, so a resume position persisted after a
//     Flush is never ahead of the archive;
//   - a watcher is never handed an ad a crash could take back, live or in catch-up;
//   - a segment is never sealed holding deferred bytes;
//   - the result is the same archive Append would have built.
//
// A process crash leaves unsynced mmap writes in the page cache, so it loses nothing. These
// tests model the worse case, a machine crash: crashImage copies the archive directory and
// zeroes every segment byte past what an msync has put on disk.

// durableTracker records, per segment file, how far msync has made it durable.
type durableTracker struct {
	mu sync.Mutex
	to map[string]int
}

// trackDurable installs the msync seam for the test's lifetime. Tests using it must not
// run in parallel: the seam is process-global.
func trackDurable(t *testing.T) *durableTracker {
	t.Helper()
	d := &durableTracker{to: map[string]int{}}
	f := func(path string, to int) {
		d.mu.Lock()
		if to > d.to[path] {
			d.to[path] = to
		}
		d.mu.Unlock()
	}
	msyncDoneHook.Store(&f)
	t.Cleanup(func() { msyncDoneHook.Store(nil) })
	return d
}

// crashImage copies a's directory to a new one as a machine crash would leave it: every
// live segment file keeps only the bytes an msync covered, the rest reads as zeros. Other
// files (sidecars, config) are copied as they are.
func crashImage(t *testing.T, a *Archive, d *durableTracker) string {
	t.Helper()
	durable := map[string]int{}
	d.mu.Lock()
	for _, sh := range a.c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil && seg.path != "" {
				durable[seg.path] = d.to[seg.path]
			}
		}
		sh.mu.RUnlock()
	}
	d.mu.Unlock()
	dst := t.TempDir()
	err := filepath.WalkDir(a.dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(a.dir, path)
		out := filepath.Join(dst, rel)
		if e.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // reaped while walking
			}
			return err
		}
		if n, ok := durable[path]; ok {
			clear(b[min(n, len(b)):])
		}
		return os.WriteFile(out, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func deferredOpts(dir string) ArchiveOptions {
	return ArchiveOptions{
		Dir:         dir,
		SegmentSize: 8 << 10, // many seals, so sealing, sidecars and zone maps are exercised
		ValueAttrs:  []string{"ID"},
		ZoneAttrs:   []string{"CompletionDate"},
	}
}

func deferredAd(t *testing.T, i int) *classad.ClassAd {
	t.Helper()
	return mustAd(t, fmt.Sprintf(`[ID=%d; Owner="user%d"; CompletionDate=%d; Pad="%0120d"]`, i, i%7, 1_700_000_000+i, i))
}

// deferredIDs returns every ID in the archive, ascending.
func deferredIDs(t *testing.T, a *Archive) []int {
	t.Helper()
	return archiveQueryIDs(t, a, "true")
}

// requirePrefix asserts ids is exactly 0..len(ids)-1.
func requirePrefix(t *testing.T, what string, ids []int) {
	t.Helper()
	for i, id := range ids {
		if id != i {
			t.Fatalf("%s: surviving IDs are not a prefix of the appends: position %d holds ID %d (%d survived)", what, i, id, len(ids))
		}
	}
}

// TestArchiveDeferredCrashKeepsFlushedPrefix runs a history-tailer loop -- AppendDeferred
// each record, Flush and then persist the resume position every chunk -- and crashes it at
// many points: mid-chunk (appended, not flushed), right after a Flush, and right after the
// position is persisted. At every crash the recovered archive must be a prefix of the
// appends that reaches at least the persisted position, and resuming from that position
// must rebuild exactly the source.
func TestArchiveDeferredCrashKeepsFlushedPrefix(t *testing.T) {
	d := trackDurable(t)
	a, err := CreateArchive(deferredOpts(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	const n, chunk = 240, 25
	cursor := 0 // the persisted resume position: source records [0, cursor) are archived
	check := func(what string, appended int) {
		t.Helper()
		img := crashImage(t, a, d)
		r, err := OpenArchive(deferredOpts(img))
		if err != nil {
			t.Fatalf("%s: reopening the crash image: %v", what, err)
		}
		defer r.Close()
		ids := deferredIDs(t, r)
		requirePrefix(t, what, ids)
		if len(ids) < cursor {
			t.Fatalf("%s: %d records survived but the persisted position is %d: the position got ahead of the archive", what, len(ids), cursor)
		}
		if len(ids) > appended {
			t.Fatalf("%s: %d records survived, only %d were appended", what, len(ids), appended)
		}
		if r.Count() != len(ids) {
			t.Fatalf("%s: Count %d, query returned %d", what, r.Count(), len(ids))
		}
		// Resume as the tailer would: from the persisted position, skipping what the archive
		// already holds. The rebuilt archive must be exactly the source.
		for i := cursor; i < n; i++ {
			if i < len(ids) {
				continue
			}
			if err := r.AppendDeferred(deferredAd(t, i)); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.Flush(); err != nil {
			t.Fatal(err)
		}
		got := deferredIDs(t, r)
		requirePrefix(t, what+" (resumed)", got)
		if len(got) != n {
			t.Fatalf("%s: resumed archive holds %d records, want %d", what, len(got), n)
		}
	}

	for i := 0; i < n; i++ {
		if err := a.AppendDeferred(deferredAd(t, i)); err != nil {
			t.Fatal(err)
		}
		if i%11 == 5 {
			check(fmt.Sprintf("crash after deferred append %d", i), i+1)
		}
		if (i+1)%chunk == 0 {
			if err := a.Flush(); err != nil {
				t.Fatal(err)
			}
			if i%(2*chunk) == chunk-1 {
				check(fmt.Sprintf("crash after Flush at %d", i+1), i+1)
			}
			cursor = i + 1
			check(fmt.Sprintf("crash after persisting position %d", cursor), i+1)
		}
	}
}

// TestArchiveDeferredSealsOnlySyncedSegments checks that no segment leaves the write path
// holding deferred bytes: every sealed segment is fully on disk the moment it seals.
func TestArchiveDeferredSealsOnlySyncedSegments(t *testing.T) {
	d := trackDurable(t)
	a, err := CreateArchive(deferredOpts(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for i := 0; i < 300; i++ {
		if err := a.AppendDeferred(deferredAd(t, i)); err != nil {
			t.Fatal(err)
		}
	}
	sh := a.c.shards[0]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	sealed := 0
	for _, seg := range sh.segs {
		if seg == nil || seg == sh.act {
			continue
		}
		sealed++
		if d.to[seg.path] < seg.used {
			t.Errorf("segment %d sealed with %d of %d bytes on disk", seg.id, d.to[seg.path], seg.used)
		}
	}
	if sealed < 3 {
		t.Fatalf("only %d sealed segments; the test needs several seals", sealed)
	}
}

// watchCollector runs a Watch in the background and records its upserts.
type watchCollector struct {
	mu      sync.Mutex
	ids     []int
	resyncs int
	synced  chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
}

func startWatch(t *testing.T, a *Archive) *watchCollector {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	seq, err := a.Watch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := &watchCollector{synced: make(chan struct{}), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		for ev := range seq {
			switch ev.Kind {
			case WatchUpsert:
				id, _ := ev.Ad.EvaluateAttrInt("ID")
				w.mu.Lock()
				w.ids = append(w.ids, int(id))
				w.mu.Unlock()
			case WatchSynced:
				close(w.synced)
			case WatchResync:
				w.mu.Lock()
				w.resyncs++
				w.mu.Unlock()
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-w.done })
	select {
	case <-w.synced:
	case <-time.After(10 * time.Second):
		t.Fatal("watch never reached Synced")
	}
	return w
}

func (w *watchCollector) seen() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]int(nil), w.ids...)
}

// waitSeen waits until the watcher has seen at least n upserts, or the timeout passes.
func (w *watchCollector) waitSeen(n int, timeout time.Duration) []int {
	deadline := time.Now().Add(timeout)
	for {
		s := w.seen()
		if len(s) >= n || time.Now().After(deadline) {
			return s
		}
		time.Sleep(time.Millisecond)
	}
}

// requireSeenDurable asserts every ID the watcher has seen survives a crash taken now.
func requireSeenDurable(t *testing.T, what string, a *Archive, d *durableTracker, seen []int) {
	t.Helper()
	img := crashImage(t, a, d)
	r, err := OpenArchive(deferredOpts(img))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	have := map[int]bool{}
	for _, id := range deferredIDs(t, r) {
		have[id] = true
	}
	for _, id := range seen {
		if !have[id] {
			t.Fatalf("%s: a watcher saw ID %d, which a crash now would lose", what, id)
		}
	}
}

// TestArchiveDeferredWatchSeesOnlyDurable: a live watcher gets a deferred append's event
// only from the sync that makes it durable, so nothing it has seen can be lost to a crash;
// Flush delivers them all.
func TestArchiveDeferredWatchSeesOnlyDurable(t *testing.T) {
	d := trackDurable(t)
	a, err := CreateArchive(deferredOpts(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	w := startWatch(t, a)

	// Few enough to stay inside one segment, so no seal syncs them on the way.
	const m = 20
	for i := 0; i < m; i++ {
		if err := a.AppendDeferred(deferredAd(t, i)); err != nil {
			t.Fatal(err)
		}
	}
	// Give a premature publish every chance to arrive before looking.
	seen := w.waitSeen(1, 200*time.Millisecond)
	requireSeenDurable(t, "before Flush", a, d, seen)
	if len(seen) != 0 {
		t.Fatalf("watcher saw %d deferred appends before any sync made them durable", len(seen))
	}

	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	seen = w.waitSeen(m, 10*time.Second)
	if len(seen) != m {
		t.Fatalf("after Flush the watcher saw %d of %d appends", len(seen), m)
	}
	requireSeenDurable(t, "after Flush", a, d, seen)

	// A durable Append publishes the deferred appends its sync covers, ahead of its own.
	for i := m; i < 2*m; i++ {
		if err := a.AppendDeferred(deferredAd(t, i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Append(deferredAd(t, 2*m)); err != nil {
		t.Fatal(err)
	}
	seen = w.waitSeen(2*m+1, 10*time.Second)
	if len(seen) != 2*m+1 {
		t.Fatalf("after a durable Append the watcher saw %d of %d appends", len(seen), 2*m+1)
	}
	for i, id := range seen {
		if id != i {
			t.Fatalf("watcher saw appends out of order: position %d holds ID %d", i, id)
		}
	}
	requireSeenDurable(t, "after durable Append", a, d, seen)
}

// TestArchiveDeferredWatchBurstBounded: deferred appends never publish a burst a watcher's
// buffer cannot hold, however long the caller goes between Flushes.
func TestArchiveDeferredWatchBurstBounded(t *testing.T) {
	opts := deferredOpts(t.TempDir())
	opts.SegmentSize = 4 << 20 // no seal, so only the cap syncs before the Flush
	a, err := CreateArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	w := startWatch(t, a)
	const m = 3000 // several watch buffers' worth
	for i := 0; i < m; i++ {
		if aerr := a.AppendDeferred(deferredAd(t, i)); aerr != nil {
			t.Fatal(aerr)
		}
	}
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	seen := w.waitSeen(m, 10*time.Second)
	w.mu.Lock()
	resyncs := w.resyncs
	w.mu.Unlock()
	if resyncs != 0 {
		t.Fatalf("the watcher was forced to resync (it saw %d of %d appends live)", len(seen), m)
	}
	if len(seen) != m {
		t.Fatalf("the watcher saw %d of %d appends", len(seen), m)
	}
}

// TestArchiveDeferredWatchCatchupIsDurable: a watcher that subscribes while deferred
// appends are outstanding reads them in catch-up, so the subscription syncs first.
func TestArchiveDeferredWatchCatchupIsDurable(t *testing.T) {
	d := trackDurable(t)
	a, err := CreateArchive(deferredOpts(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	const m = 20
	for i := 0; i < m; i++ {
		if aerr := a.AppendDeferred(deferredAd(t, i)); aerr != nil {
			t.Fatal(aerr)
		}
	}
	w := startWatch(t, a) // returns after catch-up
	seen := w.seen()
	if len(seen) != m {
		t.Fatalf("catch-up delivered %d of %d appends", len(seen), m)
	}
	requireSeenDurable(t, "catch-up", a, d, seen)

	// WatchCursor names only durable seqs.
	for i := m; i < 2*m; i++ {
		if aerr := a.AppendDeferred(deferredAd(t, i)); aerr != nil {
			t.Fatal(aerr)
		}
	}
	if _, cerr := a.WatchCursor(); cerr != nil {
		t.Fatal(cerr)
	}
	img := crashImage(t, a, d)
	r, err := OpenArchive(deferredOpts(img))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := r.Count(); got != 2*m {
		t.Fatalf("after WatchCursor a crash keeps %d of %d appends", got, 2*m)
	}
}

// TestArchiveDeferredMatchesDurable: the same ads appended with Append and with
// AppendDeferred (Flushing at irregular points) give the same archive, live and reopened.
func TestArchiveDeferredMatchesDurable(t *testing.T) {
	t.Parallel()
	const n = 400
	ads := make([]*classad.ClassAd, n)
	for i := range ads {
		ads[i] = deferredAd(t, i)
	}
	build := func(deferred bool) (*Archive, ArchiveOptions) {
		opts := deferredOpts(t.TempDir())
		a, err := CreateArchive(opts)
		if err != nil {
			t.Fatal(err)
		}
		for i, ad := range ads {
			if deferred {
				err = a.AppendDeferred(ad)
				if err == nil && i%37 == 0 {
					err = a.Flush()
				}
			} else {
				err = a.Append(ad)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		return a, opts
	}
	contents := func(a *Archive) []string {
		var out []string
		for ad := range a.Query(mustParseQuery(t, "true")) {
			out = append(out, ad.String())
		}
		return out
	}
	da, dopts := build(false)
	fa, fopts := build(true)
	if err := fa.Flush(); err != nil {
		t.Fatal(err)
	}
	want, got := contents(da), contents(fa)
	if fmt.Sprint(want) != fmt.Sprint(got) {
		t.Fatalf("deferred archive differs from durable one: %d vs %d records", len(got), len(want))
	}
	if ids := archiveQueryIDs(t, fa, "ID >= 100 && ID < 150"); len(ids) != 50 {
		t.Fatalf("indexed range query on the deferred archive returned %d records, want 50", len(ids))
	}
	if err := da.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fa.Close(); err != nil {
		t.Fatal(err)
	}
	dr, err := OpenArchive(dopts)
	if err != nil {
		t.Fatal(err)
	}
	defer dr.Close()
	fr, err := OpenArchive(fopts)
	if err != nil {
		t.Fatal(err)
	}
	defer fr.Close()
	if fmt.Sprint(contents(dr)) != fmt.Sprint(contents(fr)) {
		t.Fatal("reopened deferred archive differs from the reopened durable one")
	}
	if fr.Count() != n {
		t.Fatalf("reopened deferred archive holds %d records, want %d", fr.Count(), n)
	}
}

// TestArchiveDeferredTruncateReleasesCursor: Truncate drops held deferred events and
// retires their seqs, so they cannot hold a later live cursor back.
func TestArchiveDeferredTruncateReleasesCursor(t *testing.T) {
	t.Parallel()
	a, err := CreateArchive(deferredOpts(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	_ = startWatch(t, a)
	for i := 0; i < 10; i++ {
		if err := a.AppendDeferred(deferredAd(t, i)); err != nil {
			t.Fatal(err)
		}
	}
	sh := a.c.shards[0]
	if sh.ndeferred.Load() == 0 {
		t.Fatal("no deferred events held with a watcher attached")
	}
	a.Truncate()
	if n := sh.ndeferred.Load(); n != 0 {
		t.Fatalf("%d deferred events held after Truncate", n)
	}
	sh.pub.mu.Lock()
	pending := len(sh.pub.pending)
	sh.pub.mu.Unlock()
	if pending != 0 {
		t.Fatalf("%d seqs still pending after Truncate", pending)
	}
	if err := a.Append(deferredAd(t, 0)); err != nil {
		t.Fatal(err)
	}
	if a.Count() != 1 {
		t.Fatalf("Count %d after Truncate and one Append, want 1", a.Count())
	}
}

// TestArchiveDeferredSyncErrorIsSticky: a failed sync makes Flush fail and keeps the
// deferred appends from watchers, and later appends report it too.
func TestArchiveDeferredSyncErrorIsSticky(t *testing.T) {
	a, err := CreateArchive(deferredOpts(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	w := startWatch(t, a)
	for i := 0; i < 5; i++ {
		if aerr := a.AppendDeferred(deferredAd(t, i)); aerr != nil {
			t.Fatal(aerr)
		}
	}
	msyncFailHook = func() error { return unix.EIO }
	err = a.Flush()
	msyncFailHook = nil
	if !errors.Is(err, unix.EIO) {
		t.Fatalf("Flush returned %v although the sync failed", err)
	}
	if s := w.waitSeen(1, 200*time.Millisecond); len(s) != 0 {
		t.Fatalf("watcher saw %d appends whose sync failed", len(s))
	}
	if err := a.AppendDeferred(deferredAd(t, 5)); err == nil {
		t.Fatal("AppendDeferred succeeded after a sync failure")
	}
	if err := a.Flush(); err == nil {
		t.Fatal("Flush succeeded after a sync failure")
	}
	// Like Put, an append that reports the sticky error has still been applied.
	if ids := deferredIDs(t, a); len(ids) != 6 {
		t.Fatalf("%d appends readable after the sync failure, want 6", len(ids))
	}
}

// TestMsyncRangeUnalignedStart: an msync of a range starting mid-page must succeed. With
// the start aligned to 4 KiB rather than the real page size, a 16 KiB-page system (Apple
// silicon) rejected it with EINVAL, which was ignored, so the range was never synced.
func TestMsyncRangeUnalignedStart(t *testing.T) {
	t.Parallel()
	seg, err := newMmapSegment(0, 1<<16, identityCodec{}, filepath.Join(t.TempDir(), "seg.dat"))
	if err != nil {
		t.Fatal(err)
	}
	defer seg.reap()
	for seg.used < 3*4096+100 {
		if _, ok := seg.appendFlagged(1, noLoc, nil, make([]byte, 300), 0); !ok {
			t.Fatal("segment full")
		}
	}
	for _, from := range []int{0, 4096 + 8, 2*4096 + 8, 3 * 4096} {
		if err := seg.msyncRange(from, seg.used); err != nil {
			t.Errorf("msync [%d, %d): %v", from, seg.used, err)
		}
	}
}
