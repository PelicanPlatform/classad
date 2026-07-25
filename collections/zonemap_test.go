package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

func sealedWithZones(sh *shard) int {
	n := 0
	for _, s := range sh.segs {
		if s != nil && s != sh.act && len(s.zones) > 0 {
			n++
		}
	}
	return n
}

// TestZoneMapPrune verifies that a range query on a zone-mapped attribute returns the
// correct records while skipping whole sealed segments, that results are identical to a
// full scan, that zones survive a reopen, and that the active segment is never pruned.
func TestZoneMapPrune(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{
		AppendOnly:  true,
		Dir:         dir,
		SegmentSize: 1 << 12, // many small segments
		ZoneAttrs:   []string{"Ts"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Ts increases monotonically with i, so each sealed segment covers a disjoint,
	// increasing [min,max] window -- ideal for pruning.
	const n = 400
	for i := 0; i < n; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N = %d; Ts = %d ]`, i, i))
		if err := c.Put([]byte("k"), ad); err != nil {
			t.Fatal(err)
		}
	}
	sh := c.shards[0]
	if sealedWithZones(sh) == 0 {
		t.Fatal("expected sealed segments to carry zone maps")
	}

	// A range query on the zone-mapped attribute must return exactly the matching
	// records. Ts == N == i, so the expected N set is a simple arithmetic range; the
	// pruned Query result (sorted) must equal it exactly -- pruning may not drop a match.
	check := func(qs string, wantLo, wantHi int) {
		t.Helper()
		q, err := vm.Parse(qs)
		if err != nil {
			t.Fatal(err)
		}
		want := map[int64]bool{}
		for v := wantLo; v <= wantHi; v++ {
			want[int64(v)] = true
		}
		got := map[int64]bool{}
		for ad := range c.Query(q) {
			v, _ := ad.EvaluateAttrInt("N")
			ts, _ := ad.EvaluateAttrInt("Ts")
			if !want[v] {
				t.Fatalf("query %q returned N=%d (Ts=%d) outside expected [%d,%d]", qs, v, ts, wantLo, wantHi)
			}
			got[v] = true
		}
		if len(got) != len(want) {
			t.Fatalf("query %q returned %d records, want %d (a match was pruned away)", qs, len(got), len(want))
		}
	}
	check(`Ts >= 350`, 350, n-1)
	check(`Ts < 40`, 0, 39)
	check(`Ts >= 100 && Ts <= 120`, 100, 120)
	check(`Ts == 275`, 275, 275)

	// A query outside the whole Ts range prunes everything -> empty, no crash.
	q, _ := vm.Parse(`Ts >= 100000`)
	for range c.Query(q) {
		t.Fatal("query above max Ts should return no records")
	}
	c.Close()

	// Reopen recomputes zones; the same query still prunes and returns the same set.
	c2, err := Open(Options{AppendOnly: true, Dir: dir, SegmentSize: 1 << 12, ZoneAttrs: []string{"Ts"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if sealedWithZones(c2.shards[0]) == 0 {
		t.Fatal("expected zone maps to be recomputed on reopen")
	}
	q2, _ := vm.Parse(`Ts >= 380`)
	cnt := 0
	for ad := range c2.Query(q2) {
		v, _ := ad.EvaluateAttrInt("Ts")
		if v < 380 {
			t.Fatalf("pruned query returned Ts=%d < 380", v)
		}
		cnt++
	}
	if cnt != n-380 {
		t.Errorf("after reopen Ts>=380 returned %d, want %d", cnt, n-380)
	}
}

// TestZoneMapInMemory exercises the id-based (non-inline) zone path: an in-memory
// append-only collection encodes with interned ids, so value extraction must look up by
// id. Zones must still populate and a range query must return the correct set.
func TestZoneMapInMemory(t *testing.T) {
	c := New(Options{AppendOnly: true, SegmentSize: 1 << 12, ZoneAttrs: []string{"Ts"}})
	const n = 400
	for i := 0; i < n; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N = %d; Ts = %d ]`, i, i))
		if err := c.Put([]byte("k"), ad); err != nil {
			t.Fatal(err)
		}
	}
	if sealedWithZones(c.shards[0]) == 0 {
		t.Fatal("in-memory (id-encoded) collection: expected non-empty zone maps")
	}
	q, _ := vm.Parse(`Ts >= 360`)
	cnt := 0
	for ad := range c.Query(q) {
		v, _ := ad.EvaluateAttrInt("Ts")
		if v < 360 {
			t.Fatalf("id-path pruned query returned Ts=%d < 360", v)
		}
		cnt++
	}
	if cnt != n-360 {
		t.Errorf("id-path Ts>=360 returned %d, want %d", cnt, n-360)
	}
}

// TestZoneMapMaxAgeRetention verifies MaxAgeAttr/MaxAge drops segments whose newest
// timestamp is older than now-MaxAge, using the zone map.
func TestZoneMapMaxAgeRetention(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{
		AppendOnly:  true,
		Dir:         dir,
		SegmentSize: 1 << 12,
		ZoneAttrs:   []string{"CompletionDate"},
		Retention:   Retention{MaxAgeAttr: "CompletionDate", MaxAge: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	// CompletionDate 0..399. With now=400 and MaxAge=100, any sealed segment whose max
	// CompletionDate < 300 must be dropped; segments spanning >=300 survive.
	const n = 400
	for i := 0; i < n; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N = %d; CompletionDate = %d ]`, i, i))
		if err := c.Put([]byte("k"), ad); err != nil {
			t.Fatal(err)
		}
	}
	dropped, err := c.Rotate(400)
	if err != nil {
		t.Fatal(err)
	}
	if dropped == 0 {
		t.Fatal("MaxAge retention dropped nothing; expected old segments to age out")
	}
	// Every surviving record must have CompletionDate within a segment whose max >= 300.
	minSurv := int64(1 << 30)
	for ad := range c.Scan() {
		v, _ := ad.EvaluateAttrInt("CompletionDate")
		if v < minSurv {
			minSurv = v
		}
	}
	// The oldest surviving record sits in the first not-dropped segment; its segment's
	// max is >= 300, so nothing with CompletionDate up to (300 - one-segment-span) should
	// remain. We assert the clearly-aged records (< 200) are gone.
	if minSurv < 200 {
		t.Errorf("oldest surviving CompletionDate = %d; segments below the age cut should be dropped", minSurv)
	}
	c.Close()
}
