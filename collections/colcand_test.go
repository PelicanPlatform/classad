package collections

import (
	"fmt"
	"math"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// The reference for everything here is the RECORD SCAN -- c.Query, which decodes each ad and asks the
// ordinary evaluator -- because that is the answer the columnar tiers exist to reproduce faster. A test
// that compared one columnar path against another would agree with itself about any shared mistake, and
// the mistakes this file is looking for are exactly the shared kind: a comparison lowered to the wrong
// integer boundary, a real column read as an integer, an escaped record's undefined slot bytes taken as
// its value.

// candFixture is a collection shaped to put the narrowing path through every case it distinguishes.
//
//	JobStatus    a one-byte unsigned column: narrow, no engine has lanes for it, scalar plane kernel
//	Scale        a two-byte column, which both engines do have lanes for
//	Spill        an integer column carrying a REAL on one record in ninety-seven: ESCAPES whose value
//	             is still a number, read from the cold tail, which the plane path has to repair rather
//	             than believe the undefined bytes left in the slot
//	Sometimes    absent on one record in thirteen: escapes that are MISSING, so the comparison is
//	             UNDEFINED and the record matches nothing
//	Mixed        an integer on all but one record in two hundred, where it is an EXPRESSION: only the
//	             evaluator can compare that, so the columns have to hand the whole query back
//	Ratio        a real column, whose slot holds math.Float64bits -- read as an integer it would
//	             produce astronomical values, so it must never take the integer path
//	Huge         values past 2^53, where a float64 comparison and an integer one genuinely disagree:
//	             the lowering must decline rather than "fix" the answer
func candFixture(tb testing.TB, n int) *Collection {
	tb.Helper()
	cd, err := NewZSTDCodec(nil)
	if err != nil {
		tb.Fatal(err)
	}
	c := New(Options{Shards: 1, SegmentSize: 1 << 16, Codec: cd})
	for i := 0; i < n; i++ {
		// A real where the column is an integer one is what escaping is for, and unlike an
		// out-of-slot integer it cannot be absorbed by the encoder widening the slot.
		spill := fmt.Sprintf("%d", i%200)
		if i%97 == 0 {
			spill = fmt.Sprintf("%d.5", i%200)
		}
		mixed := fmt.Sprintf("%d", i%300)
		if i%200 == 0 {
			mixed = "ClusterId + 1"
		}
		src := fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = %d\nScale = %d\n"+
			"Spill = %s\nMixed = %s\nRatio = %.3f\nHuge = %d\nOwner = \"user%d\"\nCmd = \"/x\"\n",
			i, i%7, 1+i%5, i%4096, spill, mixed, 1.5+float64(i%100)/4, (1<<53)+int64(i%8), i%64)
		if i%13 != 0 {
			src += fmt.Sprintf("Sometimes = %d\n", i%50)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(tb, src)); err != nil {
			tb.Fatal(err)
		}
	}
	// Train demand so the queried columns are hot, which is the layout these paths run against.
	for _, e := range []string{"JobStatus >= 0", "Scale >= 0", "Spill >= 0", "Mixed >= 0",
		"Ratio >= 0", "Huge >= 0", "Sometimes >= 0"} {
		q, err := vm.Parse(e)
		if err != nil {
			tb.Fatal(err)
		}
		for j := 0; j < 20; j++ {
			for range c.Query(q) {
			}
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 16) {
		tb.Skip("no sealed segments")
	}
	return c
}

// candReadFixture is the one candFixture the read-only tests share, for the reason groupReadFixture is
// shared: building it costs real time under the race detector, and four tests building their own put the
// collections race job's budget at risk for no coverage. A test that MUTATES the collection builds its
// own, or it changes what every later test sees.
var (
	candReadOnce sync.Once
	candReadColl *Collection
)

func candReadFixture(tb testing.TB) *Collection {
	tb.Helper()
	candReadOnce.Do(func() { candReadColl = candFixture(tb, 4000) })
	if candReadColl == nil {
		tb.Skip("candidate fixture unavailable")
	}
	return candReadColl
}

// candConstraints spans the lowering's decision points, not just its happy path.
var candConstraints = []string{
	"JobStatus == 4",                   // the one-byte equality a dashboard asks
	"JobStatus != 4",                   // the complemented form
	"Scale >= 2048 && JobStatus == 4",  // a conjunction over two widths
	"Scale < 100",                      // a two-byte column, the lane-bearing width
	"Scale >= 100 && Scale <= 200",     // two comparisons on ONE field: planes intersected
	"Spill > 150",                      // a field with escaped (not-in-slot) records
	"Spill > 150.4 && Spill < 160",     // a range only the escaped records fall in
	"Sometimes == 7",                   // a field with escaped (missing) records
	"Sometimes >= 0 && JobStatus == 2", // missing is UNDEFINED, so it fails the conjunction
	"Ratio > 3.0",                      // a real column: never the integer path
	"Ratio >= 1.5 && Scale < 2000",     // real and integer in one conjunction
	"Scale > 100.5",                    // a fractional literal on an integer column
	"Scale <= 100.5",                   // the other direction of the same boundary
	"Scale == 100.5",                   // never true, and has no integer form
	"Huge > 9007199254740992",          // past 2^53: the lowering must decline
	"Huge == 9007199254740993",         // where float64 and int64 genuinely disagree
	"ProcId >= 0",                      // matches everything
	"ProcId > 100",                     // matches nothing
	"JobStatus == 4 && Scale >= 2048 && Spill > 150 && Sometimes < 40", // four fields at once
}

// scanCount is the reference: how many records the ordinary record scan says match.
func scanCount(tb testing.TB, c *Collection, expr string) int {
	tb.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		tb.Fatal(err)
	}
	n := 0
	for range c.Query(q) {
		n++
	}
	return n
}

