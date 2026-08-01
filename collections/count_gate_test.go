package collections

import (
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestChainedGateForCountFastPath documents why the COUNT(*) fast path is gated on
// !Chained(): for a chained collection Len() counts the structural (parent-only) cluster ads
// that a match-all query hides, so using Len() as the row count would overcount. For a flat
// collection there are no hidden ads, so Len() is exactly the match-all row count.
func TestChainedGateForCountFastPath(t *testing.T) {
	// Chained: 3 procs + 2 cluster (structural) ads.
	c := chainedJobsCollection(t)
	if !c.Chained() {
		t.Fatal("chained collection reports Chained() = false")
	}
	if c.Len() != 5 {
		t.Fatalf("Len() = %d, want 5 (3 procs + 2 structural clusters)", c.Len())
	}
	matchAll, err := vm.Parse("true")
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for range c.Query(matchAll) { // structural ads are hidden
		rows++
	}
	if rows != 3 {
		t.Fatalf("match-all rows = %d, want 3 (clusters hidden)", rows)
	}
	if c.Len() == rows {
		t.Fatal("expected Len() != visible rows for a chained collection (the fast path must not use Len here)")
	}

	// Flat: no structural ads ⇒ Len() == match-all rows, Chained() false ⇒ fast path valid.
	f := New(Options{})
	f.Put([]byte("a"), mustAd(t, `[X=1]`))
	f.Put([]byte("b"), mustAd(t, `[X=2]`))
	f.Put([]byte("c"), mustAd(t, `[X=3]`))
	if f.Chained() {
		t.Fatal("flat collection reports Chained() = true")
	}
	m := 0
	for range f.Query(matchAll) {
		m++
	}
	if f.Len() != 3 || m != 3 {
		t.Fatalf("flat: Len() = %d, rows = %d, want 3/3", f.Len(), m)
	}
}
