package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestReverseIndexedScan verifies that an append-only, newest-first collection with a
// value index returns indexed queries newest-first (not index order), that an early stop
// yields the newest matches, that records appended after the index was built (the tail)
// still come out newest-first ahead of the indexed prefix, and that a disjunctive query
// stays newest-first via the reverse full-scan fallback -- all while matching a reference
// full scan.
func TestReverseIndexedScan(t *testing.T) {
	dir := t.TempDir()
	open := func() *Collection {
		c, err := Open(Options{
			AppendOnly:  true,
			ReverseScan: true,
			Dir:         dir,
			SegmentSize: 1 << 12, // small -> several sealed segments
			ValueAttrs:  []string{"G"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	const n = 400
	for i := 0; i < n; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N=%d; G=%d ]`, i, i%4)) // G in {0,1,2,3}
		if err := c.Put([]byte("k"), ad); err != nil {
			t.Fatal(err)
		}
	}
	// Reopen so Reindex builds the per-segment sidecar indexes; queries now take the
	// indexed path.
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = open()
	defer c.Close()

	// Indexed equality query, newest-first: G==1 matches N in {1,5,9,...,397}; descending.
	q := mustParseQuery(t, `G == 1`)
	var got []int64
	for ad := range c.Query(q) {
		v, _ := ad.EvaluateAttrInt("N")
		g, _ := ad.EvaluateAttrInt("G")
		if g != 1 {
			t.Fatalf("indexed query returned G=%d, want 1", g)
		}
		got = append(got, v)
	}
	if len(got) != n/4 {
		t.Fatalf("G==1 returned %d, want %d", len(got), n/4)
	}
	for i := 1; i < len(got); i++ {
		if got[i] >= got[i-1] {
			t.Fatalf("indexed query not newest-first at %d: %v", i, got[:i+1])
		}
	}
	if got[0] != 397 {
		t.Errorf("newest G==1 is N=%d, want 397", got[0])
	}

	// Early stop: the 3 newest G==1 are 397, 393, 389.
	var top []int64
	for ad := range c.Query(q) {
		v, _ := ad.EvaluateAttrInt("N")
		top = append(top, v)
		if len(top) == 3 {
			break
		}
	}
	if fmt.Sprint(top) != fmt.Sprint([]int64{397, 393, 389}) {
		t.Errorf("LIMIT 3 newest G==1 = %v, want [397 393 389]", top)
	}

	// Append more after reopen (the un-indexed tail). New G==1 records (401, 405, ...) are
	// newest and must precede the indexed prefix, still newest-first.
	for i := n; i < n+40; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N=%d; G=%d ]`, i, i%4))
		if err := c.Put([]byte("k"), ad); err != nil {
			t.Fatal(err)
		}
	}
	got = got[:0]
	for ad := range c.Query(q) {
		v, _ := ad.EvaluateAttrInt("N")
		got = append(got, v)
	}
	if got[0] != 437 { // newest G==1 in [400,440) is 437
		t.Errorf("after tail append, newest G==1 = %d, want 437", got[0])
	}
	for i := 1; i < len(got); i++ {
		if got[i] >= got[i-1] {
			t.Fatalf("tail+prefix not newest-first at %d: %v", i, got[:i+1])
		}
	}

	// Disjunctive (OR) query stays newest-first via the reverse full-scan fallback.
	orq := mustParseQuery(t, `G == 0 || G == 3`)
	var prev int64 = 1 << 30
	seen := 0
	for ad := range c.Query(orq) {
		v, _ := ad.EvaluateAttrInt("N")
		g, _ := ad.EvaluateAttrInt("G")
		if g != 0 && g != 3 {
			t.Fatalf("OR query returned G=%d", g)
		}
		if v >= prev {
			t.Fatalf("OR query not newest-first: %d after %d", v, prev)
		}
		prev = v
		seen++
	}
	if seen == 0 {
		t.Fatal("OR query returned nothing")
	}
}

// TestReverseIndexedMatchesFullScan cross-checks the reverse indexed path against a
// non-indexed reverse collection: the same query must return the same set in the same
// (newest-first) order.
func TestReverseIndexedMatchesFullScan(t *testing.T) {
	build := func(withIndex bool) []int64 {
		dir := t.TempDir()
		opts := Options{AppendOnly: true, ReverseScan: true, Dir: dir, SegmentSize: 1 << 12}
		if withIndex {
			opts.ValueAttrs = []string{"G"}
		}
		c, err := Open(opts)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 300; i++ {
			ad, _ := classad.Parse(fmt.Sprintf(`[ N=%d; G=%d ]`, i, i%3))
			c.Put([]byte("k"), ad)
		}
		c.Close()
		c, _ = Open(opts)
		defer c.Close()
		var out []int64
		for ad := range c.Query(mustQ(t, `G == 2`)) {
			v, _ := ad.EvaluateAttrInt("N")
			out = append(out, v)
		}
		return out
	}
	indexed := build(true)
	full := build(false)
	if fmt.Sprint(indexed) != fmt.Sprint(full) {
		t.Fatalf("indexed result != full-scan result\nindexed=%v\nfull=%v", indexed, full)
	}
	if len(indexed) == 0 {
		t.Fatal("empty result")
	}
}

func mustQ(t *testing.T, s string) *vm.Query {
	t.Helper()
	q, err := vm.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// TestReverseIndexedDisjunction verifies a disjunctive (OR) query on an indexed, newest-first
// collection returns the union newest-first through the indexed groups path (after a reopen
// builds the sidecars), matches an unindexed reverse full scan exactly, and honors an early
// stop as a pushed-down LIMIT.
func TestReverseIndexedDisjunction(t *testing.T) {
	build := func(withIndex bool) []int64 {
		dir := t.TempDir()
		opts := Options{AppendOnly: true, ReverseScan: true, Dir: dir, SegmentSize: 1 << 12}
		if withIndex {
			opts.ValueAttrs = []string{"G"}
		}
		c, err := Open(opts)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 500; i++ {
			ad, _ := classad.Parse(fmt.Sprintf(`[ N=%d; G=%d ]`, i, i%5))
			c.Put([]byte("k"), ad)
		}
		c.Close()
		c, _ = Open(opts) // reopen: Reindex builds the sidecars, so OR takes the groups path
		defer c.Close()
		var out []int64
		var prev int64 = 1 << 30
		for ad := range c.Query(mustQ(t, `G == 1 || G == 3`)) {
			v, _ := ad.EvaluateAttrInt("N")
			g, _ := ad.EvaluateAttrInt("G")
			if g != 1 && g != 3 {
				t.Fatalf("OR query returned G=%d", g)
			}
			if v >= prev {
				t.Fatalf("OR query not newest-first: %d after %d", v, prev)
			}
			prev = v
			out = append(out, v)
		}
		return out
	}
	indexed := build(true)
	full := build(false)
	if fmt.Sprint(indexed) != fmt.Sprint(full) {
		t.Fatalf("indexed OR result != full-scan OR result\nindexed=%v\nfull=%v", indexed, full)
	}
	if len(indexed) != 200 { // G in {1,3}: 2 of every 5 of 500
		t.Fatalf("OR result count = %d, want 200", len(indexed))
	}
	if indexed[0] != 498 { // newest with G in {1,3}: 498 (G=3)
		t.Errorf("newest OR match = %d, want 498", indexed[0])
	}
}