// TestCandNarrowMatchesScan checks the whole tier-1 surface against the record scan: the count, the
// grouped count, and the grouped count under a bucket, since each walks the candidate set differently.
func TestCandNarrowMatchesScan(t *testing.T) {
	c := candReadFixture(t)
	for _, expr := range candConstraints {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		want := scanCount(t, c, expr)
		if got, ok := c.CountQuery(q); ok && got != want {
			t.Errorf("%q: count %d, row scan %d", expr, got, want)
		}
		if gs, ok := c.GroupCountQuery(q, "ProcId"); ok {
			total := 0
			for _, g := range gs {
				total += g.Count
			}
			if total != want {
				t.Errorf("%q GROUP BY ProcId: %d records across groups, row scan %d", expr, total, want)
			}
			checkGroupsMatchRows(t, c, expr, "ProcId")
		}
		if gs, ok := c.GroupStatsQueryBucketed(q, "Scale", 512, []string{"JobStatus"}); ok {
			total := 0
			for _, g := range gs {
				total += g.Count
			}
			if total != want {
				t.Errorf("%q GROUP BY bucket(Scale,512): %d records, row scan %d", expr, total, want)
			}
		}
	}
}

// TestCandFixtureHasEscapes proves the fixture reaches the mixed path at all. Without it every
// escape-handling assertion above could pass on a corpus where nothing ever escaped, which is how a
// test ends up agreeing with an implementation that quietly believes an undefined slot.
func TestCandFixtureHasEscapes(t *testing.T) {
	c := candReadFixture(t)
	st := c.schemaScan.Load()
	if st == nil {
		t.Fatal("no schema scan state")
	}
	for _, attr := range []string{"Spill", "Sometimes"} {
		id, ok := c.intern.LookupID(attr)
		if !ok {
			t.Fatalf("%s not interned", attr)
		}
		escaped, total := 0, 0
		for _, sh := range c.shards {
			_, wins := sh.snapshot()
			for _, w := range wins {
				cs := w.seg.colblk.Load()
				sch := cs.schema()
				if sch == nil {
					continue
				}
				idx, ok := sch.byID[id]
				if !ok {
					continue
				}
				for _, blk := range cs.blocks {
					total++
					if !blk.escapeFree(idx) {
						escaped++
					}
				}
			}
			releaseWindows(wins)
		}
		if total == 0 {
			t.Errorf("%s is not a column in any sealed block", attr)
		} else if escaped == 0 {
			t.Errorf("%s escapes in none of %d blocks: the mixed path is untested", attr, total)
		}
	}
}

