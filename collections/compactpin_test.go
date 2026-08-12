package collections

import (
	"fmt"
	"sync"
	"testing"
)

// compactShard reads its source segments' bytes OFF the shard lock, and retires those same segments at the
// end. Without a pin, a reap -- its own or a concurrent one -- can munmap a mapping the copy loop is still
// reading, and the fault lands at an address INSIDE a slice whose bounds are perfectly valid:
//
//	unexpected fault address 0x7f0c492d6cb8
//	zstd.DecodeAll(..., src={0x7f0c492d6a3a, 0x2e6, 0x3295c6})
//	  compact.go:492  decodeSrc
//	  compact.go:563  compactShard
//
// So this asserts the pin, not the crash. Reproducing the race on demand needs a munmap to land inside a
// specific decompress, which no test can schedule; the invariant it rests on is checkable exactly.

// TestCompactPinsSourcesThenReleases holds the two halves of the invariant: while compaction is copying, its
// sources are pinned, and once it returns none of them is.
//
// The pin count is read under the shard lock, which is where refs is maintained.
func TestCompactPinsSourcesThenReleases(t *testing.T) {
	// PERSISTENT: pin/unpin are no-ops for a RAM collection, because there is no mapping to keep alive. The
	// bug only exists for mmap'd segments, so a RAM fixture asserts nothing -- which is how the first version
	// of this test reported "0 of 2 sources pinned" with the pin correctly in place.
	c := New(Options{Shards: 1, SegmentSize: 1 << 16, Dir: t.TempDir()})
	defer c.Close()
	// Churn every key many times: one overwrite is not enough to cross the compaction threshold, and a
	// fixture that does not compact makes every assertion below vacuous -- which is how the first version of
	// this test "passed".
	const n = 500
	for round := 0; round <= 20; round++ {
		for i := 0; i < n; i++ {
			src := fmt.Sprintf("ClusterId = %d\nProcId = %d\nOwner = \"user%d\"", i, round%10, i%8)
			if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, src)); err != nil {
				t.Fatal(err)
			}
		}
	}

	sh := c.shards[0]
	before := 0
	sh.mu.RLock()
	for _, seg := range sh.segs {
		if seg != nil {
			before += seg.refs
		}
	}
	sh.mu.RUnlock()
	if before != 0 {
		t.Fatalf("segments already pinned before compaction (%d); the balance check below would be blind", before)
	}

	// The load-bearing half: at the moment compaction starts reading, every source must be pinned.
	pinnedDuring, sourcesSeen, mappedSources := 0, 0, 0
	compactPhase2Hook = func(sources []*segment) {
		sh.mu.RLock()
		defer sh.mu.RUnlock()
		for _, seg := range sources {
			sourcesSeen++
			if !seg.mapped() {
				continue
			}
			mappedSources++
			if seg.refs > 0 {
				pinnedDuring++
			}
		}
	}
	defer func() { compactPhase2Hook = nil }()

	if compacted := c.Compact(); compacted == 0 {
		t.Fatal("no shard compacted after heavy churn; every assertion here would be vacuous")
	}

	if sourcesSeen == 0 {
		t.Fatal("compaction chose no sources; the fixture did not generate enough dead bytes to compact")
	}
	// pin is a no-op for a segment that is not MAPPED, because there is no mapping to keep alive -- so on a
	// platform or configuration that does not mmap, there is nothing here to assert and saying so is better
	// than reporting "0 of 2 pinned" against a pin that is correctly in place. The production fault was Linux
	// with file-backed segments; that is where this assertion has teeth.
	if mappedSources == 0 {
		t.Skipf("no source segment is mmap-backed here (%d sources), so pin/unpin are no-ops and the "+
			"invariant cannot be observed; this asserts on a platform that mmaps", sourcesSeen)
	}
	if pinnedDuring != mappedSources {
		t.Errorf("only %d of %d mmap-backed sources were pinned when compaction began reading them; an "+
			"unpinned source can be munmapped mid-read, which faults at an address inside a slice whose "+
			"bounds are perfectly valid", pinnedDuring, mappedSources)
	}

	sh.mu.RLock()
	after := 0
	for _, seg := range sh.segs {
		if seg != nil {
			after += seg.refs
		}
	}
	sh.mu.RUnlock()
	if after != 0 {
		t.Errorf("%d pins still held after Compact returned; unpin is what performs the deferred reap, so a "+
			"leaked pin keeps retired segments mapped forever", after)
	}

	// And the table must still be correct and readable afterwards -- a pin bug that kept a stale mapping
	// alive would not necessarily show up as a crash here, but a compaction that lost records would.
	got := 0
	for range c.Scan() {
		got++
	}
	if got != n {
		t.Errorf("after compaction: %d records, want %d", got, n)
	}
}

// TestCompactConcurrentWithScansAndReindex runs compaction against the readers and the reindex/eviction path
// that retire and reap segments, which is the combination the production fault came out of. Under -race it
// also catches unsynchronized access to the segment slice; without it, the value is that a reaped mapping
// faults hard rather than quietly.
func TestCompactConcurrentWithScansAndReindex(t *testing.T) {
	c := New(Options{Shards: 4, SegmentSize: 1 << 15, Dir: t.TempDir()})
	defer c.Close()
	for i := 0; i < 6000; i++ {
		src := fmt.Sprintf("ClusterId = %d\nProcId = %d\nOwner = \"user%d\"", i, i%10, i%8)
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for range c.Scan() { // reads segment bytes, taking its own pins
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 6; i++ {
			c.Reindex() // retires and reaps, which is the other half of the race
		}
	}()
	for i := 0; i < 6; i++ {
		for k := 0; k < 1500; k++ {
			src := fmt.Sprintf("ClusterId = %d\nProcId = %d\nOwner = \"user%d\"", k, (k+i)%10, k%8)
			if err := c.Put([]byte(fmt.Sprintf("k%d", k)), mustAdOld(t, src)); err != nil {
				t.Fatal(err)
			}
		}
		c.Compact()
	}
	close(stop)
	wg.Wait()

	n := 0
	for range c.Scan() {
		n++
	}
	if n != 6000 {
		t.Errorf("after concurrent compaction: %d records, want 6000", n)
	}
}
