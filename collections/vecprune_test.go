package collections

import (
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// The zone-prunable probe analysis depends only on the query and the schema, and a scan visits one window
// per SEGMENT -- 162 on a 60k-record table -- which all share a single schema object. Recomputing it per
// window repeated identical work 161 times: 11% of a scan's time and 44% of its allocations, for an answer
// that never changed.
//
// This holds that line for both scans. It counts the analyses rather than timing them, because the failure
// mode is invisible: an un-hoisted analysis is still CORRECT, and only shows up as a query that got slower
// on a table with many segments -- which is precisely why the cost went unnoticed until the scan was
// attributed phase by phase.
func TestZonePruneAnalyzedOncePerQuery(t *testing.T) {
	c := scopeFixtureCodec(t, 60000)
	defer c.Close()

	segments := 0
	for _, sh := range c.shards {
		_, ws := sh.snapshot()
		for _, w := range ws {
			if seg := w.seg.colblk.Load(); seg != nil && seg.schema() != nil {
				segments++
			}
		}
		releaseWindows(ws)
	}
	if segments < 10 {
		t.Fatalf("fixture has only %d columnar segments; with so few, a per-window analysis would cost "+
			"nothing and this test would pass without testing anything", segments)
	}

	for _, tc := range []struct {
		name string
		run  func(*vm.Query)
	}{
		{"VectorEvalCount", func(q *vm.Query) { c.VectorEvalCount(q) }},
		{"ColumnarEvalCount", func(q *vm.Query) { c.ColumnarEvalCount(q) }},
	} {
		for _, expr := range []string{
			"RequestMemory > 4096",
			"RequestMemory > 4096 && RequestCpus >= 4",
			`Owner == "user3"`, // no prunable probe at all: must still analyze only once
		} {
			q, err := vm.Parse(expr)
			if err != nil {
				t.Fatal(err)
			}
			tc.run(q) // warm any lazily built state
			before := zonePruneAnalyses.Load()
			tc.run(q)
			got := zonePruneAnalyses.Load() - before
			if got > 1 {
				t.Errorf("%s(%q): analyzed the probes %d times across %d segments; it depends only on the "+
					"query and the schema, so once is enough", tc.name, expr, got, segments)
			}
		}
	}
}

// TestPrunePlanRecomputesOnSchemaChange checks the cache is keyed on the schema and not merely on being
// warm: segments sealed under different schemas coexist, and answering one segment's question with
// another's analysis would prune the wrong blocks.
func TestPrunePlanRecomputesOnSchemaChange(t *testing.T) {
	c := scopeFixtureCodec(t, 20000)
	defer c.Close()
	q, err := vm.Parse("RequestMemory > 4096")
	if err != nil {
		t.Fatal(err)
	}
	var sch *adSchema
	for _, sh := range c.shards {
		_, ws := sh.snapshot()
		for _, w := range ws {
			if seg := w.seg.colblk.Load(); seg != nil && seg.schema() != nil {
				sch = seg.schema()
			}
		}
		releaseWindows(ws)
	}
	if sch == nil {
		t.Fatal("no schema")
	}
	other := &adSchema{byID: map[uint32]int{}}

	var plan prunePlan
	before := zonePruneAnalyses.Load()
	plan.tests(c, q, sch)
	plan.tests(c, q, sch) // same schema: cached
	if n := zonePruneAnalyses.Load() - before; n != 1 {
		t.Errorf("same schema twice ran %d analyses, want 1", n)
	}
	before = zonePruneAnalyses.Load()
	plan.tests(c, q, other)
	if n := zonePruneAnalyses.Load() - before; n != 1 {
		t.Errorf("a different schema ran %d analyses, want 1", n)
	}
	// And back, which must not silently keep the other schema's answer.
	before = zonePruneAnalyses.Load()
	idxs, _ := plan.tests(c, q, sch)
	if n := zonePruneAnalyses.Load() - before; n != 1 {
		t.Errorf("returning to the first schema ran %d analyses, want 1", n)
	}
	if len(idxs) == 0 {
		t.Error("the first schema's analysis found no prunable probe for a numeric range on a numeric field")
	}
}
