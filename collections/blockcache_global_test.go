package collections

import (
	"sort"
	"testing"
)

// snapshotBudget captures a kind's current shared-cache budget (creating the cache if needed) and
// returns a restore closure, so a test that shrinks the process-global cache does not leak that
// change into later tests in the same binary.
func snapshotBudget(t *testing.T, kind blockCacheKind) func() {
	t.Helper()
	slot := &globalBlockCaches[kind]
	def := int64(defaultMutatingCacheBytes)
	if kind == archiveCacheKind {
		def = defaultArchiveCacheBytes
	}
	bc := slot.get(def)
	old := def
	if bc != nil {
		old = bc.c.MaxCost()
	}
	return func() { slot.setBudget(old) }
}

// TestBlockCacheKindsAreSeparate: an append-only (archive) collection and a mutating collection must
// draw from DIFFERENT process-global caches -- the whole point of two budgets for two workloads.
func TestBlockCacheKindsAreSeparate(t *testing.T) {
	mut := New(Options{Shards: 1})
	defer mut.Close()
	arc := New(Options{AppendOnly: true})
	defer arc.Close()

	mc := mut.sharedColCache()
	ac := arc.sharedColCache()
	if mc == nil || ac == nil {
		t.Fatal("shared caches were not created")
	}
	if mc == ac {
		t.Fatal("mutating and archive collections share one cache; the two budgets are not separate")
	}
	if mc != globalBlockCaches[mutatingCacheKind].bc {
		t.Error("mutating collection is not on the global mutating cache")
	}
	if ac != globalBlockCaches[archiveCacheKind].bc {
		t.Error("archive collection is not on the global archive cache")
	}
}

// TestBlockCacheSharedAcrossCollections: two collections of the SAME kind must resolve to the SAME
// cache instance, so the budget is a true global ceiling rather than N×per-collection.
func TestBlockCacheSharedAcrossCollections(t *testing.T) {
	m1 := New(Options{Shards: 1})
	defer m1.Close()
	m2 := New(Options{Shards: 4})
	defer m2.Close()
	if m1.sharedColCache() != m2.sharedColCache() {
		t.Error("two mutating collections do not share one cache; the budget is not global")
	}

	a1 := New(Options{AppendOnly: true})
	defer a1.Close()
	a2 := New(Options{AppendOnly: true})
	defer a2.Close()
	if a1.sharedColCache() != a2.sharedColCache() {
		t.Error("two archive collections do not share one cache; the budget is not global")
	}
}

// TestSharedBlockCacheSlotBudget exercises the config-knob mechanism on a LOCAL slot (so it cannot
// disturb the global caches): a budget set before creation sizes the cache, and a budget set after
// creation resizes the live cache in place.
func TestSharedBlockCacheSlotBudget(t *testing.T) {
	var slot sharedBlockCacheSlot

	// Set before first use: the cache is created at that size, not the default.
	const before = 7 << 20
	slot.setBudget(before)
	bc := slot.get(defaultMutatingCacheBytes)
	if bc == nil {
		t.Fatal("cache not created")
	}
	if got := bc.c.MaxCost(); got != before {
		t.Fatalf("budget-before-create: MaxCost %d, want %d", got, before)
	}

	// Set after creation: the SAME live instance is resized (no new cache).
	const after = 3 << 20
	slot.setBudget(after)
	if slot.get(defaultMutatingCacheBytes) != bc {
		t.Fatal("setBudget replaced the cache instead of resizing it")
	}
	if got := bc.c.MaxCost(); got != after {
		t.Fatalf("budget-after-create: MaxCost %d, want %d", got, after)
	}

	// A non-positive budget is ignored (keeps the current value).
	slot.setBudget(0)
	slot.setBudget(-1)
	if got := bc.c.MaxCost(); got != after {
		t.Fatalf("non-positive budget changed MaxCost to %d, want %d", got, after)
	}
}

// TestBlockCacheBudgetConfigKnob checks the exported knobs (and Options fields) route to the correct
// global kind and resize the live cache. It restores the prior budgets so later tests are unaffected.
func TestBlockCacheBudgetConfigKnob(t *testing.T) {
	defer snapshotBudget(t, mutatingCacheKind)()
	defer snapshotBudget(t, archiveCacheKind)()

	const mutBytes = 11 << 20
	const arcBytes = 13 << 20
	SetMutatingBlockCacheBudget(mutBytes)
	SetArchiveBlockCacheBudget(arcBytes)

	if got := globalBlockCaches[mutatingCacheKind].get(defaultMutatingCacheBytes).c.MaxCost(); got != mutBytes {
		t.Errorf("mutating budget MaxCost %d, want %d", got, mutBytes)
	}
	if got := globalBlockCaches[archiveCacheKind].get(defaultArchiveCacheBytes).c.MaxCost(); got != arcBytes {
		t.Errorf("archive budget MaxCost %d, want %d", got, arcBytes)
	}

	// Options.MutatingBlockCacheBytes on New feeds the same global setter.
	const viaOpts = 17 << 20
	c := New(Options{Shards: 1, MutatingBlockCacheBytes: viaOpts})
	defer c.Close()
	if got := c.sharedColCache().c.MaxCost(); got != viaOpts {
		t.Errorf("Options.MutatingBlockCacheBytes did not take effect: MaxCost %d, want %d", got, viaOpts)
	}
}