// TestCandNarrowMatchesRecordForm compares the plane narrowing against the per-record form over the
// REAL blocks, field by field and operator by operator.
//
// The end-to-end checks above can only see a disagreement that changes a count. This sees one that
// changes a single record's bit, including in a block whose records all fail anyway -- and it runs
// every operator against boundary literals, which no realistic constraint set covers.
func TestCandNarrowMatchesRecordForm(t *testing.T) {
	c := candReadFixture(t)
	st := c.schemaScan.Load()
	if st == nil {
		t.Fatal("no schema scan state")
	}
	cases := []struct {
		attr string
		lits []float64
	}{
		{"JobStatus", []float64{0, 1, 3, 5, 6}},
		{"Scale", []float64{0, 1, 100, 100.5, 2048, 4095, 4096, 100000}},
		{"Spill", []float64{0, 150, 150.5, 199, 199.5, 200}},
		{"Sometimes", []float64{0, 7, 49, 50}},
		{"Ratio", []float64{0, 1.5, 3, 25.75, 26}},
	}
	for _, tc := range cases {
		for _, op := range []string{"<", "<=", ">", ">=", "==", "!="} {
			for _, lit := range tc.lits {
				expr := fmt.Sprintf("%s %s %v", tc.attr, op, lit)
				q, err := vm.Parse(expr)
				if err != nil {
					t.Fatalf("%s: %v", expr, err)
				}
				preds, ok := c.numPredsOnFields(q, st.schema)
				if !ok || len(preds) != 1 {
					t.Fatalf("%s: not analyzed as one field predicate", expr)
				}
				plane := narrowAllBlocks(t, c, preds[0], st.cache, true)
				record := narrowAllBlocks(t, c, preds[0], st.cache, false)
				if plane != record {
					t.Errorf("%s: plane narrowing kept %d records, per-record form kept %d",
						expr, plane, record)
				}
			}
		}
	}
}

// narrowAllBlocks runs one predicate over every sealed block and totals the survivors, through either
// the plane form or the per-record form. Forcing the per-record form is what makes the comparison
// meaningful: both halves have to be reachable from one call site, or the test compares a path against
// itself.
func narrowAllBlocks(tb testing.TB, c *Collection, p fieldPred, bc *blockCache, lowered bool) int {
	tb.Helper()
	n, ok := narrowAllBlocksOK(c, p, bc, lowered)
	if !ok {
		tb.Fatal("narrowByField declined")
	}
	return n
}

// narrowAllBlocksOK is narrowAllBlocks reporting the DECLINE rather than failing on it, for the test
// whose subject is the decline.
func narrowAllBlocksOK(c *Collection, p fieldPred, bc *blockCache, lowered bool) (int, bool) {
	if !lowered {
		p.intOK = false
	}
	var cand candSet
	var sc predScratch
	total := 0
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			cs := w.seg.colblk.Load()
			idxs, resolved := resolveFields(cs, []fieldPred{p})
			if !resolved {
				continue
			}
			base := 0
			for _, blk := range cs.blocks {
				sc.prepare(blk.n)
				cand.setVisible(cs, w, s0, base, blk.n)
				if !narrowByField(blk, idxs[0], p, bc, &cand, &sc) {
					releaseWindows(wins)
					return 0, false
				}
				total += cand.live
				base += blk.n
			}
		}
		releaseWindows(wins)
	}
	return total, true
}

