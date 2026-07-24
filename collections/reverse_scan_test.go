package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestReverseScan verifies ReverseScan yields records newest-first across both
// segment boundaries and within a segment, that an early stop returns the K most
// recently appended, that a filtered Query preserves the order, and that it
// survives a persistent reopen.
func TestReverseScan(t *testing.T) {
	dir := t.TempDir()
	// Small segments so the 200 records span several, exercising cross-segment order.
	c, err := Open(Options{AppendOnly: true, ReverseScan: true, Dir: dir, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	const n = 200
	for i := 0; i < n; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N = %d; Even = %v ]`, i, i%2 == 0))
		if err := c.Put([]byte("k"), ad); err != nil {
			t.Fatal(err)
		}
	}

	// Full reverse scan: N should count down n-1 .. 0.
	want := n - 1
	seen := 0
	for ad := range c.Scan() {
		v, _ := ad.EvaluateAttrInt("N")
		if v != int64(want) {
			t.Fatalf("reverse scan order: got N=%d, want %d", v, want)
		}
		want--
		seen++
	}
	if seen != n {
		t.Fatalf("reverse scan saw %d records, want %d", seen, n)
	}

	// Early stop (pushed-down LIMIT 5) => the 5 newest: 199,198,197,196,195.
	var top []int64
	for ad := range c.Scan() {
		v, _ := ad.EvaluateAttrInt("N")
		top = append(top, v)
		if len(top) == 5 {
			break
		}
	}
	wantTop := []int64{199, 198, 197, 196, 195}
	if fmt.Sprint(top) != fmt.Sprint(wantTop) {
		t.Fatalf("LIMIT 5 newest = %v, want %v", top, wantTop)
	}

	// Filtered Query also newest-first: even N descending 198,196,...
	q, err := vm.Parse(`Even == true`)
	if err != nil {
		t.Fatal(err)
	}
	prev := int64(n)
	cnt := 0
	for ad := range c.Query(q) {
		v, _ := ad.EvaluateAttrInt("N")
		if v%2 != 0 {
			t.Fatalf("query returned odd N=%d", v)
		}
		if v >= prev {
			t.Fatalf("query not descending: %d after %d", v, prev)
		}
		prev = v
		cnt++
	}
	if cnt != n/2 {
		t.Fatalf("even query returned %d, want %d", cnt, n/2)
	}
	c.Close()

	// Reopen: reverse order and count survive.
	c2, err := Open(Options{AppendOnly: true, ReverseScan: true, Dir: dir, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	want = n - 1
	seen = 0
	for ad := range c2.Scan() {
		v, _ := ad.EvaluateAttrInt("N")
		if v != int64(want) {
			t.Fatalf("after reopen reverse order: got N=%d, want %d", v, want)
		}
		want--
		seen++
	}
	if seen != n {
		t.Fatalf("after reopen reverse scan saw %d, want %d", seen, n)
	}
}
