package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// The sample a decision is made from has to resemble the data the decision is applied to.
//
// Four maintenance decisions sample the collection: the columnar schema (deriveSchema), the hot set
// (RefreshHotSet), the fit report (SchemaFit) and index profiling (profileAttrs). All four took the
// ARENA PREFIX -- for an append-only table, the oldest records in it. The dictionary was the visible
// symptom, but the schema is the expensive one: it picks each int column's fixed WIDTH, and a value
// too wide for its slot escapes to the cold tail, which is the slow path row groups exist to bound.
//
// So a table whose values grew over time derived a narrow width from its ancient records and then
// escaped on everything current. These tests pin the fix by making old and new data differ in
// exactly the way that matters.

// growingFixture builds a table whose ProcId values are small in the OLD half and large in the NEW
// half -- the shape that punishes a prefix sample. Returns the collection and ProcId's intern id.
func growingFixture(tb testing.TB, n int) (*Collection, uint32) {
	tb.Helper()
	c := New(Options{Shards: 1, SegmentSize: 1 << 16})
	for i := 0; i < n; i++ {
		proc := i % 10 // fits one byte
		if i >= n/2 {
			proc = 1000000 + i // needs four
		}
		ad := mustAdOld(tb, fmt.Sprintf("ClusterId = %d\nProcId = %d\nOwner = \"u%d\"\nCmd = \"/bin/j%d\"",
			i/10, proc, i%32, i))
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			tb.Fatal(err)
		}
	}
	// Drive demand so ProcId is a hot column, as a real query workload would.
	q, err := vm.Parse("ProcId >= 0")
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		for range c.Query(q) {
		}
	}
	id, ok := c.intern.LookupID("ProcId")
	if !ok {
		tb.Fatal("ProcId not interned")
	}
	return c, id
}

// widthOf returns the width the schema assigned to a field, or 0 if absent.
func widthOf(s *adSchema, id uint32) int {
	if idx, ok := s.byID[id]; ok {
		return s.fields[idx].width
	}
	return 0
}

// TestDerivedSchemaFollowsRecentData is the payoff: the derived int width must cover the values the
// table is CURRENTLY receiving, not the ones it received first.
func TestDerivedSchemaFollowsRecentData(t *testing.T) {
	c, procID := growingFixture(t, 4000)
	defer c.Close()

	// What the prefix sampler would have decided, for contrast. Built the same way deriveSchema
	// builds it, from CollectSamples instead of the recent draw.
	prefix := c.CollectSamples(2000)
	if len(prefix) == 0 {
		t.Fatal("no prefix samples")
	}
	prefixSchema := buildAdSchema(prefix, adSchemaOpts{Presence: 0.90, Fit: 0.95, Strings: true})
	prefixWidth := widthOf(prefixSchema, procID)

	// What the collection actually derives now.
	s, hot, ok := c.deriveSchema(2000, 4)
	if !ok {
		t.Fatal("deriveSchema declined")
	}
	gotWidth := widthOf(s, procID)
	if gotWidth == 0 {
		t.Fatal("ProcId is not a field of the derived schema")
	}
	isHot := false
	for _, i := range hot {
		if s.fields[i].id == procID {
			isHot = true
		}
	}
	t.Logf("ProcId width: prefix sample would pick %d, recent sample picks %d (hot=%v)",
		prefixWidth, gotWidth, isHot)

	// The recent values need four bytes. A width that cannot hold them means every current record
	// escapes on the column queries read most.
	if gotWidth < 4 {
		t.Errorf("derived width %d cannot hold current ProcId values (~1e6); recent records would "+
			"all escape to the cold tail", gotWidth)
	}
	// And confirm the premise: the prefix sample really does pick a narrower width, so this test is
	// measuring the fix rather than passing for an unrelated reason.
	if prefixWidth >= gotWidth {
		t.Errorf("prefix sample picked width %d, not narrower than the recent sample's %d -- the "+
			"fixture no longer distinguishes the two samplers, so this test proves nothing",
			prefixWidth, gotWidth)
	}
}

// TestSchemaFitReportsRecentData covers the diagnostic: `.schema fit` exists to tell an operator
// whether the schema still matches the data. Sampling the oldest records made it report on the wrong
// end of the table, so a schema that had drifted badly could still look like a clean fit.
func TestSchemaFitReportsRecentData(t *testing.T) {
	c, procID := growingFixture(t, 4000)
	defer c.Close()

	// Enable the accelerator under a schema derived from the OLD data only, so ProcId's slot is
	// deliberately too narrow for current records. That is the drift the report must surface.
	oldSchema := buildAdSchema(c.CollectSamples(500), adSchemaOpts{Presence: 0.90, Fit: 0.95, Strings: true})
	if widthOf(oldSchema, procID) >= 4 {
		t.Skip("prefix schema already wide enough; fixture cannot create drift")
	}
	c.EnableSchemaScan(oldSchema, nil)

	fits, sampled := c.SchemaFit(2000)
	if sampled == 0 || len(fits) == 0 {
		t.Fatal("SchemaFit returned nothing")
	}
	name, ok := c.intern.Name(procID)
	if !ok {
		t.Fatal("no name for ProcId")
	}
	var got *SchemaFieldFit
	for i := range fits {
		if fits[i].Name == name {
			got = &fits[i]
		}
	}
	if got == nil {
		t.Fatalf("ProcId missing from the fit report (%d fields)", len(fits))
	}
	unstorable := got.Escaped - got.Missing
	t.Logf("ProcId fit: escaped %.1f%%, missing %.1f%%, unstorable %.1f%%",
		got.Escaped*100, got.Missing*100, unstorable*100)
	// Half the table cannot fit the slot. Sampled from recent records the report must show a large
	// unstorable fraction; sampled from the prefix it would show ~none.
	if unstorable < 0.25 {
		t.Errorf("fit reports only %.1f%% unstorable for a column that half the table overflows; "+
			"the report is sampling the wrong end of the table", unstorable*100)
	}
}
