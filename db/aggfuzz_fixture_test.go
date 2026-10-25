package db

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/aggfuzz/gen"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
)

// Fixtures for the aggregate differential fuzzer: building an archive out of a gen.Shape, and the
// cache that keeps one per shape alive for a whole fuzz run.
//
// Building is the expensive part (append, merge, columnarize, derive group schemas, reopen), and a
// fuzz iteration is one query. Rebuilding per iteration would cap a 60s run at a few hundred
// queries, so the data varies over gen's deliberately-steered shapes and the FUZZER varies the
// query. The shapes are indexed by the input's first byte, so mutation still moves between them.

// buildFuzzArchive builds and maintains an archive holding exactly ads, the way the server maintains
// one: appended in batches, merged, columnarized, group schemas derived, reopened once so the
// columns are read back from disk, and left with an unsealed tail -- which matters, because the
// unsealed tail is the part no zone map prunes and no sidecar index covers, so every fast path has
// to handle it separately from the sealed segments.
func buildFuzzArchive(tb testing.TB, dir string, shape gen.Shape, ads []*classad.ClassAd) (*Catalog, *ArchiveTable) {
	tb.Helper()
	cfg := ArchiveConfig{
		SegmentSize:        shape.SegmentBytes,
		RowGroupBytes:      shape.RowGroupBytes,
		CategoricalAttrs:   gen.CategoricalAttrs,
		ValueAttrs:         gen.ValueAttrs,
		ZoneAttrs:          gen.ZoneAttrs,
		GroupStabilityRuns: 1,
	}
	var cat *Catalog
	var h *ArchiveTable
	open := func() {
		var err error
		if cat, err = OpenCatalog(dir); err != nil {
			tb.Fatal(err)
		}
		if h, err = cat.CreateArchiveTable("history", cfg); err != nil {
			tb.Fatal(err)
		}
	}
	maintain := func() {
		h.MergePass(MergeOptions{})
		if stale, _ := h.StaleIndexSegments(); stale > 0 {
			h.Reindex()
		}
		h.BuildAndEnableSchemaScan(2000, shape.HotTopN)
		h.GroupSchemas(2000, 0)
	}
	open()
	// 60/25/15. The last batch is never maintained, so it stays the unsealed tail.
	cuts := []int{len(ads) * 60 / 100, len(ads) * 85 / 100, len(ads)}
	at := 0
	for bi, end := range cuts {
		for ; at < end; at++ {
			if err := h.Append(ads[at]); err != nil {
				tb.Fatal(err)
			}
		}
		if bi == len(cuts)-1 {
			break
		}
		maintain()
		maintain() // the second pass adopts the group schemas the first derived
		if bi == 0 {
			cat.Close()
			open() // reopen: the columns must be read back from disk, not from the writer's memory
		}
	}
	return cat, h
}

// aggFixture is one built archive plus the shape that generated it.
type aggFixture struct {
	shape gen.Shape
	cat   *Catalog
	tbl   *ArchiveTable
}

// fixtureCache keeps one built archive per shape for the lifetime of the test or fuzz target that
// created it. The directories hang off the owner's TempDir, so they are removed with it; in -fuzz
// mode each worker process builds its own.
type fixtureCache struct {
	mu   sync.Mutex
	root string
	m    map[int]*aggFixture
}

func newFixtureCache(tb testing.TB) *fixtureCache {
	c := &fixtureCache{root: tb.TempDir(), m: map[int]*aggFixture{}}
	// Close the catalogs before TempDir removes the files out from under their mmaps.
	tb.Cleanup(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, f := range c.m {
			f.cat.Close()
		}
	})
	return c
}