// TestLowerIntTests pins the lowering's boundaries directly, since the cases where it must DECLINE
// leave no trace in an answer -- the float64 loop produces the same count, just slower, so a lowering
// that wrongly accepted one of these would only be visible as a wrong answer on a corpus holding the
// values that distinguish them.
func TestLowerIntTests(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   zoneTest
		want intTest
		ok   bool
	}{
		{"integral", zoneTest{op: ">=", vals: []float64{4}}, intTest{">=", 4}, true},
		{"negative", zoneTest{op: "<", vals: []float64{-500}}, intTest{"<", -500}, true},
		{"frac lt", zoneTest{op: "<", vals: []float64{100.5}}, intTest{"<=", 100}, true},
		{"frac le", zoneTest{op: "<=", vals: []float64{100.5}}, intTest{"<=", 100}, true},
		{"frac gt", zoneTest{op: ">", vals: []float64{100.5}}, intTest{">", 100}, true},
		{"frac ge", zoneTest{op: ">=", vals: []float64{100.5}}, intTest{">", 100}, true},
		{"frac neg", zoneTest{op: ">", vals: []float64{-0.5}}, intTest{">", -1}, true},
		{"frac eq", zoneTest{op: "==", vals: []float64{100.5}}, intTest{}, false},
		{"frac ne", zoneTest{op: "!=", vals: []float64{100.5}}, intTest{}, false},
		{"nan", zoneTest{op: "==", vals: []float64{math.NaN()}}, intTest{}, false},
		{"huge", zoneTest{op: ">", vals: []float64{1 << 62}}, intTest{}, false},
		{"two values", zoneTest{op: "==", vals: []float64{1, 2}}, intTest{}, false},
	} {
		got, ok := lowerIntTests([]zoneTest{tc.in})
		if ok != tc.ok {
			t.Errorf("%s: lowered=%v, want %v", tc.name, ok, tc.ok)
			continue
		}
		if ok && got[0] != tc.want {
			t.Errorf("%s: lowered to %+v, want %+v", tc.name, got[0], tc.want)
		}
	}
	// One field carrying several comparisons lowers all of them or none: the planes are intersected,
	// and a conjunction missing one of its terms is a wrong answer rather than a slow one.
	if _, ok := lowerIntTests([]zoneTest{{op: ">=", vals: []float64{1}}, {op: "==", vals: []float64{1.5}}}); ok {
		t.Error("lowered a field whose second comparison has no integer form")
	}
}

// TestComparePlaneAgainstKernels checks the two kernels agree with each other on the same column, so a
// build with an engine and one without cannot answer differently. Only the raw path changes between
// them; the widened path is what the production build always runs.
func TestComparePlaneAgainstKernels(t *testing.T) {
	const n = 1000
	vals := make([]int64, n)
	raw := make([]byte, n*2)
	for i := range vals {
		vals[i] = int64(int16(i*37 - 20000))
		raw[i*2] = byte(uint16(vals[i]))
		raw[i*2+1] = byte(uint16(vals[i]) >> 8)
	}
	a := make([]uint64, (n+63)/64)
	b := make([]uint64, (n+63)/64)
	for _, op := range []string{"<", "<=", ">", ">=", "==", "!="} {
		for _, lit := range []int64{-20000, -1, 0, 1, 16649, 20000} {
			if !vm.CompareIntsPlane(op, vals, lit, n, a) {
				t.Fatalf("%s %d: widened kernel declined", op, lit)
			}
			if !vm.CompareRawPlane(op, raw, 2, false, lit, n, b) {
				continue // no engine in this build, which is the production case
			}
			for w := range a {
				if a[w] != b[w] {
					t.Fatalf("%s %d: word %d raw=%#016x widened=%#016x", op, lit, w, b[w], a[w])
				}
			}
		}
	}
}

