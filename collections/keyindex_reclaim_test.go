package collections

import (
	"fmt"
	"testing"
)

// liveSegments counts the non-nil segments across all shards (files/mmaps/VMAs held).
func liveSegments(c *Collection) int {
	n := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil {
				n++
			}
		}
		sh.mu.RUnlock()
	}
	return n
}

// oneBucketHasher forces every key into a single directory bucket (and, with Shards:1, a
// single shard), so all records share one recNext chain. It makes a reclaimed segment sit
// mid-chain for live keys, exercising spliceDeadFromChain -- the case a per-key chain would
// never hit.
type oneBucketHasher struct{}

func (oneBucketHasher) Hash([]byte) uint64 { return 0x1234 }

// TestReclaimDeadSegments: overwriting every key leaves the early segments fully superseded;
// Compact must unlink them (fewer live segments) without a full rewrite, and every key must
// still resolve to its latest value.
func TestReclaimDeadSegments(t *testing.T) {
	const n = 800
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 4, SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%05d", i)), mustAd(t, `[ N = 1 ]`)); err != nil {
			t.Fatal(err)
		}
	}
	// Rewrite every key: the original versions (in the early segments) all become superseded,
	// so those segments go fully dead. The peak segment count is after the rewrite.
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%05d", i)), mustAd(t, `[ N = 2 ]`)); err != nil {
			t.Fatal(err)
		}
	}
	before := liveSegments(c)
	c.Compact()
	after := liveSegments(c)
	if after >= before {
		t.Fatalf("live segments %d -> %d; expected fully-dead early segments to be reclaimed", before, after)
	}
	if c.Len() != n {
		t.Fatalf("Len = %d, want %d", c.Len(), n)
	}
	for i := 0; i < n; i++ {
		ad, ok := c.Get([]byte(fmt.Sprintf("k%05d", i)))
		if !ok {
			t.Fatalf("key k%05d missing after reclaim", i)
		}
		if v, _ := ad.EvaluateAttrInt("N"); v != 2 {
			t.Fatalf("key k%05d = %d, want 2", i, v)
		}
	}
	t.Logf("reclaim: live segments %d -> %d for %d keys", before, after, after)
}

// TestReclaimChainSpliceForcedCollision is the correctness core: with every key in one
// shared chain, some keys are overwritten (their old versions form fully-dead segments that
// get reclaimed) while others stay put -- so the survivors' records sit on the far side of a
// reclaimed, spliced segment. Every survivor and every rewritten key must still resolve, and
// deleted keys must stay gone. A botched splice would dangle the chain or drop a live key.
func TestReclaimChainSpliceForcedCollision(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 512, Hasher: oneBucketHasher{}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	const n = 300
	// Interleave: even keys get many rewrites (churn -> dead segments mid-chain), odd keys
	// are written once and left (survivors reachable only through the churned region).
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%04d", i)), mustAd(t, `[ N = 0 ]`)); err != nil {
			t.Fatal(err)
		}
	}
	for round := 1; round <= 8; round++ {
		for i := 0; i < n; i += 2 { // rewrite only the even keys
			if err := c.Put([]byte(fmt.Sprintf("k%04d", i)), mustAd(t, fmt.Sprintf(`[ N = %d ]`, round))); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Delete a scattered handful so the absent-key path is exercised too.
	deleted := map[int]bool{6: true, 20: true, 150: true, 298: true}
	for i := range deleted {
		c.Delete([]byte(fmt.Sprintf("k%04d", i)))
	}

	before := liveSegments(c)
	c.Compact()
	after := liveSegments(c)
	if after >= before {
		t.Fatalf("no segments reclaimed (%d -> %d); the splice path was not exercised", before, after)
	}

	for i := 0; i < n; i++ {
		key := []byte(fmt.Sprintf("k%04d", i))
		ad, ok := c.Get(key)
		if deleted[i] {
			if ok {
				t.Fatalf("deleted key k%04d resurfaced after reclaim", i)
			}
			continue
		}
		if !ok {
			t.Fatalf("live key k%04d lost after chain splice", i)
		}
		want := int64(0)
		if i%2 == 0 {
			want = 8 // last rewrite round
		}
		if v, _ := ad.EvaluateAttrInt("N"); v != want {
			t.Fatalf("key k%04d = %d, want %d", i, v, want)
		}
	}
	wantLen := n - len(deleted)
	if c.Len() != wantLen {
		t.Fatalf("Len = %d, want %d", c.Len(), wantLen)
	}
	t.Logf("forced-collision splice: live segments %d -> %d, %d live keys intact", before, after, wantLen)
}

// TestReclaimMVCCConflictAfterGCFloor: a transaction reads a key at an old snapshot; the key
// is then deleted and its evidence-bearing segment reclaimed. The transaction's later commit
// must CONFLICT (the reclaim raised the GC floor), not silently succeed on the stale read --
// otherwise reclaim would resurrect a deleted key.
func TestReclaimMVCCConflictAfterGCFloor(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 512, Hasher: oneBucketHasher{}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	const target = "k0000"
	if err := c.Put([]byte(target), mustAd(t, `[ N = 7 ]`)); err != nil {
		t.Fatal(err)
	}
	// Old transaction pins its snapshot by reading the key while it is still live.
	tx := c.Begin()
	if v := txnGetInt(t, tx, target, "N"); v != 7 {
		t.Fatalf("snapshot read = %d, want 7", v)
	}

	// Delete the key (supersedes its only record), then churn other keys so the segment
	// holding the target's now-superseded record goes fully dead and is reclaimed.
	c.Delete([]byte(target))
	for round := 0; round < 12; round++ {
		for i := 1; i < 60; i++ {
			if err := c.Put([]byte(fmt.Sprintf("k%04d", i)), mustAd(t, fmt.Sprintf(`[ N = %d ]`, round))); err != nil {
				t.Fatal(err)
			}
		}
	}
	c.Compact()
	if _, ok := c.Get([]byte(target)); ok {
		t.Fatal("target should be deleted")
	}

	// The stale transaction now tries to write the key. Its snapshot predates the delete and
	// the reclaim, so the commit must be refused.
	tx.Put([]byte(target), mustAd(t, `[ N = 99 ]`))
	if r := tx.Commit(); !r.Conflicted() {
		t.Fatal("stale commit after reclaim should conflict (GC floor), but it succeeded")
	}
	if _, ok := c.Get([]byte(target)); ok {
		t.Fatal("conflicted commit must not have resurrected the deleted key")
	}
}
