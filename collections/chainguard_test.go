package collections

import (
	"fmt"
	"testing"
)

// A bucket-chain link can be GARBAGE rather than stale, and that took down a daemon:
//
//	panic: runtime error: index out of range [1765] with length 2
//
// A shard never held 1766 segments, so the link was not a once-valid index into a slice that shrank -- it was
// decoded out of bytes that are no longer a record header. Every walk now bounds-checks and counts instead of
// indexing blind.
//
// This is diagnostic, not a fix: it converts a crash into a counted anomaly with a fallback, so the process
// survives and the count says whether it is happening.
func TestCorruptChainLinkIsCountedNotFatal(t *testing.T) {
	c := New(Options{Shards: 1, SegmentSize: 1 << 16})
	defer c.Close()
	for i := 0; i < 200; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, fmt.Sprintf("ClusterId = %d", i))); err != nil {
			t.Fatal(err)
		}
	}
	sh := c.shards[0]
	before := CorruptChainLinks()

	// Point one bucket at a segment index no shard could have, which is what the production link looked like.
	sh.mu.Lock()
	var victim uint64
	for h := range sh.dir {
		victim = h
		break
	}
	if victim == 0 && len(sh.dir) == 0 {
		sh.mu.Unlock()
		t.Skip("directory is empty; nothing to corrupt")
	}
	sh.dir[victim] = loc{seg: 1765, off: 0}
	sh.mu.Unlock()

	// Every read path over that bucket must survive. Before the guard this panicked.
	for i := 0; i < 200; i++ {
		txn := c.Begin()
		txn.Get([]byte(fmt.Sprintf("k%d", i))) // must not panic; a miss is an acceptable outcome
	}
	n := 0
	for range c.Scan() {
		n++
	}
	if got := CorruptChainLinks(); got <= before {
		t.Errorf("the corrupt link was not counted (%d -> %d); an anomaly nobody counts is one nobody sees",
			before, got)
	}
	t.Logf("survived a garbage chain link; corrupt links counted: %d (scan still saw %d records)",
		CorruptChainLinks()-before, n)
}
