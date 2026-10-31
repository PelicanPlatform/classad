package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestSidecarBlockWithEmptyColdTailsSurvivesReopen: a sidecar columnar section stores no cold-tail
// names when no record has a cold tail, which is also how a section keyed by a segment dictionary
// looks. Read back on a segment with no dictionary, such a section was taken for a dictionary-keyed
// one that had lost its dictionary and dropped, so every sealed segment of a store whose ads fit the
// schema exactly row-scanned after each restart. A block with no cold-tail bytes has nothing to
// translate and is kept.
func TestSidecarBlockWithEmptyColdTailsSurvivesReopen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	open := func() *Collection {
		c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14, GroupSchemaCount: -1, ColumnarSegmentBudget: -1})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	for i := range 2000 {
		// Every ad has the same attributes, every value fits its slot: no record has a cold tail.
		ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d ]`, i, i%10, i%7, i%6))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("schema scan did not enable")
	}
	covered := func(stage string) int {
		t.Helper()
		n := 0
		for _, seg := range c.shards[0].segs {
			if seg == nil || seg.dict.Load() != nil {
				continue
			}
			cs := seg.colblk.Load()
			if cs == nil {
				continue
			}
			for _, b := range cs.blocks {
				if b.coldOffAt(b.n) != 0 {
					t.Fatalf("%s: a block has cold-tail bytes; the case goes untested", stage)
				}
			}
			n++
		}
		return n
	}
	before := covered("before the reopen")
	if before == 0 {
		t.Fatal("no sidecar block over a segment without a dictionary")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = open()
	defer c.Close()
	if after := covered("after the reopen"); after != before {
		t.Fatalf("%d of %d sidecar blocks survived the reopen", after, before)
	}
	q, err := vm.Parse("ClusterId > 100")
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := c.CountQuery(q); !ok || n != len(readAll(t, c, "ClusterId > 100")) {
		t.Fatalf("CountQuery(ClusterId > 100) = %d, %v after the reopen", n, ok)
	}
}
