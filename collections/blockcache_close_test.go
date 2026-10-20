package collections

import (
	"fmt"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// closeTestArchive opens a persistent archive with n records and builds its columnar accelerator,
// so a full Scan reconstructs every ad from columnar blocks through the shared archive cache.
func closeTestArchive(t *testing.T, n int) *Archive {
	t.Helper()
	a, err := OpenArchive(ArchiveOptions{Dir: t.TempDir(), SegmentSize: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		ad, err := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nOwner = \"user%d\"\nJobStatus = %d\nCmd = \"/bin/sleep\"",
			i, i%7, i%6))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	a.Reindex()
	if !a.BuildAndEnableSchemaScan(4000, 4) {
		t.Fatal("the archive did not build its columnar accelerator")
	}
	return a
}

// requireCacheServes checks that bc is a working cache: a region read through it is retained, so a
// second lookup hits. A closed ristretto cache drops every Set and misses every Get.
func requireCacheServes(t *testing.T, label string, bc *blockCache) {
	t.Helper()
	if bc == nil {
		t.Fatalf("%s: no shared cache", label)
	}
	codec, err := NewZSTDCodec(nil)
	if err != nil {
		t.Fatal(err)
	}
	blk := &columnarBlock{id: colBlockSeq.Add(1), codec: codec, coldComp: codec.Compress(nil, []byte("probe region"))}
	if _, err := bc.stream(blk, kindCold); err != nil {
		t.Fatal(err)
	}
	bc.c.Wait()
	if _, ok := bc.c.Get(streamKey(blk.id, kindCold)); !ok {
		t.Fatalf("%s: a region read through the shared cache was not retained; the cache is closed", label)
	}
}

// freshArchiveCache points the archive slot at a new cache (created on next use) and returns the
// slot's previous cache, so each iteration below starts with an open cache that nothing else holds.
func freshArchiveCache() *blockCache {
	slot := &globalBlockCaches[archiveCacheKind]
	slot.mu.Lock()
	defer slot.mu.Unlock()
	old := slot.bc
	slot.bc, slot.made = nil, false
	return old
}

// TestCloseKeepsSharedBlockCacheOpen: closing one collection must not close the process-wide block
// cache that every other collection of its kind reads through. Archive A is closed while archive B
// scans its columnar blocks; B's reads must not panic, B must still be served by an open cache
// afterwards, and an archive opened after the close must work too. The budget is shrunk below one
// region during the race so every read misses and inserts, which is when ristretto sends on the
// channel that Close closes.
//
// Not parallel: each iteration installs a fresh global archive cache, which must not be swapped out
// from under a concurrently running test.
func TestCloseKeepsSharedBlockCacheOpen(t *testing.T) {
	defer snapshotBudget(t, archiveCacheKind)()
	orig := freshArchiveCache()
	defer func() {
		slot := &globalBlockCaches[archiveCacheKind]
		slot.mu.Lock()
		slot.bc, slot.made = orig, true
		slot.mu.Unlock()
	}()

	iters := 10
	if testing.Short() {
		iters = 3
	}
	for it := range iters {
		freshArchiveCache()
		SetArchiveBlockCacheBudget(4 << 10)
		a := closeTestArchive(t, 400)
		b := closeTestArchive(t, 400)
		shared := b.c.sharedColCache()
		if a.c.sharedColCache() != shared {
			t.Fatal("two archives do not share one block cache")
		}
		want := len(scanStrings(b.c))
		if want != 400 {
			t.Fatalf("scan returned %d ads, want 400", want)
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan string, 8)
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for range 3 {
					if n := len(scanStrings(b.c)); n != want {
						errs <- fmt.Sprintf("survivor scan returned %d ads, want %d", n, want)
						return
					}
				}
			}()
		}
		close(start)
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Fatalf("iteration %d: %s", it, e)
		}

		SetArchiveBlockCacheBudget(64 << 20)
		requireCacheServes(t, fmt.Sprintf("iteration %d survivor", it), b.c.sharedColCache())

		c := closeTestArchive(t, 200)
		if c.c.sharedColCache() != shared {
			t.Fatal("an archive opened after the close did not join the shared cache")
		}
		if n := len(scanStrings(c.c)); n != 200 {
			t.Fatalf("iteration %d: archive opened after the close scanned %d ads, want 200", it, n)
		}
		requireCacheServes(t, fmt.Sprintf("iteration %d later archive", it), c.c.sharedColCache())

		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		// This iteration's cache is unreachable now that every archive that held it is closed.
		shared.c.Close()
	}
}
