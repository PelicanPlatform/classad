package collections

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// nestedRecordAd is ad i of a fixture whose projected attributes reach their siblings only through a
// nested record literal. `[alpha = Foo].alpha` evaluates Foo in the record's scope, which falls
// through to the enclosing ad, so Foo is part of X's closure even though no expression outside the
// record names it. Ord is what the queries select on; nothing projected refers to it.
func nestedRecordAd(i int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `[ MyType = "Job"; Ord = %d; Foo = %d; Bar = %d; Baz = %d; Qux = %d`, i, i%13, i%7, 100+i%11, i%5)
	b.WriteString(`; X = [alpha = Foo; beta = Bar].alpha + Baz`)
	b.WriteString(`; L = { 1, [a = Foo; b = Qux], 3 }[1].b`)
	b.WriteString(`; D = [outer = [inner = Foo * 10].inner].outer + Qux`)
	b.WriteString(`; M = [m = Qux * 2; n = Bar].m`)
	for f := 0; f < 8; f++ {
		fmt.Fprintf(&b, `; Filler%02d = "filler %d-%d"`, f, i, f)
	}
	b.WriteString(" ]")
	return b.String()
}

// TestProjectionChasesRefsThroughNestedRecords holds the reference-chasing reads on a persistent
// (inline-keyed) store to the plain evaluation of the whole ad, for attributes whose references are
// reachable only through a nested record literal. The reference walk used to read an inline record's
// member names as interned ids, which stopped it at the record: the projected row shipped X without
// Foo or Baz and X evaluated to undefined at the far end.
//
// Run over inline segments both whole and columnarized, so the narrowed builder and its closure check
// see the same shapes the plain read does.
func TestProjectionChasesRefsThroughNestedRecords(t *testing.T) {
	const n = 1500
	for _, columnar := range []bool{false, true} {
		name := "inline"
		if columnar {
			name = "inline+columnar"
		}
		t.Run(name, func(t *testing.T) {
			c, err := Open(Options{Dir: t.TempDir(), Shards: 2, SegmentSize: 1 << 15})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.Close() })
			if !c.inline {
				t.Fatal("a persistent collection is expected to store inline-keyed records")
			}
			for i := 0; i < n; i++ {
				if err := c.Put([]byte(fmt.Sprintf("k%05d", i)), mustAd(t, nestedRecordAd(i))); err != nil {
					t.Fatal(err)
				}
			}
			if columnar {
				if !c.BuildAndEnableSchemaScan(4096, 8) {
					t.Fatal("no columnar schema derived: the columnar arm would not be covered")
				}
				c.ColumnarizeSealed()
				if columnarSegmentsForTest(c) == 0 {
					t.Fatal("fixture columnarized nothing")
				}
			}

			// The oracle: every attribute's value evaluated on the whole stored ad.
			full := map[string]map[string]string{}
			var whole []*classad.ClassAd
			for ad := range c.Scan() {
				whole = append(whole, ad)
				ord, ok := ad.EvaluateAttrInt("Ord")
				if !ok {
					t.Fatal("a stored ad has no Ord")
				}
				full[fmt.Sprint(ord)] = evalAll(ad, "X", "L", "D", "M")
			}
			if len(full) != n {
				t.Fatalf("scan returned %d ads, want %d", len(full), n)
			}
			for _, v := range full {
				if v["X"] == "undefined" || v["L"] == "undefined" || v["D"] == "undefined" || v["M"] == "undefined" {
					t.Fatalf("the fixture's expressions do not evaluate on the whole ad: %v", v)
				}
				break
			}

			for _, q := range []string{"true", "Ord >= 700"} {
				for _, proj := range [][]string{{"Ord", "X"}, {"Ord", "L"}, {"Ord", "D", "M"}} {
					label := q + "/" + strings.Join(proj, "+")
					check := func(path string, ad *classad.ClassAd) {
						t.Helper()
						ord, ok := ad.EvaluateAttrInt("Ord")
						if !ok {
							t.Fatalf("%s %s: a row has no Ord", path, label)
						}
						want := full[fmt.Sprint(ord)]
						got := evalAll(ad, proj[1:]...)
						for _, a := range proj[1:] {
							if got[a] != want[a] {
								t.Fatalf("%s %s: Ord %d %s = %s on the projected row, %s on the whole ad (row: %s)",
									path, label, ord, a, got[a], want[a], ad.String())
							}
						}
					}
					rows := 0
					for ra := range c.QueryRawProjected(mustParseQ(t, q), proj, true, false) {
						check("text", rawAdToClassAd(t, ra))
						rows++
					}
					wrows := 0
					for row := range c.QueryRawWireRefs(mustParseQ(t, q), proj, false) {
						m := wireRowMap(t, row)
						var b strings.Builder
						for k, v := range m {
							fmt.Fprintf(&b, "%s = %s\n", k, v)
						}
						ad, err := classad.ParseOld(b.String())
						if err != nil {
							t.Fatalf("wire %s: row does not parse: %v\n%s", label, err, b.String())
						}
						check("wire", ad)
						wrows++
					}
					if rows == 0 || rows != wrows {
						t.Fatalf("%s: text path returned %d rows, wire path %d", label, rows, wrows)
					}
				}
			}

			// A query that reads X matches exactly the ads the plain evaluation says it matches, on
			// the narrowed path and the full one alike.
			for _, q := range []string{"X > 108", "L == 2", "D >= 60", "M == 4"} {
				want := 0
				expr := mustParseQ(t, q).Expr()
				for _, ad := range whole {
					if b, err := ad.EvaluateExpr(expr).BoolValue(); err == nil && b {
						want++
					}
				}
				got := 0
				for range c.Query(mustParseQ(t, q)) {
					got++
				}
				if want == 0 || want == n || got != want {
					t.Fatalf("%s: query matched %d ads, plain evaluation %d of %d", q, got, want, n)
				}
			}
		})
	}
}

// evalAll evaluates each named attribute of ad, rendered for comparison.
func evalAll(ad *classad.ClassAd, names ...string) map[string]string {
	out := make(map[string]string, len(names))
	for _, nm := range names {
		out[nm] = ad.EvaluateAttr(nm).String()
	}
	return out
}

// rawAdToClassAd parses a text-projected row back into an ad.
func rawAdToClassAd(t *testing.T, ra RawAd) *classad.ClassAd {
	t.Helper()
	var b strings.Builder
	for _, e := range ra.Exprs {
		b.Write(e)
		b.WriteByte('\n')
	}
	ad, err := classad.ParseOld(b.String())
	if err != nil {
		t.Fatalf("projected row does not parse: %v\n%s", err, b.String())
	}
	return ad
}