// TestCandVisibilityAfterUpdate covers the one thing the visibility pass exists for: a record in a
// SEALED block that a later write superseded. The block still holds it, its columns still answer for
// it, and only the record header says it is gone -- so a visibility pass that read the header wrongly,
// or skipped it, would count the old value and the new one both.
//
// Every other test here reads a collection nobody updated, where a visibility pass that returned TRUE
// unconditionally would pass them all.
func TestCandVisibilityAfterUpdate(t *testing.T) {
	c := candFixture(t, 4000)
	defer c.Close()
	const expr = "JobStatus == 4 && Scale >= 2048"
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	before, ok := c.CountQuery(q)
	if !ok {
		t.Fatal("count declined")
	}
	// Rewrite one record in eight, flipping it into or out of the constraint. The new versions land
	// in the active segment; the old ones stay in their sealed blocks, superseded.
	for i := 0; i < 4000; i += 8 {
		src := fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = 4\nScale = %d\n"+
			"Spill = %d\nRatio = %.3f\nHuge = %d\nOwner = \"user%d\"\nCmd = \"/x\"\nSometimes = %d\n",
			i, i%7, 3000+i%1000, i%200, 1.5+float64(i%100)/4, (1<<53)+int64(i%8), i%64, i%50)
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	want := scanCount(t, c, expr)
	if want == before {
		t.Fatalf("the rewrite changed no counts (%d), so supersession is not being exercised", want)
	}
	got, ok := c.CountQuery(q)
	if !ok {
		t.Fatal("count declined after the rewrite")
	}
	if got != want {
		t.Errorf("%q after rewriting a record in eight: count %d, row scan %d", expr, got, want)
	}
	gs, ok := c.GroupCountQuery(q, "ProcId")
	if !ok {
		t.Fatal("grouped count declined after the rewrite")
	}
	total := 0
	for _, g := range gs {
		total += g.Count
	}
	if total != want {
		t.Errorf("%q GROUP BY ProcId after the rewrite: %d records, row scan %d", expr, total, want)
	}
}

// TestCandDeclinesOnExpression pins the one thing a narrowing pass must NOT do with a value it cannot
// compare: treat it as failing. An attribute holding an expression on even one candidate record makes the
// whole query the evaluator's, and the columnar count has to say so rather than return a number that is
// short by however many such records there were.
//
// It asserts the DECLINE rather than the answer, because the answer is right either way -- the caller
// re-runs the query on a tier that can evaluate it -- so a version that silently dropped those records
// would still agree with the row scan on every corpus where the expression happened not to match.
func TestCandDeclinesOnExpression(t *testing.T) {
	c := candReadFixture(t)
	st := c.schemaScan.Load()
	if st == nil {
		t.Fatal("no schema scan state")
	}
	q, err := vm.Parse("Mixed > 150")
	if err != nil {
		t.Fatal(err)
	}
	preds, ok := c.numPredsOnFields(q, st.schema)
	if !ok {
		t.Fatal("Mixed > 150 is not analyzed as a field predicate: this test checks nothing")
	}
	// Both forms of the narrowing have to decline, and each is checked on its own: the whole-query
	// decline below can also come from the row walk over the active segment, which would let a
	// narrowing pass that swallowed the expression go unnoticed.
	for _, lowered := range []bool{true, false} {
		if n, ok := narrowAllBlocksOK(c, preds[0], st.cache, lowered); ok {
			t.Errorf("lowered=%v: narrowing kept %d records for a field holding an expression; it "+
				"must hand the query back", lowered, n)
		}
	}
	if n, ok := c.schemaScanCountMulti(preds, st.cache); ok {
		t.Errorf("the column scan answered %d for a field holding an expression; it must hand the "+
			"query back", n)
	}
	// And the public path still returns the right answer, from whichever tier took it.
	want := scanCount(t, c, "Mixed > 150")
	if got, ok := c.CountQuery(q); !ok {
		t.Error("CountQuery declined entirely")
	} else if got != want {
		t.Errorf("Mixed > 150: count %d, row scan %d", got, want)
	}
}
