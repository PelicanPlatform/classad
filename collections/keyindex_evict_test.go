package collections

import (
	"fmt"
	"testing"
)

// residentDir sums the resident directory entries across all shards (the RAM the
// pageable primary index bounds).
func residentDir(c *Collection) int {
	n := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		n += len(sh.dir)
		sh.mu.RUnlock()
	}
	return n
}

// TestOperationTimeEviction proves phase-3 eviction happens DURING operation, not only at
// reopen: after writing enough keys to fill many sealed segments (so the directory holds
// them all), a plain Reindex seals each sealed segment's key sidecar and evicts its keys,
// dropping the resident directory to ~the active segment while Len and every Get stay
// intact. Without operation-time eviction the directory would stay O(all keys) until Close.
func TestOperationTimeEviction(t *testing.T) {
	const n = 3000
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 4, SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("c%05d", i)), mustAd(t, `[ N = 0 ]`)); err != nil {
			t.Fatal(err)
		}
	}
	// Before eviction the directory holds (nearly) every key -- sealing/eviction has not
	// run yet during this uninterrupted write burst.
	if before := residentDir(c); before < n/2 {
		t.Fatalf("directory holds %d entries before Reindex; expected close to %d (no eviction yet)", before, n)
	}

	c.Reindex() // the operation-time seal + evict pass

	after := residentDir(c)
	if after > n/4 {
		t.Fatalf("directory holds %d entries after Reindex; expected a small active-segment fraction", after)
	}
	if c.Len() != n {
		t.Fatalf("Len = %d, want %d", c.Len(), n)
	}
	for i := 0; i < n; i++ {
		key := []byte(fmt.Sprintf("c%05d", i))
		if _, ok := c.Get(key); !ok {
			t.Fatalf("key %q missing after operation-time eviction", key)
		}
	}
	t.Logf("operation-time eviction: directory %d -> %d entries for %d live keys (%.0f%%)",
		n, after, n, 100*float64(after)/float64(n))

	// Roundtrip: a clean Close now snapshots an already-partial directory (dir.snap stores
	// the live count separately from the resident entries), and the reopen must restore the
	// full store -- every key present, count intact -- served by the sealed probe.
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c2, err := Open(Options{Dir: dir, Shards: 4, SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if c2.Len() != n {
		t.Fatalf("after reopen Len = %d, want %d", c2.Len(), n)
	}
	for i := 0; i < n; i++ {
		if _, ok := c2.Get([]byte(fmt.Sprintf("c%05d", i))); !ok {
			t.Fatalf("key c%05d missing after reopen of an evicted store", i)
		}
	}
}

// TestCompactionEviction proves compaction re-bounds the resident directory. Compaction
// rebuilds the full directory as it installs fresh segments; the reindex-after-compaction
// pass must then seal those segments and evict their keys, so a long-running process that
// compacts does not leak the directory back to O(all keys). Writes create heavy garbage
// (each key overwritten many times) so compaction actually fires.
func TestCompactionEviction(t *testing.T) {
	const n = 2000
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 4, SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Write each key several times: the superseded old versions are the garbage that makes
	// shouldCompact fire, and give the destination segments something to seal.
	for pass := 0; pass < 6; pass++ {
		for i := 0; i < n; i++ {
			if err := c.Put([]byte(fmt.Sprintf("c%05d", i)), mustAd(t, fmt.Sprintf(`[ N = %d ]`, pass))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := c.Compact(); got == 0 {
		t.Fatal("no shard compacted; test did not exercise the compaction-eviction path")
	}

	after := residentDir(c)
	if after > n/4 {
		t.Fatalf("directory holds %d entries after compaction; expected a small active-segment fraction (compaction re-leaked the directory)", after)
	}
	if c.Len() != n {
		t.Fatalf("Len = %d, want %d", c.Len(), n)
	}
	for i := 0; i < n; i++ {
		ad, ok := c.Get([]byte(fmt.Sprintf("c%05d", i)))
		if !ok {
			t.Fatalf("key c%05d missing after compaction eviction", i)
		}
		if v, _ := ad.EvaluateAttrInt("N"); v != 5 { // last pass wrote N=5
			t.Fatalf("key c%05d = %d, want 5", i, v)
		}
	}
	t.Logf("compaction eviction: directory holds %d entries for %d live keys (%.0f%%)",
		after, n, 100*float64(after)/float64(n))
}
