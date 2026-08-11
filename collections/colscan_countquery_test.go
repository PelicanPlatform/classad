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

	// A conjunction across SEVERAL numeric fields is columnar-eligible too (see numPredsOnFields):
	// the shape is still `Attr OP literal`, just repeated, so it is served by one narrowing pass per
	// column rather than by a full row scan.
	for _, expr := range []string{
		"Cpus > 2",
		"Cpus > 2 && Memory > 4096",
		"Cpus > 2 && Memory > 4096 && Memory < 9000",
	} {
		q, _ := vm.Parse(expr)
		got, ok := store.CountQuery(q)
		if !ok {
			t.Errorf("%q: CountQuery declined a multi-field conjunction of scalar comparisons", expr)
			continue
		}
		if want := storeCount(expr); got != want {
			t.Errorf("%q: CountQuery = %d, store.Query = %d", expr, got, want)
		}
	}

	// Not columnar-eligible: must decline (ok=false) so the caller uses the normal scan.
	for _, expr := range []string{
		`Machine == "m0001"`,                    // string field, not a numeric schema field
		"Memory > 4096 || Cpus > 2",             // disjunction: the probes do not cover it
		"Memory > Cpus",                         // no literal to compare against
		"Memory > 4096 && Machine == \"m0001\"", // one conjunct on a string field
	} {
		q, _ := vm.Parse(expr)
		if got, ok := store.CountQuery(q); ok {
			// Serving is only a bug if the answer differs; assert the stronger property.
			if want := storeCount(expr); got != want {
				t.Errorf("%q: CountQuery accepted an ineligible predicate AND answered %d (want %d)",
					expr, got, want)
			} else {
				t.Errorf("%q: CountQuery accepted a predicate expected to be ineligible", expr)
			}
		}
	}
}
