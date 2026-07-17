package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestSealedSegmentsGoMmap is the behavior-flip anchor: on a persistent collection, sealed
// segments convert to the mmap sidecar (msidx set, heap idx freed) while the active segment
// stays in-RAM; queries stay correct through the mmap'd segments; a compaction that reaps a
// mmap'd segment does not crash (its sidecar unmaps via onReap); and a reopen maps the
// sidecars from disk without re-indexing.
func TestSealedSegmentsGoMmap(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	opts := Options{Shards: 2, Dir: dir, SegmentSize: 1 << 13, // small -> many segments roll
		CategoricalAttrs: []string{"Owner"}, ValueAttrs: []string{"Memory"}}
	c, err := Open(opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	src := map[int]bool{} // ids expected to match the probe query below
	put := func(i int) {
		mem := (i%16 + 1) * 512
		if err := c.Put([]byte(fmt.Sprintf("m%d", i)),
			mustAd(t, fmt.Sprintf(`[ Id=%d; Owner="u%d"; Memory=%d ]`, i, i%40, mem))); err != nil {
			t.Fatal(err)
		}
		src[i] = (i%40 == 3) && mem >= 4096
	}
	for i := 0; i < 5000; i++ {
		put(i)
	}
	c.Reindex()

	countMmap := func(col *Collection) (mmapped, activeInRAM int) {
		for _, sh := range col.shards {
			for _, seg := range sh.segs {
				if seg == nil {
					continue
				}
				if seg == sh.act {
					if seg.idx.Load() != nil {
						activeInRAM++
					}
				} else if seg.msidx.Load() != nil && seg.idx.Load() == nil {
					mmapped++
				}
			}
		}
		return
	}
	mmapped, _ := countMmap(c)
	if mmapped == 0 {
		t.Fatal("no sealed segment was converted to the mmap sidecar")
	}

	q := func() *vm.Query {
		x, err := vm.Parse(`Owner == "u3" && Memory >= 4096`)
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	want := func() []int {
		var ids []int
		for i, ok := range src {
			if ok {
				ids = append(ids, i)
			}
		}
		return sortedIntsOf(ids)
	}()
	if got := queryIDs(t, c, q()); !equalInts(got, want) {
		t.Fatalf("through mmap'd sealed segments: got %d ids, want %d", len(got), len(want))
	}

	// Compaction: supersede a chunk of keys to create garbage, then compact. This reaps old
	// segments -- some mmap-sealed -- so a use-after-unmap in the reap path would crash the
	// query that follows.
	for i := 0; i < 1500; i++ {
		put(i) // rewrite (supersedes the prior record); src[i] unchanged (same values)
	}
	c.Compact()
	c.Reindex()
	if got := queryIDs(t, c, q()); !equalInts(got, want) {
		t.Fatalf("after compaction: got %d ids, want %d", len(got), len(want))
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: sealed segments map their sidecars from disk (no CLIX, no full re-index).
	c2, err := Open(opts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c2.Close()
	if mm2, _ := countMmap(c2); mm2 == 0 {
		t.Fatal("reopen did not map any sealed sidecar into msidx")
	}
	if got := queryIDs(t, c2, q()); !equalInts(got, want) {
		t.Fatalf("after reopen: got %d ids, want %d", len(got), len(want))
	}
}

func sortedIntsOf(ids []int) []int {
	for i := 1; i < len(ids); i++ { // insertion sort (small, test-only)
		for j := i; j > 0 && ids[j-1] > ids[j]; j-- {
			ids[j-1], ids[j] = ids[j], ids[j-1]
		}
	}
	return ids
}
