package collections

import (
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// projectTruth reads attrs out of every ad matching expr through the full-decode path, keyed by
// ClusterId -- the answer a projected scan has to reproduce exactly.
func projectTruth(t *testing.T, c *Collection, expr string, attrs []string) map[string][]string {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for ad := range c.Query(q) {
		row := make([]string, len(attrs))
		for i, n := range attrs {
			row[i] = ad.EvaluateAttr(n).String()
		}
		out[ad.EvaluateAttr("ClusterId").String()] = row
	}
	return out
}

// TestQueryProjectExpressionValues pins what QueryProject owes its callers over a COLUMNARIZED
// segment: the value an attribute evaluates to, including when the record stores it as an
// expression whose references are not themselves projected.
//
// The aggregate path runs on this scan, so a value that comes back UNDEFINED is not an error the
// caller sees -- it is a different COUNT or a different GROUP BY bucket.
func TestQueryProjectExpressionValues(t *testing.T) {
	c, _, _ := columnarFixture(t, 3000)
	defer c.Close()
	if n := c.columnarizeSealed(); n == 0 {
		t.Skip("no sealed segment was columnarized")
	}

	// RequestMemory is a schema field that a few records store as `ProcId*512+7` (so the record
	// escapes it to the cold tail); Wide and Rare* are carried by no schema at all; ClusterId and
	// JobStatus are ordinary in-column literals. One projection covers all four shapes.
	attrs := []string{"ClusterId", "RequestMemory", "Wide", "JobStatus"}
	want := projectTruth(t, c, "true", attrs)

	q, _ := vm.Parse(`true`)
	got := map[string][]string{}
	undefined := 0
	for vals := range c.QueryProject(q, attrs) {
		row := make([]string, len(vals))
		for i, v := range vals {
			row[i] = v.String()
		}
		got[row[0]] = row
		if vals[1].IsUndefined() {
			undefined++
		}
	}
	if len(got) != len(want) {
		t.Fatalf("QueryProject returned %d records, full decode %d", len(got), len(want))
	}
	if undefined != 0 {
		t.Errorf("QueryProject returned RequestMemory UNDEFINED for %d records; every record has one", undefined)
	}
	bad := 0
	for key, w := range want {
		g, ok := got[key]
		if !ok {
			t.Fatalf("QueryProject omitted ClusterId=%s", key)
		}
		for i := range attrs {
			if g[i] != w[i] {
				bad++
				if bad <= 5 {
					t.Errorf("ClusterId=%s %s: QueryProject %q, full decode %q", key, attrs[i], g[i], w[i])
				}
			}
		}
	}
	if bad > 5 {
		t.Errorf("%d projected values disagreed with the full decode in total", bad)
	}
}

// TestProjectedAdIsNotEvaluable is the other half of the test above: it shows WHY QueryProject
// takes the reassembled record rather than the narrowed ad a projection plan would hand it, so
// the slower call is not mistaken for an oversight and quietly "fixed".
//
// The narrowed ad keeps the projected attributes as stored. An attribute stored as an expression
// keeps its references, and the narrowing is exactly what removes them -- so evaluating it
// afterwards yields UNDEFINED where a value exists. Shipping those entries is fine (QueryRawWire
// does, with a plan); evaluating them is not.
//
// If this ever stops failing, the narrowed ad has become sufficient to evaluate: pass
// c.newProjPlan(attrs) in QueryProjectStats, take the ~3x, and delete this test.
func TestProjectedAdIsNotEvaluable(t *testing.T) {
	c, _, _ := columnarFixture(t, 3000)
	defer c.Close()
	if n := c.columnarizeSealed(); n == 0 {
		t.Skip("no sealed segment was columnarized")
	}
	attrs := []string{"RequestMemory"}
	q, _ := vm.Parse(`true`)

	// The same scan QueryProject runs, differing only in whether the plan carries the projection.
	countUndefined := func(proj *projPlan) (undef, rows int) {
		scratch := make([]classad.Value, len(attrs))
		rs := &wireScope{ctx: c}
		c.scanQuery(c.newQueryPlan(q, proj, nil, false), func(w []byte, dict *segDictHandle) bool {
			rs.dict = dict
			c.projectInto(rs, w, attrs, scratch)
			rows++
			if scratch[0].IsUndefined() {
				undef++
			}
			return true
		})
		return undef, rows
	}
	whole, rows := countUndefined(nil)
	narrowed, nrows := countUndefined(c.newProjPlan(attrs))
	if rows != nrows {
		t.Fatalf("the two scans visited different record counts: %d vs %d", rows, nrows)
	}
	if whole != 0 {
		t.Fatalf("the reassembled-record scan already loses %d of %d values; the fixture is not "+
			"measuring what this test claims", whole, rows)
	}
	if narrowed == 0 {
		t.Fatalf("a projection-plan scan lost no value over %d records: either the fixture no "+
			"longer stores an expression in a projected attribute, or the narrowed ad has become "+
			"evaluable -- see this test's comment before changing QueryProjectStats", rows)
	}
	t.Logf("narrowed ad loses %d of %d values; reassembled record loses none", narrowed, rows)
}

// TestQueryProjectUsesTheColumnarPath is the other half of the closure plan: the test above shows
// the VALUES are right, and this one shows the fast path that could have broken them is actually
// running. Both are needed. A plan the scan silently declined would leave every value correct and
// every record reassembled -- the correctness tests would stay green over a scan that had quietly
// gone back to the slow path, which is the failure this pins.
//
// The measure is RecordsReassembled against the rows emitted: with no plan the scan cannot project
// from columns at all and rebuilds every record, so the two are equal. With the closure plan only
// the records that need it rebuild, and RecordsClosureReassembled names them.
func TestQueryProjectUsesTheColumnarPath(t *testing.T) {
	c, _, _ := columnarFixture(t, 3000)
	defer c.Close()
	if n := c.columnarizeSealed(); n == 0 {
		t.Skip("no sealed segment was columnarized")
	}
	attrs := []string{"ClusterId", "RequestMemory", "Wide", "JobStatus"}
	q, _ := vm.Parse(`true`)

	var st ScanStats
	rows := 0
	undefined := 0
	for vals := range c.QueryProjectStats(q, attrs, &st) {
		rows++
		// RequestMemory only: Wide and the Rare* attributes are carried by some ads and not
		// others, so UNDEFINED is the right answer for them and counting it would measure the
		// fixture rather than the plan. Every ad has a RequestMemory.
		if vals[1].IsUndefined() {
			undefined++
		}
	}
	if rows == 0 {
		t.Fatal("the scan emitted no rows")
	}
	if undefined != 0 {
		t.Fatalf("RequestMemory came back UNDEFINED for %d of %d records; the closure plan is not "+
			"holding", undefined, rows)
	}
	// The premise: without a plan this scan rebuilds every record it emits. Measured the same way,
	// on the same fixture, so the comparison is like for like rather than a remembered number.
	var base ScanStats
	baseRows := 0
	c.scanQuery(c.newQueryPlan(q, nil, &base, false), func(w []byte, dict *segDictHandle) bool {
		baseRows++
		return true
	})
	if base.RecordsReassembled != baseRows {
		t.Fatalf("the no-plan scan reassembled %d of %d records, not all of them; this test's "+
			"premise no longer holds", base.RecordsReassembled, baseRows)
	}
	if st.RecordsReassembled >= rows {
		t.Fatalf("the closure-plan scan reassembled %d of %d records -- the columnar projection "+
			"never ran, so the fast path this plan exists to enable is not engaged",
			st.RecordsReassembled, rows)
	}
	// The hazard must still be IN the fixture, or this test and TestProjectedAdIsNotEvaluable are
	// both measuring nothing. Checked directly, by looking for the stored expression, rather than
	// through RecordsClosureReassembled: that counter used to be the evidence, and it now reads
	// zero on a healthy fixture, because an escaped expression cell whose value was precomputed is
	// substituted before the closure check ever sees it. A fixture check that doubles as a
	// behaviour check stops being either the moment the behaviour improves.
	exprs := 0
	q2, _ := vm.Parse(`true`)
	for w := range c.QueryRawWireRefs(q2, []string{"RequestMemory"}, false) {
		buf, offs, _, _, ok := RenderRawAdInline(w, nil, nil)
		if !ok {
			t.Fatal("RenderRawAdInline failed on a wire row")
		}
		for i := 0; i+1 < len(offs); i++ {
			if e := string(buf[offs[i]:offs[i+1]]); strings.HasPrefix(e, "RequestMemory") &&
				strings.Contains(e, "ProcId") {
				exprs++
			}
		}
	}
	if exprs == 0 {
		t.Fatalf("no record of %d stores RequestMemory as an expression over its siblings: the "+
			"fixture no longer carries the hazard, so this test and TestProjectedAdIsNotEvaluable "+
			"are both measuring nothing", rows)
	}
	t.Logf("closure plan: %d of %d records reassembled (%d of them for closure, %d records store "+
		"the expression); no plan: %d of %d",
		st.RecordsReassembled, rows, st.RecordsClosureReassembled, exprs, base.RecordsReassembled, baseRows)
}

// BenchmarkQueryProjectPlan is what the closure plan is for: the same projected scan with the plan
// the code passes and with the nil plan it used to pass.
func BenchmarkQueryProjectPlan(b *testing.B) {
	c, _, _ := columnarFixture(b, 3000)
	defer c.Close()
	if n := c.columnarizeSealed(); n == 0 {
		b.Skip("no sealed segment was columnarized")
	}
	attrs := []string{"ClusterId", "RequestMemory", "Wide", "JobStatus"}
	q, _ := vm.Parse(`true`)
	run := func(b *testing.B, closure bool) {
		scratch := make([]classad.Value, len(attrs))
		rs := &wireScope{ctx: c}
		b.ReportAllocs()
		rows := 0
		for i := 0; i < b.N; i++ {
			var proj *projPlan
			if closure {
				proj = c.newProjPlan(attrs)
				proj.closure = true
			}
			c.scanQuery(c.newQueryPlan(q, proj, nil, false), func(w []byte, dict *segDictHandle) bool {
				rs.dict = dict
				c.projectInto(rs, w, attrs, scratch)
				rows++
				return true
			})
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(rows), "ns/row")
	}
	b.Run("no-plan", func(b *testing.B) { run(b, false) })
	b.Run("closure-plan", func(b *testing.B) { run(b, true) })
}