func (c *fixtureCache) get(tb testing.TB, shape gen.Shape) *aggFixture {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f, ok := c.m[shape.ID]; ok {
		return f
	}
	dir := filepath.Join(c.root, fmt.Sprintf("shape%02d", shape.ID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		tb.Fatal(err)
	}
	cat, h := buildFuzzArchive(tb, dir, shape, shape.Ads())
	f := &aggFixture{shape: shape, cat: cat, tbl: h}
	// The fixture is only worth fuzzing if it has the shape the fast paths need. A silently
	// unaccelerated archive would send every query to the reference scan: a green run that
	// compared the scan against itself.
	if info := h.SchemaScanInfo(); info.CoveredSegments == 0 {
		tb.Fatalf("shape %d: no columnarized segments (%+v); every query would take the reference scan",
			shape.ID, info)
	}
	if _, ok := h.CategoricalGroupCounts("Owner"); !ok {
		tb.Fatalf("shape %d: the categorical index cannot account for Owner; the index tier would never fire",
			shape.ID)
	}
	c.m[shape.ID] = f
	return f
}

// TestAggregateFuzzFixtureSteering asserts that each generated archive came out with the STRUCTURE
// the fast paths are gated on. Everything here is a steering knob that can stop working silently: a
// schema derivation that drops a column, a segment size that stops sealing, a zone map that stops
// pruning. None of them would fail a differential run -- the comparison would just quietly become
// the scan against itself.
func TestAggregateFuzzFixtureSteering(t *testing.T) {
	cache := newFixtureCache(t)
	widths := map[string]map[int]bool{} // attr -> the int widths seen across shapes
	for _, shape := range gen.Shapes() {
		f := cache.get(t, shape)
		info := f.tbl.SchemaScanInfo()
		if info.SealedSegments < 2 {
			t.Errorf("shape %d sealed %d segments; zone maps only prune SEALED ones, so a "+
				"single-segment archive tests no pruning at all", shape.ID, info.SealedSegments)
		}
		byName := map[string]collections.SchemaScanField{}
		for _, fl := range info.Schema {
			byName[fl.Name] = fl
		}
		for name, kind := range map[string]string{
			"Owner": "string", "Host": "string", "Site": "string", "NumLike": "string",
			"ProcId": "int", "Status": "int", "Prio": "int", "Small": "int", "Mem": "int", "Disk": "int",
			"Big": "int", "Ts": "int", "ClusterId": "int", "Rate": "real", "Calc": "real",
			"Flag": "bool",
		} {
			fl, ok := byName[name]
			if !ok {
				t.Errorf("shape %d: %s is not a columnar field; every query on it would scan", shape.ID, name)
				continue
			}
			if fl.Kind != kind {
				t.Errorf("shape %d: %s is a %s column, want %s", shape.ID, name, fl.Kind, kind)
			}
			if fl.Kind == "int" {
				if widths[name] == nil {
					widths[name] = map[int]bool{}
				}
				widths[name][fl.Width] = true
			}
		}
		// Rare is below the presence floor by construction: it is the attribute that must NOT be
		// columnar, so that every run also covers a group/aggregate column the columns do not carry.
		if _, ok := byName["Rare"]; ok {
			t.Errorf("shape %d: Rare became a columnar field; it is meant to sit below the presence floor", shape.ID)
		}
		// Zone maps have to actually prune. A narrow window over the monotonically advancing Ts
		// must leave most segments untouched; if it does not, every "pruned segment" path in the
		// aggregate chain is running on an archive where pruning never happens.
		var st collections.ScanStats
		lo := gen.TsBase + shape.N*gen.TsStep*3/4
		seq, err := f.tbl.QueryProjectStats(fmt.Sprintf("Ts >= %d", lo), []string{"Ts"}, &st)
		if err != nil {
			t.Fatal(err)
		}
		for range seq {
		}
		if st.SegmentsPruned == 0 {
			t.Errorf("shape %d: a Ts >= %d scan pruned 0 of %d segments", shape.ID, lo, st.SegmentsTotal)
		}
	}
	// Across the shape set, the integer columns have to span the slot widths: a 1-byte column reads
	// differently from an 8-byte one, and a run that only ever saw one width tested one reader.
	seen := map[int]bool{}
	for _, ws := range widths {
		for w := range ws {
			seen[w] = true
		}
	}
	for _, w := range []int{1, 2, 4, 6, 8} {
		if !seen[w] {
			t.Errorf("no %d-byte integer column in any shape; widths seen: %v", w, seen)
		}
	}
	t.Logf("integer column widths by attribute: %v", widths)
}
