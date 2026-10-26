package collections

import (
	"fmt"
	"testing"
)

// TestFindCurrentDanglingLocNoPanic reproduces the reported crash shape: a directory/chain
// location that references a segment index past the live set (a stale link left by a reaped
// segment). The walk must treat it as end-of-chain, not index out of range.
func TestFindCurrentDanglingLocNoPanic(t *testing.T) {
	sh := &shard{} // no live segments: any seg index is out of range
	if _, ok := sh.findCurrent(loc{seg: 44, off: 0x2ce}, []byte("16257.1830")); ok {
		t.Fatal("a dangling head loc must yield not-found, not a match")
	}
}

// TestSpliceDeadFromChainDanglingNoPanic guards the compaction chain-splice against the same
// dangling location.
func TestSpliceDeadFromChainDanglingNoPanic(t *testing.T) {
	sh := &shard{dir: map[uint64]loc{7: {seg: 44, off: 16}}}
	// deadSet marks a live-set index; the head at seg 44 is simply nonexistent.
	sh.spliceDeadFromChain(7, map[uint32]struct{}{0: {}}, fnvHasher{})
	// And with the dangling segment itself marked dead (the leading-loop path).
	sh.dir[7] = loc{seg: 44, off: 16}
	sh.spliceDeadFromChain(7, map[uint32]struct{}{44: {}}, fnvHasher{})
	if _, ok := sh.dir[7]; ok {
		t.Errorf("dir entry pointing only into a dead/absent segment should be dropped")
	}
}

// TestReclaimSpliceIgnoresForeignHead: a directory head that names some other key's record -- what a
// head left stale by a segment rewrite can land on -- is not followed or written through by the
// dead-segment splice; it is dropped. The walk used to rewrite the next link of whatever record (or
// bytes) the head named.
func TestReclaimSpliceIgnoresForeignHead(t *testing.T) {
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	put := func(i, gen int) {
		t.Helper()
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[ K=%d; Gen=%d; Pad="%080d" ]`, i, gen, i))); err != nil {
			t.Fatal(err)
		}
	}
	for gen := range 2 {
		for i := range 60 {
			put(i, gen)
		}
	}
	sh := c.shards[0]
	if !sh.hasReclaimableSegment() {
		t.Fatal("fixture: no fully dead segment")
	}
	// k0's first version is in a dead segment, so the reclaim walks k0's bucket. Point that bucket's
	// head at k1's current record.
	h0 := c.h.Hash([]byte("k0"))
	h1 := c.h.Hash([]byte("k1"))
	sh.mu.Lock()
	foreign, ok := sh.findCurrent(sh.dirGet(h1), []byte("k1"))
	if !ok {
		sh.mu.Unlock()
		t.Fatal("fixture: k1 has no current record in the directory")
	}
	sh.dir[h0] = foreign
	sh.mu.Unlock()
	if c.reclaimDeadShard(sh) == 0 {
		t.Fatal("fixture: nothing reclaimed")
	}
	sh.mu.RLock()
	l, kept := sh.dir[h0]
	sh.mu.RUnlock()
	if kept && l == foreign {
		t.Fatal("the splice kept a head that names another key's record")
	}
	for i := range 60 {
		ad, ok := c.Get([]byte(fmt.Sprintf("k%d", i)))
		if !ok {
			t.Fatalf("k%d lost", i)
		}
		if gen, _ := ad.EvaluateAttrInt("Gen"); gen != 1 {
			t.Fatalf("k%d Gen=%d", i, gen)
		}
	}
}
