package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestPersistentSegmentsReleaseFDs verifies the file-descriptor scaling fix: a
// persistent segment releases its backing fd once mapped, so a store with many
// segments holds no per-segment fds -- yet the data stays durable and reloads
// correctly. Segments are tiny here so a few hundred ads span many of them.
func TestPersistentSegmentsReleaseFDs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}

	const n = 400
	for i := 0; i < n; i++ {
		ad, err := classad.Parse(fmt.Sprintf(`[ Owner="u%d"; Seq=%d ]`, i%7, i))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}

	// Every persistent segment must be marked persistent AND hold no open fd (it was
	// released right after mmap). Tiny segments guarantee we made many of them.
	segs, withFD := 0, 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			segs++
			if !seg.persistent {
				t.Errorf("segment %d not marked persistent", seg.id)
			}
			if seg.file != nil {
				withFD++
			}
		}
		sh.mu.RUnlock()
	}
	if segs < 5 {
		t.Fatalf("expected many tiny segments, got %d", segs)
	}
	if withFD != 0 {
		t.Fatalf("%d of %d segments still hold an fd; want 0 (released after mmap)", withFD, segs)
	}
	t.Logf("%d segments, 0 open fds", segs)

	// Durability across all those fd-released segments: reopen and confirm every ad
	// survived with its value intact.
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c2, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if got := c2.Len(); got != n {
		t.Fatalf("recovered %d ads, want %d", got, n)
	}
	for i := 0; i < n; i++ {
		ad, ok := c2.Get([]byte(fmt.Sprintf("k%d", i)))
		if !ok {
			t.Fatalf("k%d missing after reopen", i)
			continue
		}
		if v, _ := ad.EvaluateAttrInt("Seq"); v != int64(i) {
			t.Fatalf("k%d Seq=%d after reopen, want %d", i, v, i)
		}
	}
}
