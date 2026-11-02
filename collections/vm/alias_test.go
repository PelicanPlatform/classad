package vm

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// sides are the two join sides every test here evaluates against: the ads a resolver answers
// from, and the members of the composite ad the reference implementation evaluates.
func sides() (*classad.ClassAd, *classad.ClassAd) {
	j := classad.New()
	j.InsertAttrString("Owner", "alice")
	j.InsertAttr("ClusterId", 42)
	j.InsertAttrFloat("Weight", 1.5)
	j.InsertAttrBool("Done", true)

	h := classad.New()
	h.InsertAttrString("Owner", "alice")
	h.InsertAttr("ClusterId", 7)
	h.InsertAttrString("Site", "wisc")
	return j, h
}

// composite builds the joined row the aliased form is meant to avoid building. A nil side is
// left out entirely, which is how a LEFT JOIN's unmatched row carries its inner side.
func composite(j, h *classad.ClassAd) *classad.ClassAd {
	row := classad.New()
	if j != nil {
		row.InsertAttrClassAd("j", j)
	}
	if h != nil {
		row.InsertAttrClassAd("h", h)
	}
	return row
}

// aliasResolver answers (alias index, name) out of the two sides, which is what a join
// holding its rows separately can do. A nil side answers undefined for every attribute --
// the same thing an absent member of the composite evaluates to.
func aliasResolver(j, h *classad.ClassAd) func(int, string) classad.Value {
	return func(alias int, name string) classad.Value {
		ad := j
		if alias == 1 {
			ad = h
		}
		if ad == nil {
			return classad.NewUndefinedValue()
		}
		return ad.EvaluateAttr(name)
	}
}

var aliasExprs = []string{
	`j.Owner == h.Owner`,
	`j.Owner != h.Owner`,
	`j.ClusterId == h.ClusterId`,
	`j.ClusterId > h.ClusterId`,
	`j.ClusterId + h.ClusterId == 49`,
	`J.Owner == H.Owner`,                  // alias case-folding
	`j.owner == h.OWNER`,                  // attribute case-folding
	`j.Done && j.ClusterId > h.ClusterId`, // short-circuit over a load
	`j.Done || j.Missing`,                 // short-circuit skipping a load
	`j.Missing == h.Missing`,              // undefined both sides
	`j.Missing`,                           // bare undefined
	`h.Site == "wisc"`,
	`j.Weight * 2 == 3.0`,
	`strcat(j.Owner, h.Site) == "alicewisc"`, // a strict function over two loads
	`ifThenElse(j.Done, j.Owner, h.Site) == "alice"`,
	`isUndefined(j.Missing)`,
	`j.Owner =?= h.Owner`,
	`j.Missing =?= undefined`,
	`!(j.ClusterId == h.ClusterId)`,
	`j.ClusterId == 42 && h.ClusterId == 7 && j.Owner == h.Owner`,
	// Not aliases, so these must stay delegated and still be right.
	`j.Owner == "alice" && other.x == 1`,
	`j.Weight.inner == 1`,
}

// TestAliasedMatchesCompositeEvaluation is the differential oracle: for every expression, the
// aliased program resolving two separate ads must agree with the SAME expression compiled the
// ordinary way and evaluated against a materialized composite ad. If these ever disagree, the
// optimization is changing answers, which is the only way it can be wrong.
func TestAliasedMatchesCompositeEvaluation(t *testing.T) {
	j, h := sides()
	for _, nullInner := range []bool{false, true} {
		jj, hh := j, h
		if nullInner {
			hh = nil // a LEFT JOIN row that matched nothing
		}
		row := composite(jj, hh)
		for _, src := range aliasExprs {
			t.Run(fmt.Sprintf("null=%v/%s", nullInner, src), func(t *testing.T) {
				plain, err := Parse(src)
				if err != nil {
					t.Fatalf("parsing %q: %v", src, err)
				}
				want := plain.Eval(row)

				aq, err := ParseAliases(src, []string{"j", "h"})
				if err != nil {
					t.Fatalf("parsing %q with aliases: %v", src, err)
				}
				got, ok := aq.Matcher().EvalAliased(aliasResolver(jj, hh), nil)
				if !ok {
					// Declined: something in it needs a real record. The caller
					// falls back to the composite, so what must hold is that the
					// decline is honest -- the query says so -- and that nothing
					// was evaluated.
					if aq.AliasNative() {
						t.Fatalf("%q: declined although AliasNative", src)
					}
					return
				}
				if !aq.AliasNative() {
					t.Fatalf("%q: evaluated although not AliasNative", src)
				}
				if !sameValue(got, want) {
					t.Fatalf("%q: aliased %v, composite %v", src, got, want)
				}
			})
		}
	}
}

// TestAliasedProgramWithoutResolverMatchesPlain pins the safety property that makes
// OpLoadAlias unconditional: run an aliased program the ORDINARY way, against a real
// composite ad and with no alias resolver, and it must answer exactly as the unaliased
// program does. Without this, compiling aliases would be a trap for any caller that did not
// know to install a resolver.
func TestAliasedProgramWithoutResolverMatchesPlain(t *testing.T) {
	j, h := sides()
	row := composite(j, h)
	for _, src := range aliasExprs {
		plain, err := Parse(src)
		if err != nil {
			t.Fatalf("parsing %q: %v", src, err)
		}
		aq, err := ParseAliases(src, []string{"j", "h"})
		if err != nil {
			t.Fatalf("parsing %q with aliases: %v", src, err)
		}
		want, got := plain.Eval(row), aq.Eval(row)
		if !sameValue(got, want) {
			t.Fatalf("%q without a resolver: %v, plain %v", src, got, want)
		}
	}
}

