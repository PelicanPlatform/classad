package collections

import (
	"fmt"
	"testing"
)

// widenedCapability runs fn with name temporarily absent from pureFuncs, emulating a build whose
// computed-cell capability was narrower than this one's -- the state every segment columnarized
// before such a capability widened is still in.
func withoutPureFunc(t *testing.T, name string, fn func()) {
	t.Helper()
	if _, ok := pureFuncs[name]; !ok {
		t.Fatalf("pureFuncs has no %q to remove; the test no longer emulates a narrower build", name)
	}
	delete(pureFuncs, name)
	defer func() { pureFuncs[name] = struct{}{} }()
	fn()
}

// computedConvergeAd keeps Need a schema field in every record -- the dominant kind and int width
// are the same either way -- and varies only HOW the value is written: a literal in most records,
// a max({...}) expression in a small minority -- small enough that the literals alone clear the
// presence floor, so Need is a field either way and both values land in the same int width. That is the shape the bug needs. When widening the
// capability also ADDS a field, the derived schema differs and convergence rewrites the segment
// for that reason alone; here the schema is identical before and after, so the only thing that
// changed is whether those expression cells can be computed.
func computedConvergeAd(i int) string {
	if i%20 < 19 { // 95% literal: Need clears the 0.90 presence floor without the computed cells
		return fmt.Sprintf(`[ Key=%d; Base=%d; Need=%d; Pad=%q ]`, i, 100+i%50, 200+i%100, padStr(i))
	}
	return fmt.Sprintf(`[ Key=%d; Base=%d; Need=max({200, (Base * 2)}); Pad=%q ]`, i, 100+i%50, padStr(i))
}

func padStr(i int) string {
	b := make([]byte, 64)
	for j := range b {
		b[j] = byte('a' + (i+j)%26)
	}
	return string(b)
}

// A segment columnarized while the computed-cell capability was narrower keeps escaped cells the
// current capability could compute. Convergence has to notice and rewrite it: otherwise widening
// what can be computed only ever reaches data written after the change, and history already on
// disk stays on the per-record path forever.
func TestConvergenceRebuildsWidenedComputedCells(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	opts := Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1,
		ColumnarSegmentBudget: 1 << 20}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	const n = 3000
	for i := range n {
		tx := c.Begin()
		tx.SetDurable(false)
		tx.Put([]byte(fmt.Sprintf("k%05d", i)), mustAd(t, computedConvergeAd(i)))
		if r := tx.Commit(); r.Conflicted() {
			t.Fatal("conflict")
		}
	}

	var narrowSchema *adSchema
	withoutPureFunc(t, "max", func() {
		if !c.BuildAndEnableSchemaScan(4096, 8) {
			t.Fatal("schema scan did not enable")
		}
		for range 20 { // columnarize everything under the narrow capability
			c.BuildAndEnableSchemaScan(4096, 8)
		}
		narrowSchema = c.schemaScan.Load().schema
	})

	// The capability is now wide again. The derived schema must be unchanged, or the test is
	// measuring an ordinary schema change instead of the gap it is for.
	wide, _, ok := c.deriveSchema(4096, 8)
	if !ok {
		t.Fatal("deriveSchema failed")
	}
	if !sameSchemaLayout(narrowSchema, wide) {
		t.Fatal("widening the capability changed the schema layout; this corpus tests the wrong path")
	}

	// staleSegmentCount deliberately does NOT count this category: answering it costs a read of
	// the segment, and that call runs over every segment on a reporting path. Convergence asks the
	// expensive question itself, for a bounded number of segments per pass -- so the assertion is
	// that the cells get built, not that the count moves.
	before := computedCellsBuilt.Load()
	for pass := 0; pass < 64; pass++ {
		c.BuildAndEnableSchemaScan(4096, 8)
		if computedCellsBuilt.Load() > before {
			break
		}
	}
	built := computedCellsBuilt.Load() - before
	if built == 0 {
		t.Fatal("convergence never built a computed cell after the capability widened: the widened " +
			"capability reaches only segments columnarized after it, never data already on disk")
	}
	t.Logf("convergence built %d computed cells", built)

	// And it settles: once the segments carry their computed cells, further passes rewrite nothing.
	// A staleness rule that stayed true after its own rewrite would rewrite the archive forever.
	for pass := range 8 {
		c.BuildAndEnableSchemaScan(4096, 8)
		_ = pass
	}
	settled := computedCellsBuilt.Load()
	for range 4 {
		c.BuildAndEnableSchemaScan(4096, 8)
	}
	if n := computedCellsBuilt.Load() - settled; n != 0 {
		t.Fatalf("convergence kept rewriting after it was done: %d more computed cells over 4 idle passes", n)
	}
}
