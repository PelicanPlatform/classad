package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestBuildAndEnableSchemaScanFromDemand closes the loop: read demand on Memory makes it the
// hot tier, and a string constraint auto-routes to the columnar scan with a count equal to the
// store's own query engine.
func TestBuildAndEnableSchemaScanFromDemand(t *testing.T) {
	store := New(Options{Shards: 1, SegmentSize: 1 << 12}) // small -> sealed segments
	const n = 1000
	for i := 0; i < n; i++ {
		ad := mustAdOld(t, fmt.Sprintf("Cpus=%d\nMemory=%d\nDisk=%d\nMachine=\"m%04d\"", 1+i%8, 1024+(i%64)*256, i*4096, i))
		if err := store.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	// Build read demand on Memory (projected reads bump recordReads).
	mq, _ := vm.Parse("true")
	for i := 0; i < 20; i++ {
		for range store.QueryProject(mq, []string{"Memory"}) {
		}
	}

	if !store.BuildAndEnableSchemaScan(2000, 4) {
		t.Fatal("BuildAndEnableSchemaScan returned false")
	}
	st := store.schemaScan.Load()
	if st == nil {
		t.Fatal("schema scan not enabled")
	}
	// Memory (the read-demanded attr) should be in the hot tier.
	memID, _ := store.intern.LookupID("Memory")
	memIdx := st.schema.byID[memID]
	hot := false
	for _, i := range st.hot {
		if i == memIdx {
			hot = true
		}
	}
	if !hot {
		t.Errorf("Memory (read-demanded) not selected into the hot tier %v", st.hot)
	}

	for _, expr := range []string{"Memory > 4096", "Memory <= 4096", "Memory >= 2000 && Memory < 9000"} {
		got, ok := store.CountConstraint(expr)
		if !ok {
			t.Errorf("%q: CountConstraint declined", expr)
			continue
		}
		q, _ := vm.Parse(expr)
		want := 0
		for range store.Query(q) {
			want++
		}
		if got != want {
			t.Errorf("%q: columnar %d != store.Query %d", expr, got, want)
		}
	}
}
