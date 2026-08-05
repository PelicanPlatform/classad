package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestSchemaScanPersistAtEnable covers the persist-at-enable path: segments sealed BEFORE
// schema-scan is enabled must have their columnar block written to the sidecar at enable time (not
// just kept in RAM), so a reopen reloads them off disk with ZERO rebuilds instead of row-falling-
// back. (Before this, only segments sealed AFTER enable persisted.)
func TestSchemaScanPersistAtEnable(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	exprs := []string{"Memory > 4096", "Memory <= 4096", "Memory >= 2000 && Memory < 9000"}
	open := func() *Collection {
		c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 12})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	truth := func(c *Collection, e string) int {
		q, _ := vm.Parse(e)
		n := 0
		for range c.Query(q) {
			n++
		}
		return n
	}
	sealedWithBlock := func(c *Collection) (sealed, withBlk int) {
		for _, sh := range c.shards {
			sh.mu.RLock()
			for _, seg := range sh.segs {
				if seg != nil && seg != sh.act && seg.used > 0 {
					sealed++
					if seg.colblk.Load() != nil {
						withBlk++
					}
				}
			}
			sh.mu.RUnlock()
		}
		return
	}

	// Write ALL the data first -> segments seal as inline (schema-scan not yet enabled).
	c := open()
	const n = 2400
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAdOld(t, fmt.Sprintf("Cpus=%d\nMemory=%d\nDisk=%d", 1+i%8, 1024+(i%64)*256, i*4096))); err != nil {
			t.Fatal(err)
		}
	}
	mq, _ := vm.Parse("true")
	for i := 0; i < 20; i++ {
		for range c.QueryProject(mq, []string{"Memory"}) {
		}
	}
	// Enable NOW, over already-sealed segments -> they must persist at enable time.
	if !c.BuildAndEnableSchemaScan(2000, 4) {
		t.Fatal("BuildAndEnableSchemaScan false")
	}
	sealed, withBlk := sealedWithBlock(c)
	if sealed == 0 || withBlk != sealed {
		t.Fatalf("pre-reopen: %d/%d sealed segments have a block (want all)", withBlk, sealed)
	}
	want := map[string]int{}
	for _, e := range exprs {
		got, ok := c.CountConstraint(e)
		if !ok || got != truth(c, e) {
			t.Fatalf("pre-reopen %q: ok=%v got=%d", e, ok, got)
		}
		want[e] = got
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: the pre-enable segments' blocks must reload off disk, no rebuild.
	before := colSegmentBuilds.Load()
	c2 := open()
	defer c2.Close()
	if built := colSegmentBuilds.Load() - before; built != 0 {
		t.Fatalf("reopen rebuilt %d blocks (want 0 -- persist-at-enable should reload from disk)", built)
	}
	if c2.schemaScan.Load() == nil {
		t.Fatal("reopen did not re-enable schema-scan (blocks not persisted at enable?)")
	}
	s2, w2 := sealedWithBlock(c2)
	if s2 == 0 || w2 != s2 {
		t.Fatalf("post-reopen: %d/%d sealed segments reloaded a block (want all)", w2, s2)
	}
	for _, e := range exprs {
		got, ok := c2.CountConstraint(e)
		if !ok || got != want[e] || got != truth(c2, e) {
			t.Fatalf("post-reopen %q: ok=%v got=%d want=%d", e, ok, got, want[e])
		}
	}
}
