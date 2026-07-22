package collections

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestTxnCommitParallelSync verifies that a commit touching multiple shards runs their
// durability syncs CONCURRENTLY rather than one after another: the CommitSync hook (the
// per-shard durability point) sees more than one shard syncing at once, and the commit's
// wall time is far below the serial sum of the per-shard syncs.
func TestTxnCommitParallelSync(t *testing.T) {
	const hold = 20 * time.Millisecond
	var mu sync.Mutex
	var cur, maxConcurrent, calls int
	hook := func() {
		mu.Lock()
		cur++
		calls++
		if cur > maxConcurrent {
			maxConcurrent = cur
		}
		mu.Unlock()
		time.Sleep(hold) // stand in for an fsync so overlaps are observable
		mu.Lock()
		cur--
		mu.Unlock()
	}

	c := New(Options{Shards: 16, CommitSync: hook})
	tx := c.Begin()
	const n = 200 // enough distinct keys to spread across all 16 shards
	for i := 0; i < n; i++ {
		tx.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, `[ x = 1 ]`))
	}

	start := time.Now()
	res := tx.Commit()
	elapsed := time.Since(start)

	if res.Committed != n {
		t.Fatalf("committed %d writes, want %d", res.Committed, n)
	}
	mu.Lock()
	mc, nc := maxConcurrent, calls
	mu.Unlock()

	if nc < 2 {
		t.Fatalf("CommitSync fired %d times; the writes should have spanned multiple shards", nc)
	}
	if mc < 2 {
		t.Fatalf("max concurrent syncs = %d; the per-shard syncs did not overlap (still serial)", mc)
	}
	// Serial execution would take at least nc*hold; concurrent should be a small multiple
	// of a single hold. Assert well under the serial bound (generous slack for scheduling).
	if serial := time.Duration(nc) * hold; elapsed >= serial {
		t.Fatalf("commit took %v to sync %d shards of %v each -- looks serial (serial bound %v)", elapsed, nc, hold, serial)
	}
	t.Logf("commit synced %d shards, up to %d concurrently, in %v (serial would be ~%v)",
		nc, mc, elapsed, time.Duration(nc)*hold)
}
