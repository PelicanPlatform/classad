package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// What the COLUMNAR-NATIVE tier does with an attribute holding an expression, which is the opposite
// of what the sidecar tier does and was not pinned anywhere.
//
// Three tests say a columnar path must DECLINE a field holding an expression -- the candidate
// narrowing (TestCandDeclinesOnExpression), the grouped count
// (TestGroupCountHeterogeneousGroupValues) and the escape-bitmap presence path
// (TestPresenceCountDeclinesOnExpression). All three are right, and all three are about a SIDECAR
// block, whose columns hold literals: a node cannot be compared without evaluating it, and an
// escape there proves only that the value is not in the column.
//
// A columnarized segment is not in that position. The rewrite evaluates the expression once and
// stores the result beside the column as a computed cell (colcomputed.go), so the columns can
// answer and the answer is the one the records give. Every one of those three tests runs on an
// in-memory fixture, and until recently an in-memory collection never columnarized -- so the tier
// that every persistent table actually uses was asserted about by none of them.
//
// This holds that tier to the requirement that does apply to it: not a refusal, but the row path's
// answer.

// exprFixtureNative builds a PERSISTENT collection whose sealed segments are columnarized, holding
// attributes that are expressions on a small fraction of records -- one numeric, one that groups,
// one that evaluates to undefined. The rates are ~1% because a schema field needs a dominant
// storable kind across 90% of the sample; make the exceptions common and the attribute is not a
// field at all and nothing here is exercised.
func exprFixtureNative(tb testing.TB, n int) *Collection {
	tb.Helper()
	cd, err := NewZSTDCodec(nil)
	if err != nil {
		tb.Fatal(err)
	}
	c, err := Open(Options{Dir: tb.TempDir(), Shards: 1, SegmentSize: 1 << 16, Codec: cd,
		ColumnarSegmentBudget: 1 << 20})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { c.Close() })
	for i := 0; i < n; i++ {
		mixed := fmt.Sprintf("%d", i%300)
		if i%200 == 0 {
			mixed = "ClusterId + 1" // a numeric expression: comparable only once evaluated
		}
		tally := fmt.Sprintf("%d", i%10)
		if i%200 == 7 {
			tally = "ProcId * 2" // a group value that exists only once something evaluates it
		}
		gone := fmt.Sprintf("%d", i%5)
		if i%200 == 11 {
			gone = "NoSuchAttr + 1" // evaluates to UNDEFINED: "present" would be the wrong answer
		}
		src := fmt.Sprintf("ClusterId = %d\nProcId = %d\nMixed = %s\nTally = %s\nGone = %s\n"+
			"RequestMemory = %d\nOwner = \"user%d\"\n",
			i, i%7, mixed, tally, gone, 1024+(i%32)*512, i%64)
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(tb, src)); err != nil {
			tb.Fatal(err)
		}
	}
	for _, e := range []string{"Mixed >= 0", "Tally >= 0", "Gone >= 0", "ProcId >= 0", "RequestMemory >= 0"} {
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
	info := c.SchemaScanInfo()
	if info.ColumnarSegments == 0 {
		tb.Fatalf("no sealed segment was columnarized (%d sealed, %d covered): this fixture would be "+
			"the sidecar one, which the decline tests already cover",
			info.SealedSegments, info.CoveredSegments)
	}
	return c
}

func TestColumnarNativeAnswersExpressionValuedFields(t *testing.T) {
	c := exprFixtureNative(t, 4000)

	// The fixture must really hold expressions, or every assertion below is about literals.
	for _, attr := range []string{"Mixed", "Tally", "Gone"} {
		if !schemaHas(c, attr) {
			t.Fatalf("%s is not a schema field, so no column ever carries it and this proves nothing", attr)
		}
	}

	t.Run("count", func(t *testing.T) {
		q, err := vm.Parse("Mixed > 150")
		if err != nil {
			t.Fatal(err)
		}
		want := scanCount(t, c, "Mixed > 150")
		if want == 0 {
			t.Fatal("nothing matches: the comparison would be vacuous")
		}
		served := 0
		for name, fn := range map[string]func(*vm.Query) (int, bool){
			"CountQuery": c.CountQuery, "VectorEvalCount": c.VectorEvalCount,
			"ColumnarEvalCount": c.ColumnarEvalCount,
		} {
			got, ok := fn(q)
			if !ok {
				continue // declining is always allowed; answering WRONG is not
			}
			served++
			if got != want {
				t.Errorf("%s = %d, row scan = %d", name, got, want)
			}
		}
		// If every path declined, the agreement above was agreement about nothing. The whole point
		// of this tier is that it CAN answer where the sidecar one must not.
		if served == 0 {
			t.Error("every columnar count path declined: this tier is supposed to answer here")
		}
		t.Logf("%d of 3 columnar count paths served; row scan = %d", served, want)
	})

	t.Run("groupBy", func(t *testing.T) {
		q, err := vm.Parse("RequestMemory > 4096")
		if err != nil {
			t.Fatal(err)
		}
		// Tally's expression is `ProcId * 2`, which EVALUATES to an integer -- so the row path puts
		// those records in an ordinary numeric group and the columnar partition has to agree. That
		// is the sharp form of the question: a tier that silently dropped a cell it could not
		// compare would return a partition short by exactly those records and otherwise perfect.
		want, _ := rowGroupTruth(t, c, "RequestMemory > 4096", "Tally")
		wantTotal := 0
		for _, n := range want {
			wantTotal += n
		}
		if wantTotal == 0 {
			t.Fatal("nothing matches: the comparison would be vacuous")
		}
		got, ok := c.GroupCountQuery(q, "Tally")
		if !ok {
			t.Fatal("the grouped count declined: on a columnarized segment the expression's computed " +
				"value is in the payload, so this is the tier that is supposed to answer")
		}
		total := 0
		for _, g := range got {
			total += g.Count
		}
		if total != wantTotal {
			t.Errorf("grouped count covers %d records, row path %d", total, wantTotal)
		}
		for key, n := range want {
			if g := groupCountOf(got, key); g != n {
				t.Errorf("group %d: columnar %d, row path %d", key, g, n)
			}
		}
	})

	t.Run("presence", func(t *testing.T) {
		q, err := vm.Parse("Gone is undefined")
		if err != nil {
			t.Fatal(err)
		}
		want := rowTruth(t, c, "Gone is undefined")
		if want == 0 {
			t.Fatal("no record has an undefined Gone: the expression that evaluates to undefined is missing")
		}
		pGot, pServed := c.PresenceCountQuery(q)
		if pServed && pGot != want {
			t.Errorf("PresenceCountQuery = %d, row path = %d", pGot, want)
		}
		cGot, cServed := c.CountQuery(q)
		if cServed && cGot != want {
			t.Errorf("CountQuery = %d, row path = %d", cGot, want)
		}
		if !pServed && !cServed {
			t.Error("both presence paths declined: the agreement above is agreement about nothing")
		}
		t.Logf("presence served: PresenceCountQuery=%v CountQuery=%v, row path = %d", pServed, cServed, want)
	})
}

// groupCountOf reads one group's count out of a GroupCountQuery result, 0 when the group is absent.
func groupCountOf(gs []GroupCount, key int64) int {
	for _, g := range gs {
		if n, err := g.Value.IntValue(); err == nil && n == key {
			return g.Count
		}
	}
	return 0
}
