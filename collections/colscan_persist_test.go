package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestSchemaScanReloadZeroRebuild is the P3(2) payoff: with schema-scan enabled, segments that
// seal persist their columnar block into the sidecar. On reopen the accelerator comes back LIVE
// straight off disk -- schema-scan re-enabled by adopt-from-sidecar, every persisted block
// reloaded, CountConstraint auto-routing to the same answers -- with ZERO block rebuilds (no
// re-sample, no re-transcode).
func TestSchemaScanReloadZeroRebuild(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	exprs := []string{"Memory > 4096", "Memory <= 4096", "Memory >= 2000 && Memory < 9000"}

	open := func() *Collection {
		c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 12}) // small ⇒ many sealed segments
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	put := func(c *Collection, lo, hi int) {
		for i := lo; i < hi; i++ {
			ad := mustAdOld(t, fmt.Sprintf("Cpus=%d\nMemory=%d\nDisk=%d\nMachine=\"m%05d\"",
				1+i%8, 1024+(i%64)*256, i*4096, i))
			if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
				t.Fatal(err)
			}
		}
	}
	truth := func(c *Collection, expr string) int {
		q, _ := vm.Parse(expr)
		n := 0
		for range c.Query(q) {
			n++
		}
		return n
	}
	countPersistedBlocks := func(c *Collection) int {
		n := 0
		for _, sh := range c.shards {
			sh.mu.RLock()
			for _, seg := range sh.segs {
				if seg != nil && seg != sh.act && seg.colblk.Load() != nil {
					n++
				}
			}
			sh.mu.RUnlock()
		}
		return n
	}

	// Build a first tranche, sample it, and enable schema-scan. Then write MORE so fresh segments
	// seal WHILE schema-scan is enabled -- those persist their block at seal.
	c := open()
	put(c, 0, 600)
	mq, _ := vm.Parse("true")
	for i := 0; i < 20; i++ {
		for range c.QueryProject(mq, []string{"Memory"}) { // drive Memory read demand -> hot tier
		}
	}
	if !c.BuildAndEnableSchemaScan(2000, 4) {
		t.Fatal("BuildAndEnableSchemaScan returned false")
	}
	put(c, 600, 2400) // these segments seal with a persisted columnar block

	persisted := countPersistedBlocks(c)
	if persisted == 0 {
		t.Fatal("no segment persisted a columnar block after enabling schema-scan")
	}
	want := map[string]int{}
	for _, e := range exprs {
		got, ok := c.CountConstraint(e)
		if !ok || got != truth(c, e) {
			t.Fatalf("pre-reopen %q: ok=%v got=%d want=%d", e, ok, got, truth(c, e))
		}
		want[e] = got
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: the accelerator must come back off disk with NO block builds.
	before := colSegmentBuilds.Load()
	c2 := open()
	defer c2.Close()
	if built := colSegmentBuilds.Load() - before; built != 0 {
		t.Fatalf("reopen rebuilt %d columnar blocks (want 0 -- should reload from disk)", built)
	}
	if c2.schemaScan.Load() == nil {
		t.Fatal("reopen did not re-enable schema-scan from the persisted blocks (adopt-from-sidecar)")
	}
	if got := countPersistedBlocks(c2); got < persisted {
		t.Fatalf("reopen recovered %d columnar blocks, had %d before close", got, persisted)
	}
	for _, e := range exprs {
		got, ok := c2.CountConstraint(e)
		if !ok {
			t.Fatalf("post-reopen %q: CountConstraint declined", e)
		}
		if got != want[e] || got != truth(c2, e) {
			t.Fatalf("post-reopen %q: got=%d want=%d truth=%d", e, got, want[e], truth(c2, e))
		}
	}
}
