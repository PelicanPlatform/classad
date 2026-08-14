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

// TestGroupColumnsSurviveRestart checks that group columns answer the same before and after a
// restart, and do not start deferring to the record.
//
// The fixture produces 27 escaped group cells, so RanExit's exceptional values do land in the group
// block's cold tail. The assertion that makes this discriminate is on SERVED, not just on the counts:
// a payload that has stopped being readable does not usually answer wrongly -- the columnar path
// declines and the row path answers, correctly and silently -- so a query that was served before the
// restart and is not after is the thing to catch.
func TestGroupColumnsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	open := func() *Collection {
		c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16,
			GroupSchemaCount: 4, GroupStabilityRuns: 1})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	for i := range 4000 {
		// A below-floor attribute first, so the global id space shifts across the restart.
		lead := ""
		if i < 3 {
			lead = fmt.Sprintf("Aardvark=%d; ", i)
		}
		text := fmt.Sprintf(`[ %sClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d ]`,
			lead, i, i%10, i%7, i%6)
		if i%3 == 0 {
			// The bundle that becomes a group schema. RanExit is usually an integer and occasionally
			// a string, so those records escape into the GROUP block's cold tail.
			exit := fmt.Sprintf("%d", i%3)
			if i%150 == 0 {
				exit = `"aborted"`
			}
			text = fmt.Sprintf(`[ %sClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RanStart=%d; RanEnd=%d; RanHost="h%d"; RanExit=%s ]`,
				lead, i, i%10, i%7, i%6, i, i+10, i%50, exit)
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
	if st := c.schemaScan.Load(); st == nil || len(st.groups) == 0 {
		c.Close()
		t.Skip("no group schema was derived")
	}
	exprs := []string{
		`RanExit == "aborted"`, // the escaped group value
		"RanExit == 0",         // an in-column group value
		"RanHost == \"h7\"",    // a group string column
		"RanStart is undefined",
		"ProcId < 5",
	}
	// served is recorded alongside the counts, because a payload that has stopped being readable does
	// not usually produce a wrong number -- the columnar path DECLINES and the row path answers, which
	// is correct and silent. A query that was served before a restart and is not after is the symptom
	// to catch.
	measure := func(c *Collection) (counts []int, served []bool, fallbacks int64) {
		before := ColumnarFallbacks()
		for _, e := range exprs {
			q, err := vm.Parse(e)
			if err != nil {
				t.Fatal(err)
			}
			n, ok := c.CountQuery(q)
			if want := len(readAll(t, c, e)); ok && n != want {
				t.Errorf("%s: columnar says %d, the row path says %d", e, n, want)
			}
			counts = append(counts, n)
			served = append(served, ok)
		}
		return counts, served, ColumnarFallbacks() - before
	}
	want, wantServed, wantFB := measure(c)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2 := open()
	defer c2.Close()
	got, gotServed, gotFB := measure(c2)
	for i, e := range exprs {
		if got[i] != want[i] {
			t.Errorf("%s: %d after restart, want %d", e, got[i], want[i])
		}
		if wantServed[i] && !gotServed[i] {
			t.Errorf("%s: the columnar path served this before the restart and declines after it, "+
				"which returns the right answer from the row path and hides that the group columns "+
				"became unreadable", e)
		}
	}
	if gotFB > wantFB {
		t.Errorf("group columns deferred to the record %d times after a restart and %d before", gotFB, wantFB)
	}
	t.Logf("counts %v; fallbacks before=%d after=%d", got, wantFB, gotFB)
}

// TestNonInternedColumnarSurvivesRestart covers the segments the dictionary fix does NOT reach.
//
// A segment with no dictionary has no durable id space of its own, so its cold tail carries global
// intern ids -- the ones renumbered at every Open. The section names them instead, and this is the
// test that the naming works: without it, a type-exception value on an attribute whose id moves reads
// as undefined after a restart, exactly as it did for interned segments.
func TestNonInternedColumnarSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	open := func() *Collection {
		// Not append-only and no interning at seal, so the sealed segments stay inline.
		c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	for i := range 4000 {
		lead := ""
		if i < 3 {
			lead = fmt.Sprintf("Aardvark=%d; ", i) // below the presence floor: shifts the id space
		}
		cid := fmt.Sprintf("%d", i)
		if i%50 == 1 {
			cid = `"cluster-as-text"` // a type exception, so it lands in the cold tail
		}
		ad, err := classad.Parse(fmt.Sprintf(
			`[ %sClusterId=%s; ProcId=%d; Owner="user%d"; JobStatus=%d ]`, lead, cid, i%10, i%7, i%6))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	// No RetrainDict: interning rides that pass, so the sealed segments stay INLINE, which is the
	// state a table is in before its first retrain.
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		c.Close()
		t.Skip("schema scan did not enable")
	}
	inline := 0
	for _, seg := range c.shards[0].segs {
		if seg != nil && seg.used > 0 && seg.dict.Load() == nil {
			inline++
		}
	}
	if inline == 0 {
		c.Close()
		t.Skip("every sealed segment was interned; this test is about the ones that are not")
	}

	exprs := []string{"ClusterId is undefined", "ClusterId < 100", "ProcId < 5"}
	measure := func(c *Collection) []int {
		var out []int
		for _, e := range exprs {
			q, err := vm.Parse(e)
			if err != nil {
				t.Fatal(err)
			}
			n, served := c.CountQuery(q)
			if want := len(readAll(t, c, e)); served && n != want {
				t.Errorf("%s: columnar says %d, the row path says %d", e, n, want)
			}
			out = append(out, n)
		}
		return out
	}
	want := measure(c)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c2 := open()
	defer c2.Close()
	got := measure(c2)
	for i, e := range exprs {
		if got[i] != want[i] {
			t.Errorf("%s: %d after restart, want %d", e, got[i], want[i])
		}
	}
	t.Logf("%d inline sealed segments; counts %v across the restart", inline, got)
}
