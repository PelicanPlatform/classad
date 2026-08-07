package collections

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
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

// TestSchemaScanReloadCorruptSection verifies the reload's "any doubt rebuilds" contract: a
// bit-flipped columnar section fails its CRC and is ignored, so that segment row-scans while the
// rest stay columnar -- and CountConstraint still returns the correct answer.
func TestSchemaScanReloadCorruptSection(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	exprs := []string{"Memory > 4096", "Memory <= 4096"}

	open := func() *Collection {
		c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 12})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	for i := 0; i < 600; i++ {
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
	if !c.BuildAndEnableSchemaScan(2000, 4) {
		t.Fatal("BuildAndEnableSchemaScan false")
	}
	for i := 600; i < 2400; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAdOld(t, fmt.Sprintf("Cpus=%d\nMemory=%d\nDisk=%d", 1+i%8, 1024+(i%64)*256, i*4096))); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]int{}
	for _, e := range exprs {
		want[e], _ = c.CountConstraint(e)
	}
	c.Close()

	// Corrupt the columnar body of exactly one v2 sidecar (flip a byte just before the 16-byte
	// container trailer, inside the col section; the trailer stays intact so the container still
	// parses and yields the section, but its CRC now fails).
	corrupted := ""
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".idx" || corrupted != "" {
			return nil
		}
		b, e := os.ReadFile(p)
		if e != nil || len(b) < sidecarTrailerLenV2+colSectionHdr+1 {
			return nil
		}
		// Locate the columnar section by its recorded length rather than by position: a v3
		// container appends a zone section after it, so "just before the trailer" is no
		// longer the col body.
		var attrLen, keyLen, colLen int
		switch binary.LittleEndian.Uint32(b[len(b)-4:]) {
		case sidecarContainerMagicV4:
			t := b[len(b)-sidecarTrailerLenV4:]
			attrLen = int(binary.LittleEndian.Uint32(t[0:]))
			keyLen = int(binary.LittleEndian.Uint32(t[4:]))
			colLen = int(binary.LittleEndian.Uint32(t[8:]))
		case sidecarContainerMagicV3:
			t := b[len(b)-sidecarTrailerLenV3:]
			attrLen = int(binary.LittleEndian.Uint32(t[0:]))
			keyLen = int(binary.LittleEndian.Uint32(t[4:]))
			colLen = int(binary.LittleEndian.Uint32(t[8:]))
		case sidecarContainerMagicV2:
			t := b[len(b)-sidecarTrailerLenV2:]
			attrLen = int(binary.LittleEndian.Uint32(t[0:]))
			keyLen = int(binary.LittleEndian.Uint32(t[4:]))
			colLen = int(binary.LittleEndian.Uint32(t[8:]))
		default:
			return nil // v1 (no columnar section)
		}
		if colLen == 0 {
			return nil
		}
		b[attrLen+keyLen+colLen-1] ^= 0xFF // last byte of the col body
		if e := os.WriteFile(p, b, 0o644); e != nil {
			t.Fatal(e)
		}
		corrupted = p
		return nil
	})
	if corrupted == "" {
		t.Fatal("no sidecar with a columnar section found to corrupt")
	}

	before := colSegmentBuilds.Load()
	c2 := open()
	defer c2.Close()
	if built := colSegmentBuilds.Load() - before; built != 0 {
		t.Fatalf("reopen rebuilt %d blocks (want 0; a corrupt section should just row-scan)", built)
	}
	// Exactly one sealed segment should now lack a block (the corrupted one), and reads are correct.
	missing := 0
	for _, sh := range c2.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil && seg != sh.act && seg.used > 0 && seg.colblk.Load() == nil {
				missing++
			}
		}
		sh.mu.RUnlock()
	}
	if missing == 0 {
		t.Fatal("corrupt section was not rejected (segment still has a block)")
	}
	for _, e := range exprs {
		got, ok := c2.CountConstraint(e)
		if !ok || got != want[e] {
			t.Fatalf("%q after corruption: ok=%v got=%d want=%d", e, ok, got, want[e])
		}
	}
}

// TestSchemaScanReindexKeepsBlock exercises the zero-copy block's mapping-swap path: a reindex
// (AddIndex) rewrites and remaps each sealed segment's sidecar. The columnar block aliases that
// mapping, so it must be re-published from the new mapping on the swap (like msidx) -- otherwise it
// would read a released mapping. Query correctness must survive the reindex, and it must NOT
// re-transcode the block from records (colBlobPreserve re-marshals the existing one).
func TestSchemaScanReindexKeepsBlock(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 300; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAdOld(t, fmt.Sprintf("Cpus=%d\nMemory=%d\nOwner=\"u%d\"", 1+i%8, 1024+(i%64)*256, i%5))); err != nil {
			t.Fatal(err)
		}
	}
	mq, _ := vm.Parse("true")
	for i := 0; i < 20; i++ {
		for range c.QueryProject(mq, []string{"Memory"}) {
		}
	}
	if !c.BuildAndEnableSchemaScan(2000, 4) {
		t.Fatal("BuildAndEnableSchemaScan false")
	}
	for i := 300; i < 1500; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAdOld(t, fmt.Sprintf("Cpus=%d\nMemory=%d\nOwner=\"u%d\"", 1+i%8, 1024+(i%64)*256, i%5))); err != nil {
			t.Fatal(err)
		}
	}
	expr := "Memory > 4096"
	want, ok := c.CountConstraint(expr)
	if !ok {
		t.Fatal("CountConstraint declined pre-reindex")
	}

	// Reindex every sealed segment by adding a categorical index -> reindexSealedFile rewrites and
	// remaps each sidecar, re-publishing the aliased block from the new mapping.
	before := colSegmentBuilds.Load()
	if !c.AddIndex([]string{"Owner"}, nil) {
		t.Fatal("AddIndex returned false")
	}
	if built := colSegmentBuilds.Load() - before; built != 0 {
		t.Fatalf("reindex re-transcoded %d blocks (want 0; colBlobPreserve should re-marshal)", built)
	}
	// The block still answers correctly after the mapping swap (a use-after-free would corrupt this).
	got, ok := c.CountConstraint(expr)
	if !ok || got != want {
		t.Fatalf("post-reindex %q: ok=%v got=%d want=%d", expr, ok, got, want)
	}
	// And the categorical query the reindex enabled also works (the reindex genuinely happened).
	q, _ := vm.Parse(`Owner == "u3"`)
	n := 0
	for range c.Query(q) {
		n++
	}
	if n == 0 {
		t.Fatal("Owner index query returned nothing after AddIndex")
	}
}
