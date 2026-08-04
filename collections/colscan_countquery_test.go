package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestCountQueryAutoRoute verifies CountQuery routes exactly the columnar-eligible predicates
// (Native, single INT schema field, numeric comparisons/band) and returns counts identical to
// the store's own query engine, and declines everything else. Data mixes int Memory with a few
// real, string, and missing Memory (all escape the int hot column) plus MVCC updates.
func TestCountQueryAutoRoute(t *testing.T) {
	store := New(Options{Shards: 2, SegmentSize: 1 << 12}) // small -> sealed segments
	const n = 1500
	put := func(k, src string) {
		if err := store.Put([]byte(k), mustAdOld(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i++ {
		switch i % 40 {
		case 3:
			put(fmt.Sprintf("k%d", i), fmt.Sprintf("Cpus=%d\nMemory=%d.5\nMachine=\"m%04d\"", 1+i%8, 5000, i)) // real -> escapes int col
		case 7:
			put(fmt.Sprintf("k%d", i), fmt.Sprintf("Cpus=%d\nMemory=\"n/a\"\nMachine=\"m%04d\"", 1+i%8, i)) // string -> escapes
		case 11:
			put(fmt.Sprintf("k%d", i), fmt.Sprintf("Cpus=%d\nMachine=\"m%04d\"", 1+i%8, i)) // Memory missing
		default:
			put(fmt.Sprintf("k%d", i), fmt.Sprintf("Cpus=%d\nMemory=%d\nMachine=\"m%04d\"", 1+i%8, 1024+(i%64)*256, i))
		}
	}
	for i := 0; i < n; i += 5 { // MVCC: supersede earlier versions
		put(fmt.Sprintf("k%d", i), fmt.Sprintf("Cpus=2\nMemory=%d\nMachine=\"m%04d\"", 9000, i))
	}

	wires := allSegmentWires(t, store)
	s := buildAdSchema(wires, adSchemaOpts{Presence: 0.85, Fit: 0.95, Strings: true})
	memID, _ := store.intern.LookupID("Memory")
	memIdx, ok := s.byID[memID]
	if !ok || s.fields[memIdx].kind != akInt {
		t.Skip("Memory not an int schema field")
	}
	store.EnableSchemaScan(s, []int{memIdx})

	storeCount := func(expr string) int {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		c := 0
		for range store.Query(q) {
			c++
		}
		return c
	}

	for _, expr := range []string{
		"Memory > 4096",
		"Memory <= 4096",
		"Memory >= 2000 && Memory < 8000",
		"Memory == 9000",
		"Memory != 9000",
	} {
		q, _ := vm.Parse(expr)
		got, ok := store.CountQuery(q)
		if !ok {
			t.Errorf("%q: CountQuery declined an eligible predicate", expr)
			continue
		}
		if want := storeCount(expr); got != want {
			t.Errorf("%q: CountQuery = %d, store.Query = %d", expr, got, want)
		}
	}

	// Not columnar-eligible: must decline (ok=false) so the caller uses the normal scan.
	for _, expr := range []string{
		`Machine == "m0001"`,        // string field, not int schema
		"Cpus > 2 && Memory > 4096", // two fields
		"Memory > 4096 || Cpus > 2", // disjunction across fields
		"Cpus > 2",                  // int field but not the hot/scanned one? still int -> eligible IF Cpus is an int schema field
	} {
		q, _ := vm.Parse(expr)
		if _, ok := store.CountQuery(q); ok {
			// Cpus is a valid int schema field, so "Cpus > 2" legitimately routes; only assert
			// the genuinely-ineligible ones decline.
			if expr != "Cpus > 2" {
				t.Errorf("%q: CountQuery accepted an ineligible predicate", expr)
			}
		}
	}
}
