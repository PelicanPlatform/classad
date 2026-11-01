package collections

import (
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
