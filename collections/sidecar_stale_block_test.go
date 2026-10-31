package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestSidecarRewriteWithUnusableColumnSectionDropsTheOldBlock: a block read back from a sidecar ALIASES
// that sidecar's mapping, which a sidecar rewrite (installSidecar) unmaps once no scan pins it. When
// the rewritten sidecar's column section yields no block, the old one used to stay published, pointing
// into the unmapped bytes; the next columnar read of the segment faulted (SIGSEGV in a count). The old
// block must not outlive its mapping: with nothing to replace it, the segment row-scans until rebuilt.
func TestSidecarRewriteWithUnusableColumnSectionDropsTheOldBlock(t *testing.T) {
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
		cid := fmt.Sprintf("%d", i)
		if i%50 == 1 {
			cid = `"text"` // a cold-tail entry, so the section names its cold ids and reloads
		}
		ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId=%s; ProcId=%d; Owner="user%d"; JobStatus=%d ]`, cid, i%10, i%7, i%6))
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
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = open() // the blocks now come from the sidecars' mappings
	defer c.Close()
	sh := c.shards[0]
	var seg *segment
	for _, s := range sh.segs {
		if s != nil && s.colblk.Load() != nil && snapshotPath(s) != "" {
			seg = s
			break
		}
	}
	if seg == nil {
		t.Fatal("no sealed segment with a sidecar block")
	}
	old := seg.colblk.Load()

	// Rewrite the sidecar with a column section that frames correctly but holds no block.
	path := snapshotPath(seg)
	c.reindexMu.Lock()
	data, closer, err := mapFile(path)
	if err != nil {
		c.reindexMu.Unlock()
		t.Fatal(err)
	}
	attr, key, _, zone, ok := splitSegmentSidecar(data)
	if !ok {
		_ = closer()
		c.reindexMu.Unlock()
		t.Fatal("sidecar does not split")
	}
	container := buildSegmentSidecar(append([]byte(nil), attr...), append([]byte(nil), key...),
		wrapColSection([]byte("not a columnar section"), seg.used), append([]byte(nil), zone...),
		seg.used, seg.dictRecOff(), seg.colRecOff(), encodeDamage(seg.damaged()))
	_ = closer()
	c.installSidecar(sh, seg, path, container)
	c.reindexMu.Unlock()

	if seg.colblk.Load() == old {
		t.Fatal("the block read from the displaced sidecar mapping is still published")
	}
	q, err := vm.Parse("ClusterId > 100")
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := c.CountQuery(q); ok && n != len(readAll(t, c, "ClusterId > 100")) {
		t.Fatalf("CountQuery(ClusterId > 100) = %d after the rewrite", n)
	}
}