// TestAliasedIsNotNative keeps EvalResolved from being handed a query it cannot answer: its
// resolver takes a name and a scope and has no way to say which side a name came from, so a
// query carrying qualified references must not pass the gate that admits it.
func TestAliasedIsNotNative(t *testing.T) {
	aq, err := ParseAliases(`j.Owner == h.Owner`, []string{"j", "h"})
	if err != nil {
		t.Fatal(err)
	}
	if !aq.Aliased() {
		t.Fatal("expected the query to carry qualified references")
	}
	if aq.Native() {
		t.Fatal("an aliased query must not report Native: EvalResolved cannot answer it")
	}
	if !aq.AliasNative() {
		t.Fatal("expected AliasNative")
	}
}

// TestUnknownAliasStaysDelegated: a select whose record is not a declared alias may be a
// genuine select over a nested record, which only the scope can answer, so it must not be
// turned into a load against a side that does not exist.
func TestUnknownAliasStaysDelegated(t *testing.T) {
	aq, err := ParseAliases(`other.x == 1`, []string{"j", "h"})
	if err != nil {
		t.Fatal(err)
	}
	if aq.Aliased() {
		t.Fatal("a select over an undeclared name must not compile to OpLoadAlias")
	}
	if aq.AliasNative() {
		t.Fatal("expected it to stay delegated")
	}
}

// TestScopedRecordStaysDelegated: MY.j names the scope's own attribute j, not the join side,
// so it is a select over a record and must stay delegated.
func TestScopedRecordStaysDelegated(t *testing.T) {
	for _, src := range []string{`MY.j.Owner == 1`, `TARGET.j.Owner == 1`} {
		aq, err := ParseAliases(src, []string{"j", "h"})
		if err != nil {
			t.Fatal(err)
		}
		if aq.Aliased() {
			t.Fatalf("%q must not compile to OpLoadAlias", src)
		}
	}
}

// TestNoAliasesCompilesAsBefore: with no alias list the compiler must behave exactly as
// CompileProgram always has, so every existing caller is untouched.
func TestNoAliasesCompilesAsBefore(t *testing.T) {
	for _, src := range aliasExprs {
		a, err := ParseAliases(src, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		if a.Aliased() {
			t.Fatalf("%q compiled an alias load with no aliases declared", src)
		}
		if a.Native() != b.Native() {
			t.Fatalf("%q: Native %v with an empty alias list, %v without", src, a.Native(), b.Native())
		}
	}
}

// TestDuplicateAliasKeepsFirstIndex: a repeated alias must not renumber the earlier one, or a
// reference already compiled against index 0 would start reading side 1.
func TestDuplicateAliasKeepsFirstIndex(t *testing.T) {
	ix := newAliasIndex([]string{"j", "h", "J"})
	if ix["j"] != 0 {
		t.Fatalf("expected the first spelling of j to keep index 0, got %d", ix["j"])
	}
}

// TestAliasResolverSeesOriginalCasing: the resolver knows how its own backing stores names,
// so it is handed the name as written, exactly as SetResolver's is.
func TestAliasResolverSeesOriginalCasing(t *testing.T) {
	aq, err := ParseAliases(`j.OwNeR == "x"`, []string{"j"})
	if err != nil {
		t.Fatal(err)
	}
	var seen string
	if _, ok := aq.Matcher().EvalAliased(func(_ int, name string) classad.Value {
		seen = name
		return classad.NewStringValue("x")
	}, nil); !ok {
		t.Fatal("expected the query to be AliasNative")
	}
	if seen != "OwNeR" {
		t.Fatalf("resolver saw %q, want the name as written", seen)
	}
}

// TestEvalAliasedResolvesUnqualifiedRefs: a WHERE conjunct over the joined row may name an
// attribute of the row itself rather than of a side, so the unqualified resolver must still
// be reachable alongside the alias one.
func TestEvalAliasedResolvesUnqualifiedRefs(t *testing.T) {
	j, h := sides()
	aq, err := ParseAliases(`j.Owner == h.Owner && Extra == 9`, []string{"j", "h"})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := aq.Matcher().EvalAliased(aliasResolver(j, h),
		func(name string, _ ast.AttributeScope) classad.Value {
			if name == "Extra" {
				return classad.NewIntValue(9)
			}
			return classad.NewUndefinedValue()
		})
	if !ok {
		t.Fatal("expected the query to be AliasNative")
	}
	if b, err := got.BoolValue(); err != nil || !b {
		t.Fatalf("got %v, want true", got)
	}
}

// TestEvalAliasedDeclinesDelegated is the reason EvalAliased reports ok rather than
// documenting a precondition. Each of these carries something only a real record can answer
// -- a non-strict function, a select over a name that is not a side, a select over a nested
// record -- and evaluating it against the nil scope EvalAliased installs would not fail. It
// would read undefined, the condition would stop matching, and the join would quietly return
// too few rows.
func TestEvalAliasedDeclinesDelegated(t *testing.T) {
	j, h := sides()
	for _, src := range []string{
		`strcat(j.Owner, h.Site) == "alicewisc"`, // strcat is not a strict function
		`j.Owner == "alice" && other.x == 1`,     // a select over an undeclared name
		`j.Weight.inner == 1`,                    // a select over a nested record
	} {
		aq, err := ParseAliases(src, []string{"j", "h"})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := aq.Matcher().EvalAliased(aliasResolver(j, h), nil); ok {
			t.Fatalf("%q: EvalAliased accepted a query it cannot answer", src)
		}
		// And the fallback the caller must take is right.
		plain, err := Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		if got := plain.Eval(composite(j, h)); got.Type() == classad.UndefinedValue &&
			src == `strcat(j.Owner, h.Site) == "alicewisc"` {
			t.Fatalf("%q: the composite fallback read undefined too", src)
		}
	}
}
