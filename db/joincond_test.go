package db

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// withCompositeCond runs fn with the aliased condition path disabled.
func withCompositeCond(t *testing.T, fn func()) {
	t.Helper()
	forceCompositeCond = true
	defer func() { forceCompositeCond = false }()
	fn()
}

// TestAliasedCondMatchesCompositeRows is the equivalence oracle for the aliased condition:
// the same join, run with the condition read off the two sides and run against a built
// composite row, must return the same rows. An optimization that changes which pairs survive
// is the only way this can be wrong, and nothing but comparing the two paths can see it.
func TestAliasedCondMatchesCompositeRows(t *testing.T) {
	cat := joinFixture(t)
	conds := [][]string{
		{`j.ClusterId == h.ClusterId`},
		{`j.ClusterId == h.ClusterId`, `h.ExitCode == 0`},
		{`j.ClusterId == h.ClusterId`, `j.Cpus > 2`},
		{`j.ClusterId == h.ClusterId`, `j.Owner == "alice"`},
		{`j.ClusterId == h.ClusterId && j.Cpus >= h.ExitCode`},
		{`j.Cpus > h.ExitCode`}, // no equality at all
		{`j.ClusterId == h.ClusterId`, `j.Missing == h.Missing`},
		// Declines the aliased path, so this also checks the fallback still joins.
		{`j.ClusterId == h.ClusterId`, `strcat(j.Owner, "x") == "alicex"`},
	}
	for _, kind := range []JoinKind{JoinInner, JoinLeft} {
		for _, cond := range conds {
			t.Run(fmt.Sprintf("%v/%v", kind, cond), func(t *testing.T) {
				spec := JoinSpec{
					Kind:  kind,
					Left:  JoinSide{Table: "jobs", Alias: "j"},
					Right: JoinSide{Table: "hist", Alias: "h"},
					Cond:  cond,
				}
				fast, _ := drain(t, cat, spec)
				var slow []string
				withCompositeCond(t, func() { slow, _ = drain(t, cat, spec) })
				if len(fast) != len(slow) {
					t.Fatalf("aliased %d rows, composite %d rows\naliased: %v\ncomposite: %v",
						len(fast), len(slow), fast, slow)
				}
				for i := range fast {
					if fast[i] != slow[i] {
						t.Fatalf("row %d: aliased %q, composite %q", i, fast[i], slow[i])
					}
				}
			})
		}
	}
}

// TestAliasedCondReadsExpressionValuedAttrs is the case the aliased path could plausibly get
// wrong and the fixture above cannot see. An attribute holding an EXPRESSION over its own
// siblings evaluates in the scope of the ad that holds it. Against the composite that falls
// out of how a select works; through the resolver it depends on the resolver evaluating the
// attribute against its own side, so this pins it.
func TestAliasedCondReadsExpressionValuedAttrs(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	jobs, err := cat.CreateTable("jobs")
	if err != nil {
		t.Fatal(err)
	}
	hist, err := cat.CreateTable("hist")
	if err != nil {
		t.Fatal(err)
	}
	put := func(d *DB, key, text string) {
		ad, perr := classad.Parse(text)
		if perr != nil {
			t.Fatal(perr)
		}
		if werr := d.Put(key, ad); werr != nil {
			t.Fatal(werr)
		}
	}
	// Total is an expression over a sibling of its OWN ad, and both sides define Cpus, so a
	// resolver that evaluated it in the wrong scope would get a different number rather than
	// an error -- which is exactly the kind of wrong that returns plausible rows.
	put(jobs, "1", `[ ClusterId = 1; Cpus = 2; Total = Cpus * 10 ]`)
	put(jobs, "2", `[ ClusterId = 2; Cpus = 4; Total = Cpus * 10 ]`)
	put(hist, "a", `[ ClusterId = 1; Cpus = 99; Total = Cpus * 10 ]`)
	put(hist, "b", `[ ClusterId = 2; Cpus = 99; Total = Cpus * 10 ]`)

	spec := JoinSpec{
		Kind:  JoinInner,
		Left:  JoinSide{Table: "jobs", Alias: "j"},
		Right: JoinSide{Table: "hist", Alias: "h"},
		Cond:  []string{`j.ClusterId == h.ClusterId`, `j.Total == 20`, `h.Total == 990`},
	}
	fast, _ := drain(t, cat, spec)
	var slow []string
	withCompositeCond(t, func() { slow, _ = drain(t, cat, spec) })
	if len(fast) != len(slow) {
		t.Fatalf("aliased %d rows, composite %d rows", len(fast), len(slow))
	}
	if len(fast) != 1 {
		t.Fatalf("expected exactly the ClusterId=1 pair, got %v", fast)
	}
}

// TestAliasedCondHoldsDeclines covers the branch no join can reach. compileAliasedCond
// admits only AliasNative terms, so holds never sees a decline through the ordinary path --
// which means a mutation removing the check passes every join test. The branch still earns
// its place: a declined term reads undefined rather than failing, so dropping it would make
// a join return too few rows silently. Reaching it needs the condition built by hand.
func TestAliasedCondHoldsDeclines(t *testing.T) {
	// strcat is not a strict function, so the whole call is delegated and the query is not
	// AliasNative -- exactly what compileAliasedCond refuses to admit.
	q, err := vm.ParseAliases(`strcat(j.Owner, "x") == "alicex"`, []string{"j", "h"})
	if err != nil {
		t.Fatal(err)
	}
	if q.AliasNative() {
		t.Fatal("fixture term is AliasNative; it cannot exercise the decline")
	}
	a := &aliasedCond{ms: []*vm.Matcher{q.Matcher()}}

	ad, err := classad.Parse(`[ Owner = "alice" ]`)
	if err != nil {
		t.Fatal(err)
	}
	row := &joinRow{ad: ad}
	if _, ok := a.holds([]*joinRow{row, row}); ok {
		t.Fatal("holds accepted a term EvalAliased declined; the caller would keep an undefined answer")
	}
	// And the whole point: compileAliasedCond would not have produced this.
	if compileAliasedCond([]string{`strcat(j.Owner, "x") == "alicex"`}, []string{"j", "h"}) != nil {
		t.Fatal("compileAliasedCond admitted a term that is not AliasNative")
	}
}
