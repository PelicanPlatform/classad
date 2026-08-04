package collections

import (
	"fmt"
	"testing"
)

// TestInterningEndToEnd is the phase-1 acceptance test: a persistent collection compacts its
// segments to the INTERNED form (asserted directly via seg.dict), and every read path returns
// correct results over those interned segments -- before AND after a reopen (recovery restores
// the dictionaries). It exercises current + superseded records (updates), multiple sealed
// segments, and multiple shards.
func TestInterningEndToEnd(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const n = 3000
	// Small segments so the data seals into several segments per shard (each compacted to an
	// interned segment), exercising the roll/finalize path.
	c, err := Open(Options{Shards: 2, Dir: dir, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	key := func(i int) []byte { return []byte(fmt.Sprintf("k%05d", i)) }
	// Val = i%100 is stable per key across updates, so a range query has a known count.
	ad := func(i, rev int) string {
		return fmt.Sprintf(`[Id=%d; Val=%d; Name="host-%d.example.org"; Rev=%d; Req=(Val>=50)]`, i, i%100, i, rev)
	}
	for i := 0; i < n; i++ {
		if err := c.Put(key(i), mustAd(t, ad(i, 0))); err != nil {
			t.Fatal(err)
		}
	}
	// Update every key (creates superseded versions -> dead space -> compaction rewrites).
	for i := 0; i < n; i++ {
		if err := c.Put(key(i), mustAd(t, ad(i, 1))); err != nil {
			t.Fatal(err)
		}
	}
	// Force every shard to compact (bypass the dead-ratio threshold) so the transcode runs
	// deterministically regardless of how the data happened to fragment.
	for _, sh := range c.shards {
		c.compactShard(sh, c.currentCodec())
	}
	c.reindexAfterCompaction()

	assertInterned := func(tag string, col *Collection) {
		sealed, interned := 0, 0
		for _, sh := range col.shards {
			sh.mu.RLock()
			act := sh.act
			for _, seg := range sh.segs {
				if seg == nil || seg == act || seg.used == 0 {
					continue
				}
				sealed++
				if seg.dict.Load() != nil {
					interned++
				}
			}
			sh.mu.RUnlock()
		}
		if sealed == 0 {
			t.Fatalf("%s: no sealed segments (test did not seal)", tag)
		}
		if interned == 0 {
			t.Fatalf("%s: %d sealed segments but none interned -- transcode did not run", tag, sealed)
		}
		if interned != sealed {
			t.Fatalf("%s: %d/%d sealed segments interned (expected all after compaction)", tag, interned, sealed)
		}
		t.Logf("%s: %d sealed segments, all interned", tag, sealed)
	}

	// verify checks every read path against the known state (latest rev=1 for every key).
	verify := func(tag string, col *Collection) {
		if got := col.Len(); got != n {
			t.Fatalf("%s: Len=%d want %d", tag, got, n)
		}
		// Point lookups.
		for i := 0; i < n; i++ {
			a, ok := col.Get(key(i))
			if !ok {
				t.Fatalf("%s: Get(%s) missing", tag, key(i))
			}
			if v, _ := a.EvaluateAttrInt("Id"); int(v) != i {
				t.Fatalf("%s: Get(%s) Id=%d", tag, key(i), v)
			}
			if v, _ := a.EvaluateAttrInt("Rev"); v != 1 {
				t.Fatalf("%s: Get(%s) Rev=%d want 1 (stale version?)", tag, key(i), v)
			}
			if s, _ := a.EvaluateAttrString("Name"); s != fmt.Sprintf("host-%d.example.org", i) {
				t.Fatalf("%s: Get(%s) Name=%q", tag, key(i), s)
			}
		}
		// Full scan: exactly n distinct keys.
		seen := map[int]int{}
		for a := range col.Scan() {
			id, _ := a.EvaluateAttrInt("Id")
			seen[int(id)]++
		}
		if len(seen) != n {
			t.Fatalf("%s: Scan distinct=%d want %d", tag, len(seen), n)
		}
		for id, cnt := range seen {
			if cnt != 1 {
				t.Fatalf("%s: Scan yielded id %d %d times", tag, id, cnt)
			}
		}
		// Filtered query: Val>=50 -> ids with i%100 in [50,99] -> exactly half.
		q := mustQuery(t, "Val >= 50")
		cnt := 0
		for range col.Query(q) {
			cnt++
		}
		if cnt != n/2 {
			t.Fatalf("%s: Query(Val>=50)=%d want %d", tag, cnt, n/2)
		}
		// Raw scan path (wireToInline for interned): same n rows.
		raw := 0
		for range col.ScanRaw() {
			raw++
		}
		if raw != n {
			t.Fatalf("%s: ScanRaw=%d want %d", tag, raw, n)
		}
	}

	assertInterned("post-compact", c)
	verify("post-compact", c)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: recovery must restore the dictionaries; every read path must still be correct.
	c2, err := Open(Options{Shards: 2, Dir: dir, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c2.Close()
	assertInterned("post-reopen", c2)
	verify("post-reopen", c2)
}

// TestInterningSchemaScan checks the adschema columnar accelerator over INTERNED segments: the
// per-segment block build must resolve segment-local ids (recordToInternedDict), so a
// CountConstraint routed through the columnar scan matches the query engine.
func TestInterningSchemaScan(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const n = 4000
	c, err := Open(Options{Shards: 2, Dir: dir, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%05d", i)), mustAd(t, fmt.Sprintf("[Id=%d; Val=%d]", i, i%100))); err != nil {
			t.Fatal(err)
		}
	}
	// Read demand on Val so the schema build ranks it into the hot tier.
	tq := mustQuery(t, "true")
	for r := 0; r < 20; r++ {
		for range c.QueryProject(tq, []string{"Val"}) {
		}
	}
	// Force compaction -> interned segments, then enable the columnar scan over them.
	for _, sh := range c.shards {
		c.compactShard(sh, c.currentCodec())
	}
	c.reindexAfterCompaction()
	interned := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil && seg != sh.act && seg.used > 0 && seg.dict.Load() != nil {
				interned++
			}
		}
		sh.mu.RUnlock()
	}
	if interned == 0 {
		t.Fatal("no interned segments to scan")
	}
	if !c.BuildAndEnableSchemaScan(n, 4) {
		t.Fatal("BuildAndEnableSchemaScan returned false over interned segments")
	}
	for _, expr := range []string{"Val >= 50", "Val < 25", "Val >= 10 && Val < 90"} {
		got, ok := c.CountConstraint(expr)
		if !ok {
			t.Errorf("%q: CountConstraint declined over interned segments", expr)
			continue
		}
		want := 0
		for range c.Query(mustQuery(t, expr)) {
			want++
		}
		if got != want {
			t.Errorf("%q: columnar %d != query %d over interned segments", expr, got, want)
		}
	}
}
