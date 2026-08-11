package collections

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// A sealed segment's CONTENT is immutable; its MAPPING is not. Compact, a merge, a rotation and a
// reseal all retire a sealed segment and reap it -- munmap + unlink -- and they hold maintMu, not the
// shard lock. So a reader that captures a *segment under the shard read lock, releases the lock, and
// then reads seg.data is reading address space that may already be gone.
//
// That is a real production crash, not a theoretical one:
//
//	unexpected fault address 0x7fde6772b057
//	[signal SIGSEGV ...]
//	huff0.(*bitReaderShifted).init ... zstd.(*Decoder).DecodeAll
//	collections.buildColumnarFromSegment(...)   <- reading seg.data
//	collections.(*Collection).EnableSchemaScan
//	db.(*DB).Maintain                            <- admin RPC
//	  ... concurrently ...
//	collections.(*segment).reap -> unix.Munmap
//	collections.(*Collection).compactShard
//	db.(*DB).Compact                             <- background maintenance goroutine
//
// The fault address sits INSIDE a slice whose bounds are valid, which is why this reads as correct
// code. Three call sites carried a comment asserting sealed segments were safe to read off-lock.

// pinRaceFixture builds a persistent collection with several sealed segments and enough superseded
// records that a compaction actually rewrites and reaps them.
func pinRaceFixture(t *testing.T, n int) *Collection {
	t.Helper()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAdOld(t, fmt.Sprintf("ClusterId = %d\nProcId = %d\nMemory = %d\nOwner = \"u%d\"",
				i/10, i%10, 1024+(i%64)*512, i%32))); err != nil {
			t.Fatal(err)
		}
	}
	// Supersede every key so compaction has garbage to reclaim and will retire the originals.
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAdOld(t, fmt.Sprintf("ClusterId = %d\nProcId = %d\nMemory = %d\nOwner = \"v%d\"",
				i/10, i%10, 2048+(i%64)*512, i%32))); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// TestSchemaScanBuildSurvivesConcurrentCompaction reproduces the crash deterministically: the
// transcode is stalled mid-build (holding the segment) while a compaction runs to completion, and
// then the build reads the segment's bytes. Unpinned, that read is a use-after-munmap.
func TestSchemaScanBuildSurvivesConcurrentCompaction(t *testing.T) {
	c := pinRaceFixture(t, 4000)
	defer c.Close()

	// Fire once, and NON-REENTRANTLY. Compact itself reaches buildColSegment (via
	// reindexAfterCompaction -> Reindex -> sealAndEvictShard -> colBlobForSeg), so a hook that held
	// any lock while waiting for Compact would deadlock against its own nested call. The CAS is
	// claimed before the compaction starts, so the nested call returns immediately.
	var fired atomic.Bool
	colBuildStallHook = func() {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		// Compact takes maintMu, which the build does not hold, so it proceeds -- and retires and
		// reaps the very segments this build is walking.
		done := make(chan int, 1)
		go func() { done <- c.Compact() }()
		t.Logf("compaction reclaimed %d segment(s) while a transcode held one", <-done)
	}
	defer func() { colBuildStallHook = nil }()

	if !c.BuildAndEnableSchemaScan(2000, 4) {
		t.Skip("nothing to sample")
	}

	// The accelerator must still answer, and agree with the ordinary query path. Ground truth comes
	// from Query rather than allBruteCount: this collection is PERSISTENT, so its records store
	// attribute names inline and an interned-id lookup finds nothing.
	q, err := vm.Parse("Memory >= 2048")
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for range c.Query(q) {
		want++
	}
	got, served := c.CountConstraint("Memory >= 2048")
	if !served {
		t.Skip("columnar path declined; the build produced no usable block")
	}
	if got != want {
		t.Errorf("columnar count %d != row count %d after a build raced compaction", got, want)
	}
}

// TestReindexSurvivesConcurrentCompaction covers the same hole in Reindex, which decompresses every
// record it covers off-lock while holding only reindexMu -- which does not exclude Compact.
func TestReindexSurvivesConcurrentCompaction(t *testing.T) {
	c := pinRaceFixture(t, 4000)
	defer c.Close()
	if !c.AddIndex([]string{"Owner"}, nil) {
		t.Skip("AddIndex declined")
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.Reindex() }()
		wg.Add(1)
		go func() { defer wg.Done(); c.Compact() }()
	}
	wg.Wait()
	// Still queryable and consistent.
	q, err := vm.Parse("Owner =!= undefined")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range c.Query(q) {
		n++
	}
	if n == 0 {
		t.Error("no records returned after concurrent reindex + compaction")
	}
}
