package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// durabilityFixture writes ads whose values exercise the columnar block's awkward corners -- a
// numeric too wide for any fitted slot, so it ESCAPES into the block's cold tail -- and returns the
// collection with its sealed segments columnarized.
func durabilityFixture(t *testing.T, dir string) *Collection {
	t.Helper()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 4000 {
		// A TYPE exception, which is what actually escapes. A merely large integer does not: the
		// schema fits the widest value it sees, so 1<<60 just widens Big's slot to eight bytes. Only a
		// value the slot cannot represent at all is pushed into the cold tail as uvarint(id)+node --
		// and that entry is the only place the payload stores an attribute id inline.
		cid := fmt.Sprintf("%d", i)
		if i%50 == 1 {
			cid = `"cluster-as-text"` // type exception on an attribute whose intern id MOVES
		}
		big := fmt.Sprintf("%d", i%100)
		if i%50 == 0 {
			big = `"not-a-number"`
		}
		// An attribute that appears in a FEW ads, FIRST, and never makes the schema. It consumes a
		// global intern id while these segments are written, and a reopen never interns it again --
		// nothing in the payload names it -- so every id assigned after it shifts. Any id the payload
		// stored that the section does not also store as a NAME is then addressing the wrong
		// attribute. This is the ordinary case of an attribute below the schema's presence floor, not
		// a contrived one.
		text := fmt.Sprintf(`[ ClusterId=%s; ProcId=%d; Owner="user%d"; JobStatus=%d; Big=%s ]`,
			cid, i%10, i%7, i%6, big)
		if i < 3 {
			text = fmt.Sprintf(`[ Aardvark=%d; ClusterId=%s; ProcId=%d; Owner="user%d"; JobStatus=%d; Big=%s ]`,
				i, cid, i%10, i%7, i%6, big)
		}
		ad, err := classad.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.RetrainDict(0)
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		c.Close()
		t.Skip("schema scan did not enable")
	}
	return c
}

// TestColumnarPathSurvivesRestartWithoutFallingBack is the test whose absence let a durable structure
// depend on a process-local id space.
//
// Comparing query ANSWERS across a restart cannot catch that dependency. A columnar block that is
// addressing stale attribute ids does not return wrong answers -- it fails to resolve, defers to the
// record, and the record is authoritative, so the answer stays right and only the speed changes.
// Every such deferral is silent by construction.
//
// So this asserts the absence of the deferral, not the presence of the answer: the same queries over
// the same data must defer no more often after a restart than before it. The global intern table is
// rebuilt at every Open and numbers names in first-seen order, so ids genuinely do move between
// processes -- measured, an attribute that was id 5 came back as id 6 -- and anything durable that
// embedded them would show up right here.
func TestColumnarPathSurvivesRestartWithoutFallingBack(t *testing.T) {
	dir := t.TempDir()
	c := durabilityFixture(t, dir)

	exprs := []string{
		// ClusterId is the interesting one: it is a schema field that ESCAPES on a few records (a
		// string where the slot holds an integer), and its global intern id demonstrably moves across
		// a restart -- measured 3 before and 5 after. Its cold-tail entries were written with the old
		// id, so this is where a payload that stored an id without also storing its NAME would come
		// apart.
		"ClusterId is undefined",
		"ClusterId < 100",
		"Big < 50",   // in-slot values
		"ProcId < 5", // a plain hot column
		"Owner == \"user3\"",
	}
	// The ROW path is authoritative, so each columnar answer is checked against it rather than only
	// against the previous run. A before/after comparison alone is blind to an error that is uniform
	// across both -- which is exactly what happened once: the columnar path reported the escaped
	// values undefined in BOTH processes and the comparison saw nothing wrong.
	rowCount := func(c *Collection, expr string) int { return len(readAll(t, c, expr)) }
	run := func(c *Collection) (counts []int, fallbacks int64) {
		before := ColumnarFallbacks()
		for _, e := range exprs {
			q, err := vm.Parse(e)
			if err != nil {
				t.Fatal(err)
			}
			n, served := c.CountQuery(q)
			if want := rowCount(c, e); served && n != want {
				t.Errorf("%s: columnar says %d, the row path says %d", e, n, want)
			}
			counts = append(counts, n)
		}
		return counts, ColumnarFallbacks() - before
	}

	for _, n := range []string{"Aardvark", "ClusterId", "Big"} {
		id, ok := c.intern.LookupID(n)
		t.Logf("before: %-10s id=%d ok=%v", n, id, ok)
	}
	wantCounts, wantFallbacks := run(c)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	for _, n := range []string{"Aardvark", "ClusterId", "Big"} {
		id, ok := c2.intern.LookupID(n)
		t.Logf("after:  %-10s id=%d ok=%v", n, id, ok)
	}
	gotCounts, gotFallbacks := run(c2)

	for i, e := range exprs {
		if gotCounts[i] != wantCounts[i] {
			t.Errorf("%s: %d after restart, want %d", e, gotCounts[i], wantCounts[i])
		}
	}
	if gotFallbacks > wantFallbacks {
		t.Errorf("the columnar path deferred to the record %d times after a restart and %d before: "+
			"the answers are still correct, which is exactly why this has to be asserted rather than inferred",
			gotFallbacks, wantFallbacks)
	}
	t.Logf("counts %v; fallbacks before=%d after=%d", gotCounts, wantFallbacks, gotFallbacks)
}
