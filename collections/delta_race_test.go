package collections

import (
	"fmt"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestDeltaCollapseDoesNotLoseUpdates pins the snapshot discipline of the seal collapse.
//
// The collapse rewrites a key's chain as one whole record. Reading the value with c.Get and
// only THEN opening the transaction captured the snapshot after the read, so a commit landing
// in between could not conflict and was silently overwritten -- correctly-written,
// conflict-retrying clients lost committed updates. Writers here are properly
// snapshot-isolated and retry on conflict, so any lost increment is the store's fault.
func TestDeltaCollapseDoesNotLoseUpdates(t *testing.T) {
	// A small segment makes seals, and therefore collapses, frequent.
	c, err := Open(Options{Dir: t.TempDir(), Shards: 4, SegmentSize: 1 << 14, DeltaMax: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	const keys = 16
	for i := 0; i < keys; i++ {
		tx := c.Begin()
		tx.Put([]byte(fmt.Sprintf("r%d", i)), jobAd(i, map[string]int64{"N": 0}))
		if r := tx.Commit(); r.Conflicted() {
			t.Fatalf("seed %d conflicted", i)
		}
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	applied := make(map[string]int64)
	const writers, perWriter = 8, 1200
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for n := 0; n < perWriter; n++ {
				key := []byte(fmt.Sprintf("r%d", (w*7+n)%keys))
				for {
					tx := c.Begin()
					ad, ok := tx.Get(key)
					if !ok {
						break
					}
					v, _ := ad.EvaluateAttrInt("N")
					patch := classad.New()
					patch.InsertAttr("N", v+1)
					tx.PatchAttrs(key, patch, nil)
					if r := tx.Commit(); r.Conflicted() {
						continue // honest retry: this writer takes no credit for a lost race
					}
					mu.Lock()
					applied[string(key)]++
					mu.Unlock()
					break
				}
			}
		}(w)
	}
	wg.Wait()

	lost := 0
	for i := 0; i < keys; i++ {
		key := fmt.Sprintf("r%d", i)
		ad, ok := c.Get([]byte(key))
		if !ok {
			t.Fatalf("%s vanished", key)
		}
		got, _ := ad.EvaluateAttrInt("N")
		if want := applied[key]; got != want {
			t.Errorf("%s: N = %d, want %d (%d updates lost)", key, got, want, want-got)
			lost++
		}
		// And the whole ad must still be intact, not just the counter.
		if v, _ := ad.EvaluateAttrInt("Pad19"); v != 19 {
			t.Errorf("%s: Pad19 = %d, want 19 (attribute lost)", key, v)
		}
	}
	if lost > 0 {
		t.Fatalf("%d of %d keys lost committed updates", lost, keys)
	}
}
