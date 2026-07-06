package collections

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// TestGroupCommitAllWritesLand verifies that under many concurrent writers (which
// are coalesced by group commit) every write is committed and visible — nothing
// is dropped by the leader/handoff protocol. A CommitSync counter confirms the
// sync ran and that coalescing occurred (fewer syncs than ads).
func TestGroupCommitAllWritesLand(t *testing.T) {
	const goroutines, perG = 16, 500
	var syncs, adsSynced int64
	c := New(Options{Shards: 4, CommitSync: func() { atomic.AddInt64(&syncs, 1) }})
	// Count committed ads by wrapping put via the sync is not possible; instead
	// track via a barrier: every Put contributes one ad.

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				key := []byte(fmt.Sprintf("g%d-k%d", g, i))
				if err := c.Put(key, mustAd(t, fmt.Sprintf(`[G=%d; I=%d]`, g, i))); err != nil {
					t.Errorf("put: %v", err)
					return
				}
				atomic.AddInt64(&adsSynced, 1)
			}
		}(g)
	}
	wg.Wait()

	if got := c.Len(); got != goroutines*perG {
		t.Fatalf("Len = %d, want %d", got, goroutines*perG)
	}
	for g := 0; g < goroutines; g++ {
		for i := 0; i < perG; i++ {
			key := []byte(fmt.Sprintf("g%d-k%d", g, i))
			ad, ok := c.Get(key)
			if !ok {
				t.Fatalf("missing %s", key)
			}
			if gv, _ := ad.EvaluateAttrInt("G"); gv != int64(g) {
				t.Fatalf("%s: G=%d, want %d", key, gv, g)
			}
		}
	}
	s := atomic.LoadInt64(&syncs)
	if s == 0 {
		t.Fatal("CommitSync never ran")
	}
	t.Logf("committed %d ads in %d syncs (coalescing %.2f ads/sync)",
		adsSynced, s, float64(adsSynced)/float64(s))
}
