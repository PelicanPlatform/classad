package collections

import "testing"

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
	sh.spliceDeadFromChain(7, map[uint32]struct{}{0: {}})
	// And with the dangling segment itself marked dead (the leading-loop path).
	sh.dir[7] = loc{seg: 44, off: 16}
	sh.spliceDeadFromChain(7, map[uint32]struct{}{44: {}})
	if _, ok := sh.dir[7]; ok {
		t.Errorf("dir entry pointing only into a dead/absent segment should be dropped")
	}
}
