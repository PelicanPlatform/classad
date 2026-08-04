package collections

import (
	"fmt"
	"testing"
)

// TestAppendOnlyInterning is the acceptance test for interning append-only (archive)
// collections: RetrainDict reseals each segment INTERNED (asserted via seg.dict), and reads --
// full scan, filtered query, and zone-pruned range query -- stay correct over those interned
// segments, before and after a reopen (recovery restores the dicts and recomputes zone maps
// dict-aware). Ts is monotonic so its per-segment [min,max] zones are disjoint and a range
// query genuinely prunes segments; a wrong (dict-blind) zone extraction would mis-prune and
// change the count.
func TestAppendOnlyInterning(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const n = 4000
	// AppendOnly + a value-indexed attr (Ts) -> zone maps; small segments -> several sealed.
	c, err := Open(Options{AppendOnly: true, Dir: dir, SegmentSize: 1 << 16, ValueAttrs: []string{"Ts"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%05d", i)),
			mustAd(t, fmt.Sprintf("[Id=%d; Ts=%d; Val=%d; Host=\"h%d.example.org\"]", i, i, i%100, i))); err != nil {
			t.Fatal(err)
		}
	}

	// Retrain the dictionary -> reseal every segment (now interned + recompressed).
	if _, err := c.RetrainDict(n); err != nil {
		t.Fatalf("RetrainDict: %v", err)
	}

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
		if sealed == 0 || interned != sealed {
			t.Fatalf("%s: %d/%d sealed segments interned (want all)", tag, interned, sealed)
		}
		t.Logf("%s: %d sealed segments, all interned", tag, sealed)
	}

	verify := func(tag string, col *Collection) {
		// Full scan: exactly n records (append-only keeps every version; keys are unique here).
		total := 0
		ids := map[int]int{}
		for a := range col.Scan() {
			total++
			id, _ := a.EvaluateAttrInt("Id")
			ids[int(id)]++
		}
		if total != n || len(ids) != n {
			t.Fatalf("%s: scan total=%d distinct=%d want %d", tag, total, len(ids), n)
		}
		// Filtered query (Val>=50 -> half).
		cnt := 0
		for range col.Query(mustQuery(t, "Val >= 50")) {
			cnt++
		}
		if cnt != n/2 {
			t.Fatalf("%s: Query(Val>=50)=%d want %d", tag, cnt, n/2)
		}
		// Zone-pruned range query on the monotonic Ts: Ts >= 3000 -> ids [3000,3999] = 1000.
		zc := 0
		for range col.Query(mustQuery(t, "Ts >= 3000")) {
			zc++
		}
		if zc != n-3000 {
			t.Fatalf("%s: Query(Ts>=3000)=%d want %d (zone pruning mis-extracted?)", tag, zc, n-3000)
		}
	}

	assertInterned("post-retrain", c)
	verify("post-retrain", c)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2, err := Open(Options{AppendOnly: true, Dir: dir, SegmentSize: 1 << 16, ValueAttrs: []string{"Ts"}})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c2.Close()
	assertInterned("post-reopen", c2)
	verify("post-reopen", c2)
}