// TestTinyCacheColnativeUnchanged is the correctness guarantee at the block-cache read path: a
// COLUMNARIZED record must read back byte-identical even when the shared cache is far too small to
// hold a single block, so every read re-decompresses. recordWire on a columnar-native segment goes
// straight through blockCache.stream, so this proves the sharing/shrinking never changes results,
// only hit rate. Columnar-native rewrite is a persistent-store feature (it allocates a replacement
// segment file); the in-memory store's columnar read path is covered by
// TestTinyCacheScanResultsUnchanged below.
func TestTinyCacheColnativeUnchanged(t *testing.T) {
	defer snapshotBudget(t, mutatingCacheKind)()
	SetMutatingBlockCacheBudget(4 << 10) // 4 KiB: cannot hold a block, forces re-decompress

	c, s, hot := columnarFixture(t, 3000) // persistent (t.TempDir)
	defer c.Close()

	sh := c.shards[0]
	sh.mu.Lock()
	act := sh.act
	var src *segment
	for _, seg := range sh.segs {
		if seg != nil && seg != act && seg.used > 0 && !seg.columnarized() {
			src = seg
			break
		}
	}
	if src == nil {
		sh.mu.Unlock()
		t.Skip("no sealed segment to columnarize")
	}

	// Truth: what the segment holds before columnarization.
	before := map[string]string{}
	var buf []byte
	for off := 0; off < src.used; {
		o := uint32(off)
		rl := recTotalLen(src.data, o)
		if rl == 0 {
			break
		}
		off += int(rl)
		if recIsMarker(src.data, o) {
			continue
		}
		raw, err := src.codec.Decompress(buf[:0], recAd(src.data, o))
		if err != nil {
			sh.mu.Unlock()
			t.Fatal(err)
		}
		buf = raw
		before[string(recKey(src.data, o))] = adSummary(t, c, raw)
	}

	dst, _, _ := c.columnarizeSegment(sh, src, s, hot)
	sh.mu.Unlock()
	if dst == nil || !dst.columnarized() {
		t.Skip("segment did not columnarize")
	}
	defer func() {
		dst.retire()
		dst.reapAndHook()
	}()
	if c.colCache == nil {
		t.Fatal("shared cache not attached")
	}
	if got := c.colCache.c.MaxCost(); got != 4<<10 {
		t.Fatalf("tiny budget not in effect: MaxCost %d", got)
	}

	// Read every record back THROUGH the block cache and confirm it is unchanged. Loop twice so at
	// least one pass runs against a cache that has already thrashed.
	for pass := 0; pass < 2; pass++ {
		for off := 0; off < dst.used; {
			o := uint32(off)
			rl := recTotalLen(dst.data, o)
			if rl == 0 {
				break
			}
			off += int(rl)
			if recIsMarker(dst.data, o) {
				continue
			}
			full, err := c.recordWire(dst, o, nil)
			if err != nil {
				t.Fatalf("pass %d record %d: %v", pass, o, err)
			}
			key := string(recKey(dst.data, o))
			if got, want := adSummary(t, c, full), before[key]; got != want {
				t.Fatalf("pass %d key %q changed under a tiny cache\n before %s\n  after %s",
					pass, key, want, got)
			}
		}
	}
}

// scanStrings collects every ad a full Scan yields, rendered to text and sorted, so two scans of the
// same data compare regardless of the map/slice churn between them.
func scanStrings(c *Collection) []string {
	var out []string
	for ad := range c.Scan() {
		out = append(out, ad.String())
	}
	sort.Strings(out)
	return out
}

// TestTinyCacheScanResultsUnchanged covers BOTH stores through the production schema-scan entry
// point (BuildAndEnableSchemaScan builds the sidecar columnar blocks the scan reads through the
// shared cache) and asserts a full Scan returns the SAME ads with a byte-token cache as with a
// generous one. The two stores are tested because the in-memory and persistent columnar paths have
// diverged before, and the block cache backs both.
func TestTinyCacheScanResultsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  func() string
	}{
		{"in-memory", func() string { return "" }},
		{"persistent", func() string { return t.TempDir() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer snapshotBudget(t, mutatingCacheKind)()

			// A generous cache first, to capture the reference result set.
			SetMutatingBlockCacheBudget(defaultMutatingCacheBytes)
			c, _, _ := columnarFixtureIn(t, tc.dir(), 3000)
			defer c.Close()
			c.BuildAndEnableSchemaScan(4096, 8) // build sidecar columnar blocks over the sealed segments
			want := scanStrings(c)
			if len(want) == 0 {
				t.Fatal("scan returned nothing")
			}

			// Now shrink the shared cache below one block and scan again: identical results.
			SetMutatingBlockCacheBudget(4 << 10)
			if c.colCache != nil {
				if got := c.colCache.c.MaxCost(); got != 4<<10 {
					t.Fatalf("tiny budget not in effect: MaxCost %d", got)
				}
			}
			got := scanStrings(c)
			if len(got) != len(want) {
				t.Fatalf("record count changed under a tiny cache: got %d, want %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("ad %d changed under a tiny cache:\n large %s\n small %s", i, want[i], got[i])
				}
			}
		})
	}
}
