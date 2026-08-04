package collections

import (
	"fmt"
	"sync"
	"testing"
)

// TestSegDictResolveCache verifies the id->name cache returns the correct names (matching the
// zero-copy segDictName), is safe under concurrent first-use, and reports absence for an
// out-of-range id.
func TestSegDictResolveCache(t *testing.T) {
	names := make([]string, 500)
	for i := range names {
		names[i] = fmt.Sprintf("Attr_%d_%x", i, i*2654435761&0xffff)
	}
	h := &segDictHandle{data: appendSegDict(nil, names)}

	// Concurrent first use: many goroutines race to build the cache; all must agree.
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range names {
				got, ok := h.resolve(uint32(id))
				if !ok || got != names[id] {
					t.Errorf("resolve(%d)=%q,%v want %q", id, got, ok, names[id])
				}
			}
		}()
	}
	wg.Wait()

	// Cached results match the direct mmap read for every id.
	for id := range names {
		got, _ := h.resolve(uint32(id))
		if got != string(segDictName(h.data, 0, uint32(id))) {
			t.Fatalf("cache/mmap disagree at id %d", id)
		}
	}
	// Out of range.
	if _, ok := h.resolve(uint32(len(names))); ok {
		t.Errorf("resolve(out-of-range) should be false")
	}
}
