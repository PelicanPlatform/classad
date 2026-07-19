package collections

import (
	"fmt"
	"sync"
	"testing"
)

// openEvicted opens a persistent collection, writes n counters, closes it, and
// reopens it -- so on reopen every counter is in a sealed segment and evicted from
// the directory (served by the sealed probe). It returns the reopened collection.
func openEvicted(t *testing.T, n int) *Collection {
	t.Helper()
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 4, SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("c%05d", i)), mustAd(t, `[ N = 0 ]`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c2, err := Open(Options{Dir: dir, Shards: 4, SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	return c2
}

// TestPhase3DirectoryEvicted proves the RAM win: after reopening a store whose keys
// are all in sealed segments, the resident directory holds only the active segment's
// keys (~0 on a fresh reopen), not all of them, while Len (the live count) is intact.
func TestPhase3DirectoryEvicted(t *testing.T) {
	const n = 3000 // enough keys for many segments per shard, so the active fraction is small
	c := openEvicted(t, n)
	defer c.Close()

	resident := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		resident += len(sh.dir)
		sh.mu.RUnlock()
	}
	if c.Len() != n {
		t.Fatalf("Len = %d, want %d", c.Len(), n)
	}
	// The directory is bounded by the active segment (a fixed size regardless of the
	// total key count) -- O(working set), not O(all keys). It must be a small fraction
	// of n; the rest are served by the sealed probe.
	if resident > n/4 {
		t.Fatalf("directory holds %d entries for %d live keys; expected a small active-segment fraction (eviction did not happen)", resident, n)
	}
	t.Logf("RAM win: directory holds %d entries for %d live keys (%.0f%%)", resident, n, 100*float64(resident)/float64(n))
}

// TestTxnPhase3EvictedWriteWriteConflict: a write-write conflict on a key that was
// evicted from the directory is still detected -- conflictSince finds the conflicting
// version through the sealed probe.
func TestTxnPhase3EvictedWriteWriteConflict(t *testing.T) {
	c := openEvicted(t, 200)
	defer c.Close()
	key := []byte("c00100")

	a := c.Begin()
	b := c.Begin()
	_ = txnGetInt(t, a, "c00100", "N") // reads the evicted key via the snapshot probe
	_ = txnGetInt(t, b, "c00100", "N")
	a.Put(key, mustAd(t, `[ N = 10 ]`))
	b.Put(key, mustAd(t, `[ N = 20 ]`))
	if r := a.Commit(); r.Conflicted() {
		t.Fatalf("first commit conflicted: %+v", r)
	}
	if r := b.Commit(); !r.Conflicted() || len(r.Conflicts) != 1 {
		t.Fatalf("second commit on an evicted key = %+v, want 1 conflict", r)
	}
	if ad, _ := c.Get(key); func() int64 { v, _ := ad.EvaluateAttrInt("N"); return v }() != 10 {
		t.Fatal("loser's write should not have applied")
	}
}

// TestTxnPhase3EvictedConcurrentIncrement is the OCC torture test for phase 3:
// concurrent read-modify-write increments to counters that start EVICTED must
// converge with no lost updates. A lost update would mean conflictSince missed a
// write-write conflict on an evicted key (the probe's correctness), and the counter
// would end below its increment count. Reads go through the snapshot probe (getAt).
func TestTxnPhase3EvictedConcurrentIncrement(t *testing.T) {
	const nCounters = 100
	c := openEvicted(t, nCounters)
	defer c.Close()

	const workers, perWorker = 8, 300
	want := make([]int64, nCounters)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed uint32) {
			defer wg.Done()
			r := seed | 1
			for i := 0; i < perWorker; i++ {
				r = r*1664525 + 1013904223 // deterministic LCG (reproducible, no shared rand)
				ci := int(r>>8) % nCounters
				ks := fmt.Sprintf("c%05d", ci)
				kb := []byte(ks)
				for { // retry until this increment commits
					tx := c.Begin()
					v := txnGetInt(t, tx, ks, "N")
					tx.Put(kb, mustAd(t, fmt.Sprintf(`[ N = %d ]`, v+1)))
					if !tx.Commit().Conflicted() {
						break
					}
				}
				mu.Lock()
				want[ci]++
				mu.Unlock()
			}
		}(uint32(w) + 1)
	}
	wg.Wait()

	for i := 0; i < nCounters; i++ {
		ad, ok := c.Get([]byte(fmt.Sprintf("c%05d", i)))
		if !ok {
			t.Fatalf("counter %d missing after concurrent increments", i)
		}
		got, _ := ad.EvaluateAttrInt("N")
		if got != want[i] {
			t.Fatalf("counter %d = %d, want %d -- lost update (conflictSince missed a conflict on an evicted key)", i, got, want[i])
		}
	}
}

// TestTxnPhase3EvictedSnapshotIsolation: a transaction that reads an evicted key sees
// its snapshot version even after another writer updates it -- the snapshot probe
// (getAt via lookupSealedAt) respects MVCC visibility.
func TestTxnPhase3EvictedSnapshotIsolation(t *testing.T) {
	c := openEvicted(t, 50)
	defer c.Close()
	key := []byte("c00010")

	tx := c.Begin()
	if v := txnGetInt(t, tx, "c00010", "N"); v != 0 { // pins the snapshot at the evicted version
		t.Fatalf("initial read = %d, want 0", v)
	}
	// A concurrent unconditional write bumps the key.
	if err := c.Put(key, mustAd(t, `[ N = 99 ]`)); err != nil {
		t.Fatal(err)
	}
	// The transaction still sees its snapshot (0), not the new value.
	if v := txnGetInt(t, tx, "c00010", "N"); v != 0 {
		t.Fatalf("snapshot read after concurrent write = %d, want 0", v)
	}
	// A fresh read sees the update.
	if ad, _ := c.Get(key); func() int64 { v, _ := ad.EvaluateAttrInt("N"); return v }() != 99 {
		t.Fatal("fresh Get should see the concurrent write")
	}
}
