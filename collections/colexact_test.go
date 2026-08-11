package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// The columnar aggregate paths answer from the query's PROBES and never re-verify a record against
// the query. Probes is an over-approximation built for index pruning -- it omits any conjunct that is
// not `Attr OP literal` -- so answering from it silently over-counts.
//
// The trap is that Native() looks like the guard for this and is not. An attribute-to-attribute
// comparison compiles natively and is omitted from Probes, so `ProcId >= 5 && ClusterId != ProcId`
// passed every gate and returned the count of `ProcId >= 5`: 1500 where the answer is 0.

// exactFixture makes ClusterId == ProcId for every record, so `ClusterId != ProcId` excludes
// everything and any path that ignores that conjunct is caught by a maximal margin.
func exactFixture(t *testing.T, n int) *Collection {
	t.Helper()
	c := New(Options{Shards: 1, SegmentSize: 1 << 16})
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAdOld(t, fmt.Sprintf("ClusterId = %d\nProcId = %d\nOwner = \"u%d\"\nMemory = %d",
				i%10, i%10, i%32, 1024+(i%16)*512))); err != nil {
			t.Fatal(err)
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 4) {
		t.Skip("no sealed segments")
	}
	return c
}

// TestColumnarCountExactness is the regression: every query the columnar path AGREES to serve must
// produce the row path's answer.
func TestColumnarCountExactness(t *testing.T) {
	c := exactFixture(t, 3000)
	defer c.Close()

	for _, expr := range []string{
		"ProcId >= 5",                        // pure, servable
		"ProcId >= 5 && ProcId <= 8",         // conjunction on one field, servable
		"ProcId >= 5 && ClusterId != ProcId", // attr-to-attr conjunct: NATIVE but omitted from Probes
		"ProcId >= 5 && Memory > ProcId",     // arithmetic-free attr-to-attr, same hole
		"ProcId >= 5 && Owner == \"u3\"",     // two fields: declined for a different reason
		"ProcId >= 5 || ProcId < 2",          // disjunction: not a conjunction at all
	} {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		for range c.Query(q) {
			want++
		}
		got, served := c.CountQuery(q)
		t.Logf("%-40s served=%-5v columnar=%-6d row=%d", expr, served, got, want)
		if served && got != want {
			t.Errorf("%s: columnar answered %d, row path says %d -- the columnar path served a "+
				"query whose probes do not cover it", expr, got, want)
		}
	}
}

// TestColumnarStatsExactness covers the same hole in MIN/MAX/SUM, which share numPredOnField.
func TestColumnarStatsExactness(t *testing.T) {
	c := exactFixture(t, 3000)
	defer c.Close()

	q, err := vm.Parse("ProcId >= 5 && ClusterId != ProcId")
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := c.NumStatsQuery(q, "ProcId"); ok {
		t.Errorf("NumStatsQuery served a query with an omitted conjunct (n=%d, max=%v); no record "+
			"matches it, so any aggregate it returns is wrong", st.N, st.Max)
	}
}

// TestExactProbesContract pins the vm-level distinction directly, so the guarantee is testable
// without going through a store.
func TestExactProbesContract(t *testing.T) {
	for _, tc := range []struct {
		expr      string
		wantExact bool
	}{
		{"ProcId >= 5", true},
		{"ProcId >= 5 && ProcId <= 8", true},
		{"ProcId >= 5 && Owner == \"u3\"", true},      // both conjuncts recognized
		{"ProcId >= 5 && ClusterId != ProcId", false}, // attr-to-attr: omitted
		{"ProcId >= 5 && Memory > ProcId", false},     // attr-to-attr: omitted
		{"ProcId >= 5 || ProcId < 2", false},          // the whole expression is one unrecognized conjunct
	} {
		q, err := vm.Parse(tc.expr)
		if err != nil {
			t.Fatal(err)
		}
		probes, exact := q.ExactProbes()
		if exact != tc.wantExact {
			t.Errorf("%-38s exact=%v want %v (probes=%d, Probes()=%d)",
				tc.expr, exact, tc.wantExact, len(probes), len(q.Probes()))
		}
		// Whatever exactness says, the probes must remain a sound candidate filter: never more
		// probes than Probes() would give.
		if len(probes) > len(q.Probes()) {
			t.Errorf("%s: ExactProbes returned more probes (%d) than Probes (%d)",
				tc.expr, len(probes), len(q.Probes()))
		}
	}
}
