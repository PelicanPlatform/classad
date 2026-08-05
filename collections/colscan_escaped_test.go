package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestSchemaScanEscapedField drives the escaped-value scan path (escapedNumField): most Memory
// values fit a small int width, but a heavy tail of huge values ESCAPE it and live in the cold
// tail. A threshold placed among the escaped values forces the scan to read those escaped values
// (not just the hot column) -- the columnar count must equal the row truth across the boundary.
func TestSchemaScanEscapedField(t *testing.T) {
	store := New(Options{Shards: 1, SegmentSize: 1 << 12}) // small -> sealed segments
	const n = 2000
	for i := 0; i < n; i++ {
		mem := 1024 + (i%32)*128 // mostly small (fits a narrow width)
		if i%10 == 0 {
			mem = 5_000_000 + i // 10% huge -> escape the fitted width to the cold tail
		}
		ad := mustAdOld(t, fmt.Sprintf("Cpus=%d\nMemory=%d\nDisk=%d", 1+i%8, mem, i))
		if err := store.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 20; i++ {
		mq, _ := vm.Parse("true")
		for range store.QueryProject(mq, []string{"Memory"}) {
		}
	}
	if !store.BuildAndEnableSchemaScan(2000, 4) {
		t.Fatal("BuildAndEnableSchemaScan false")
	}
	// Thresholds: below all escaped (counts them), between, and above all.
	for _, expr := range []string{
		"Memory > 4096",      // small + all escaped
		"Memory > 1000000",   // only the escaped huge ones
		"Memory >= 5000000",  // escaped ones at/above the tail base
		"Memory > 100000000", // above every value -> zero
		"Memory == 5000000",  // exact escaped value (i==0)
	} {
		got, ok := store.CountConstraint(expr)
		if !ok {
			t.Fatalf("%q: CountConstraint declined", expr)
		}
		q, _ := vm.Parse(expr)
		want := 0
		for range store.Query(q) {
			want++
		}
		if got != want {
			t.Fatalf("%q: columnar %d != row truth %d (escaped-field path)", expr, got, want)
		}
	}
}
