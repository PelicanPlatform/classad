package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestZoneMapPruneWirePath is the third instance of the bug TestZoneMapPruneProjectedPaths
// covers: a scan path that extracts probes for index planning but never hands them to the
// query plan at all, so sealed segments it could skip wholesale are walked instead.
// QueryProject and QueryRawProjected were fixed; QueryRawWire was not.
//
// It is kept separate from that test because the wire path needs a PERSISTENT collection
// (QueryRawWire returns nothing unless records carry inline names), which the in-memory
// fixture there cannot provide.
func TestZoneMapPruneWirePath(t *testing.T) {
	c, err := Open(Options{AppendOnly: true, Dir: t.TempDir(), SegmentSize: 1 << 12, ZoneAttrs: []string{"Ts"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const n = 400
	for i := 0; i < n; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N = %d; Ts = %d ]`, i, i))
		if err := c.Put([]byte("k"), ad); err != nil {
			t.Fatal(err)
		}
	}
	if sealedWithZones(c.shards[0]) == 0 {
		t.Fatal("expected sealed segments to carry zone maps")
	}

	// A predicate above the whole Ts range matches nothing, so every sealed segment is
	// prunable. Measured as segments rather than decompressions because the stats variant
	// reports pruning directly.
	q, _ := vm.Parse(`Ts >= 100000`)
	var st ScanStats
	rows := 0
	for range c.QueryRawWireStats(q, []string{"N"}, false, &st) {
		rows++
	}
	if rows != 0 {
		t.Fatalf("QueryRawWire(Ts>=100000) returned %d rows, want 0", rows)
	}
	if st.SegmentsPruned == 0 {
		t.Fatalf("QueryRawWire pruned no segments for an out-of-range predicate "+
			"(total=%d scanned=%d); the wire path is not passing zone probes",
			st.SegmentsTotal, st.SegmentsScanned)
	}

	// Pruning must never drop a match: an in-range predicate returns every matching record.
	q2, _ := vm.Parse(`Ts >= 380`)
	got := 0
	for range c.QueryRawWire(q2, []string{"Ts"}, false) {
		got++
	}
	if got != n-380 {
		t.Errorf("QueryRawWire(Ts>=380) returned %d rows, want %d", got, n-380)
	}
}
