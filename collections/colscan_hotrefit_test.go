package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestRefitHotIntWidths unit-tests the re-fit helper: two fields share a distribution (mostly
// small, a ~2% tail that overflows 2 bytes), but only one is in the hot set. The hot field must be
// widened to cover its tail (no escape); the cold field must keep the tight base width.
func TestRefitHotIntWidths(t *testing.T) {
	c := New(Options{Shards: 1})
	var wires [][]byte
	for i := 0; i < 3000; i++ {
		v := 1024 + (i%64)*256 // fits 2 bytes
		if i%50 == 0 {
			v = 5_000_000 + i // ~2% tail, overflows 2 bytes
		}
		wires = append(wires, c.encodeAd(mustAdOld(t,
			fmt.Sprintf("Cpus=%d\nMem=%d\nCold=%d", 1+i%8, v, v)).AST()))
	}
	s := buildAdSchema(wires, adSchemaOpts{Presence: 0.9, Fit: 0.95, Strings: true})
	memID, _ := c.intern.LookupID("Mem")
	coldID, _ := c.intern.LookupID("Cold")
	// Precondition: at the base Fit both land at the narrow (escaping) width.
	if w := s.fields[s.byID[memID]].width; w != 2 {
		t.Fatalf("precondition: Mem base width %d, want 2", w)
	}
	// Re-fit with only Mem hot.
	s2, hot2 := refitHotIntWidths(s, wires, []int{s.byID[memID]}, hotIntFit)
	memF := s2.fields[s2.byID[memID]]
	coldF := s2.fields[s2.byID[coldID]]
	if !intFits(5_000_000+3000, memF.width, memF.unsigned) {
		t.Fatalf("hot Mem width=%d still escapes its tail", memF.width)
	}
	if intFits(5_000_000, coldF.width, coldF.unsigned) {
		t.Fatalf("cold width=%d widened (should stay narrow)", coldF.width)
	}
	if memF.width <= coldF.width {
		t.Fatalf("hot Mem width %d not wider than cold %d", memF.width, coldF.width)
	}
	// hot2 must point at Mem in the re-laid-out schema.
	if len(hot2) != 1 || s2.fields[hot2[0]].id != memID {
		t.Fatalf("hot2 does not map to Mem after re-layout: %v", hot2)
	}
	t.Logf("hot Mem=%dB (covers tail), cold=%dB (escapes)", memF.width, coldF.width)
}

// TestHotTierRefitEndToEnd drives query demand on Mem (a constraint that evaluates it), so
// BuildAndEnableSchemaScan makes Mem hot and re-fits it; the columnar count stays correct across
// the tail.
func TestHotTierRefitEndToEnd(t *testing.T) {
	store := New(Options{Shards: 1, SegmentSize: 1 << 12})
	const n = 3000
	for i := 0; i < n; i++ {
		v := 1024 + (i%64)*256
		if i%50 == 0 {
			v = 5_000_000 + i
		}
		if err := store.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAdOld(t, fmt.Sprintf("Cpus=%d\nMem=%d\nCold=%d", 1+i%8, v, v))); err != nil {
			t.Fatal(err)
		}
	}
	// Evaluate Mem (recordReads bumps its demand) -> Mem becomes the hot field.
	mc, _ := vm.Parse("Mem >= 0")
	for i := 0; i < 30; i++ {
		for range store.Query(mc) {
		}
	}
	if !store.BuildAndEnableSchemaScan(2000, 1) {
		t.Fatal("BuildAndEnableSchemaScan false")
	}
	st := store.schemaScan.Load()
	memID, _ := store.intern.LookupID("Mem")
	if len(st.hot) == 0 || st.schema.fields[st.hot[0]].id != memID {
		got := "?"
		if len(st.hot) > 0 {
			got, _ = store.intern.Name(st.schema.fields[st.hot[0]].id)
		}
		t.Fatalf("Mem is not the hot field (got %q) -- demand not driven", got)
	}
	memF := st.schema.fields[st.schema.byID[memID]]
	if !intFits(5_000_000+n, memF.width, memF.unsigned) {
		t.Fatalf("hot Mem width=%d not widened to cover its tail", memF.width)
	}
	for _, expr := range []string{"Mem > 4096", "Mem > 1000000", "Mem >= 5000000"} {
		got, ok := store.CountConstraint(expr)
		if !ok {
			t.Fatalf("%q declined", expr)
		}
		q, _ := vm.Parse(expr)
		want := 0
		for range store.Query(q) {
			want++
		}
		if got != want {
			t.Fatalf("%q: columnar %d != row %d", expr, got, want)
		}
	}
}
