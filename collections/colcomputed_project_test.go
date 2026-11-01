package collections

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// computedProjectFixture is a columnarized collection in which ONE schema field is written as an
// expression over another in every record -- the shape that costs a projection its columnar path,
// and the shape colcomputed.go exists for. Rss is the literal control: same kind, same column, no
// expression, so a difference between the two is about the expression and not about the fixture.
func computedProjectFixture(t testing.TB, n int) *Collection {
	t.Helper()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	for i := range n {
		text := fmt.Sprintf(
			`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; Rss=%d; Mem=(Rss+1023)/1024; Cmd="/bin/x" ]`,
			i, i%10, i%7, 1+i%6, 1024*(1+i%37))
		ad, perr := classad.Parse(text)
		if perr != nil {
			t.Fatal(perr)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	// The production entry point, and REQUIRED rather than skipped on: a fixture that quietly
	// failed to columnarize would send every test below down the record path, where they would
	// all pass while measuring nothing.
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no columnar schema was derived")
	}
	info := c.SchemaScanInfo()
	if !info.Enabled || info.SealedSegments == 0 || info.CoveredSegments < info.SealedSegments {
		t.Fatalf("columnar coverage is %+v; every sealed segment must be covered", info)
	}
	return c
}

func projectScanStats(t *testing.T, c *Collection, attrs []string) (ScanStats, []classad.Value, int) {
	t.Helper()
	q, _ := vm.Parse(`true`)
	var st ScanStats
	rows, undef := 0, 0
	var first []classad.Value
	for vals := range c.QueryProjectStats(q, attrs, &st) {
		if rows == 0 {
			first = append([]classad.Value(nil), vals...)
		}
		rows++
		for _, v := range vals {
			if v.IsUndefined() {
				undef++
			}
		}
	}
	if undef != 0 {
		t.Fatalf("%v: %d projected values came back UNDEFINED", attrs, undef)
	}
	return st, first, rows
}

// TestEvaluatingProjectionUsesComputedCells is the point of the substitution: projecting an
// expression-valued attribute must cost no more of the columnar path than projecting a literal one.
//
// Before it, an expression cell stayed in the narrowed ad, the closure check rejected it -- its
// references are the attributes the projection did not name -- and after eight consecutive misses
// the scan abandoned the fast path for the rest of the segment. So the measure is RecordsReassembled
// against the literal control, not against zero.
func TestEvaluatingProjectionUsesComputedCells(t *testing.T) {
	c := computedProjectFixture(t, 3000)
	lit, _, litRows := projectScanStats(t, c, []string{"JobStatus", "Rss"})
	expr, _, exprRows := projectScanStats(t, c, []string{"JobStatus", "Mem"})
	if litRows != exprRows || litRows == 0 {
		t.Fatalf("the two projections visited %d and %d records", litRows, exprRows)
	}
	if lit.RecordsReassembled >= litRows {
		t.Fatalf("the literal control reassembled %d of %d records, so the columnar projection is "+
			"not running at all and this test cannot measure the expression case",
			lit.RecordsReassembled, litRows)
	}
	if expr.RecordsReassembled > lit.RecordsReassembled {
		t.Errorf("projecting an expression-valued attribute reassembled %d of %d records against "+
			"the literal control's %d: the precomputed cell is not being substituted",
			expr.RecordsReassembled, exprRows, lit.RecordsReassembled)
	}
	t.Logf("reassembled: literal %d, expression %d, of %d records",
		lit.RecordsReassembled, expr.RecordsReassembled, litRows)
}

// TestComputedSubstitutionKeepsTheValue is the correctness half: the substituted literal must be
// the value the expression has, for every record, measured against the full-decode path rather
// than against itself.
func TestComputedSubstitutionKeepsTheValue(t *testing.T) {
	c := computedProjectFixture(t, 3000)
	attrs := []string{"ClusterId", "Mem", "Rss"}
	want := projectTruth(t, c, "true", attrs)
	q, _ := vm.Parse(`true`)
	got := 0
	for vals := range c.QueryProject(q, attrs) {
		key := vals[0].String()
		w, ok := want[key]
		if !ok {
			t.Fatalf("QueryProject returned ClusterId=%s, which the full decode did not", key)
		}
		for i := range attrs {
			if vals[i].String() != w[i] {
				t.Fatalf("ClusterId=%s %s: projected %q, full decode %q",
					key, attrs[i], vals[i].String(), w[i])
			}
		}
		got++
	}
	if got != len(want) {
		t.Fatalf("QueryProject returned %d records, full decode %d", got, len(want))
	}
}

// TestRelayingProjectionKeepsTheExpression is the safety half, and it is the reason the
// substitution is gated on projPlan.evaluates rather than on projPlan.closure.
//
// Those two are easy to conflate: a closure plan means the consumer FOLLOWS REFERENCES, and
// QueryRawWireRefs does exactly that -- and then ships the entries it was given. A substitution
// licensed by closure would put a computed literal into a relayed row, so an ad read back out of
// this store would hold `Mem = 19` where the record holds `Mem = (Rss+1023)/1024`. The storage
// layer's rule is that a computed value never stands in for the stored literal, and a relay is
// precisely where that rule bites.
func TestRelayingProjectionKeepsTheExpression(t *testing.T) {
	c := computedProjectFixture(t, 3000)
	q, _ := vm.Parse(`true`)
	rows, exprs := 0, 0
	for w := range c.QueryRawWireRefs(q, []string{"JobStatus", "Mem"}, false) {
		buf, offs, _, _, ok := RenderRawAdInline(w, nil, nil)
		if !ok {
			t.Fatal("RenderRawAdInline failed on a wire row")
		}
		rows++
		for i := 0; i+1 < len(offs); i++ {
			e := string(buf[offs[i]:offs[i+1]])
			if !strings.HasPrefix(e, "Mem") {
				continue
			}
			if !strings.Contains(e, "Rss") {
				t.Fatalf("a relayed row carries %q: the computed literal was substituted into a "+
					"read whose consumer SHIPS what it is given", e)
			}
			exprs++
		}
	}
	if rows == 0 {
		t.Fatal("the wire-row read returned nothing")
	}
	if exprs != rows {
		t.Fatalf("Mem appeared as an expression in %d of %d relayed rows; the rest lost it", exprs, rows)
	}
}
