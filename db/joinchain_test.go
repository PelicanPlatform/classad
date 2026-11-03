package db

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// JOINING MORE THAN TWO TABLES.
//
// Every expectation below is written from SQL's own semantics and the fixture, not from what
// another implementation produces. That is deliberate and it is not pedantry: the two-table
// join and the client's had the SAME null-extension bug at the same time, so the differential
// test between them stayed green while both were wrong. An oracle is only an oracle for what
// it does differently.

// chainFixture builds three tables with a deliberate shape:
//
//	history       ClusterId 1..5
//	epoch_history ClusterId 1,2,3   Size = 10*ClusterId
//	job_metrics   ClusterId 1,2
//
// So the chain has a row that matches at both steps (1, 2), one that matches at the first and
// not the second (3), and two that match at neither (4, 5).
func chainFixture(t *testing.T) *Catalog {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	mk := func(name string) *DB {
		d, cerr := cat.CreateTable(name)
		if cerr != nil {
			t.Fatal(cerr)
		}
		return d
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
	hist, epoch, metric := mk("history"), mk("epoch_history"), mk("job_metrics")
	for i := 1; i <= 5; i++ {
		put(hist, fmt.Sprintf("h%d", i), fmt.Sprintf(`[ ClusterId = %d; Owner = "u%d" ]`, i, i))
	}
	for i := 1; i <= 3; i++ {
		put(epoch, fmt.Sprintf("e%d", i), fmt.Sprintf(`[ ClusterId = %d; Size = %d ]`, i, 10*i))
	}
	for i := 1; i <= 2; i++ {
		put(metric, fmt.Sprintf("m%d", i), fmt.Sprintf(`[ ClusterId = %d; Eff = %d ]`, i, 50+i))
	}
	return cat
}

// chainSpec is `history h <k1> epoch_history e ON h.ClusterId == e.ClusterId
// <k2> job_metrics m ON e.ClusterId == m.ClusterId`, with the residual WHERE given.
func chainSpec(k1, k2 JoinKind, post ...string) JoinSpec {
	return JoinSpec{
		Left: JoinSide{Table: "history", Alias: "h", Project: []string{"ClusterId"}},
		Steps: []JoinStep{
			{
				Kind: k1,
				Side: JoinSide{Table: "epoch_history", Alias: "e", Project: []string{"ClusterId", "Size"}},
				Cond: []string{"h.ClusterId == e.ClusterId"},
				Equi: []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
			},
			{
				Kind: k2,
				Side: JoinSide{Table: "job_metrics", Alias: "m", Project: []string{"ClusterId", "Eff"}},
				Cond: []string{"e.ClusterId == m.ClusterId"},
				Equi: []JoinEqui{{Left: "e.ClusterId", Right: "ClusterId"}},
			},
		},
		Post: post,
	}
}

// chainRows runs a chain and renders each row as "h:e:m", with "-" for an absent side, which
// is the one thing these tests are about.
func chainRows(t *testing.T, cat *Catalog, spec JoinSpec) []string {
	t.Helper()
	var stats JoinStats
	seq, err := cat.Join(spec, JoinLimits{}, &stats)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	var out []string
	for ad := range seq {
		part := func(alias, attr string) string {
			ex, perr := classad.ParseExpr(alias + "." + attr)
			if perr != nil {
				t.Fatal(perr)
			}
			v := ex.Eval(ad)
			if v.IsUndefined() {
				return "-"
			}
			n, verr := v.IntValue()
			if verr != nil {
				return "?"
			}
			return fmt.Sprint(n)
		}
		out = append(out, strings.Join([]string{
			part("h", "ClusterId"), part("e", "ClusterId"), part("m", "ClusterId"),
		}, ":"))
	}
	if stats.Err != nil {
		t.Fatalf("join stopped early: %v", stats.Err)
	}
	sort.Strings(out)
	return out
}

func wantRows(t *testing.T, what string, got, want []string) {
	t.Helper()
	sort.Strings(want)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("%s:\n got  %v\n want %v", what, got, want)
	}
}

// TestChainInnerKeepsOnlyRowsThatMatchedEveryStep states the inner case: a row survives only
// if it paired at BOTH steps, so clusters 3, 4 and 5 are gone.
func TestChainInnerKeepsOnlyRowsThatMatchedEveryStep(t *testing.T) {
	cat := chainFixture(t)
	wantRows(t, "h JOIN e JOIN m",
		chainRows(t, cat, chainSpec(JoinInner, JoinInner)),
		[]string{"1:1:1", "2:2:2"})
}

// TestChainNullExtendsOnlyWhereItsOwnStepIsLeft is the asymmetry: in a left-deep chain a side
// is null-extendable exactly when ITS OWN step is a LEFT join, and every earlier side is
// preserved by every later step whatever that step is.
func TestChainNullExtendsOnlyWhereItsOwnStepIsLeft(t *testing.T) {
	cat := chainFixture(t)

	// INNER then LEFT: a row must have paired with e to survive, and then keeps its place
	// whether or not m matched. Cluster 3 has an e and no m.
	wantRows(t, "h JOIN e LEFT JOIN m",
		chainRows(t, cat, chainSpec(JoinInner, JoinLeft)),
		[]string{"1:1:1", "2:2:2", "3:3:-"})

	// LEFT then INNER: every h row reaches the second step, but only a row that pairs with
	// an m survives it. 3, 4 and 5 are dropped by the INNER step, not by the LEFT one.
	wantRows(t, "h LEFT JOIN e JOIN m",
		chainRows(t, cat, chainSpec(JoinLeft, JoinInner)),
		[]string{"1:1:1", "2:2:2"})

	// LEFT then LEFT: every h row survives. 4 and 5 reach the second step with e absent, so
	// its ON reads e.ClusterId as undefined, matches nothing, and m is absent too.
	wantRows(t, "h LEFT JOIN e LEFT JOIN m",
		chainRows(t, cat, chainSpec(JoinLeft, JoinLeft)),
		[]string{"1:1:1", "2:2:2", "3:3:-", "4:-:-", "5:-:-"})
}

// TestChainAntiJoinReportsOnlyTheUnmatchedRows is the trap, stated as SQL.
//
// `... LEFT JOIN m ON ... WHERE m.ClusterId IS NULL` asks for the rows that found no m. The
// bug it guards is a row that MATCHED the ON clause and was then dropped by WHERE coming back
// null-extended -- at which point `m.ClusterId =?= undefined` holds for it, and the anti-join
// reports every row instead of the unmatched ones.
func TestChainAntiJoinReportsOnlyTheUnmatchedRows(t *testing.T) {
	cat := chainFixture(t)
	// Cluster 3 DID pair with an epoch row and found no metrics row, so it keeps its e and
	// loses only its m. 4 and 5 paired with neither.
	wantRows(t, "the rows with no job_metrics row",
		chainRows(t, cat, chainSpec(JoinLeft, JoinLeft, "m.ClusterId =?= undefined")),
		[]string{"3:3:-", "4:-:-", "5:-:-"})
}

// TestChainWhereDoesNotResurrectARowAsNullExtended is the same trap from the other side, and
// the one a passing anti-join test can still hide.
//
// `WHERE e.Size > 25` drops clusters 1 and 2, whose e DID match. They must be GONE, not back
// with e absent -- a dropped row is dropped, not null-extended.
func TestChainWhereDoesNotResurrectARowAsNullExtended(t *testing.T) {
	cat := chainFixture(t)
	wantRows(t, "the rows whose epoch row is large enough",
		chainRows(t, cat, chainSpec(JoinLeft, JoinLeft, "e.Size > 25")),
		[]string{"3:3:-"})
}

// TestChainRefusesAForwardReference pins that an ON clause naming a table a LATER step folds
// in is an error rather than a deferral. Deferring it would answer a different query, and
// evaluating it where it stands reads undefined -- so the clause silently never holds.
func TestChainRefusesAForwardReference(t *testing.T) {
	cat := chainFixture(t)
	spec := chainSpec(JoinInner, JoinInner)
	spec.Steps[0].Cond = []string{"h.ClusterId == e.ClusterId && m.Eff > 0"}
	_, err := cat.Join(spec, JoinLimits{}, nil)
	if err == nil {
		t.Fatal("a forward reference to m was accepted; it reads undefined and the ON clause never holds")
	}
	if !strings.Contains(err.Error(), "forward reference") {
		t.Errorf("error does not say what is wrong: %v", err)
	}
}

// TestChainRefusesBothSpellingsAtOnce pins that a spec carrying the two-table spelling AND a
// chain is refused. There is no reading of it that is obviously the caller's, and picking one
// would answer a query nobody wrote.
func TestChainRefusesBothSpellingsAtOnce(t *testing.T) {
	cat := chainFixture(t)
	spec := chainSpec(JoinInner, JoinInner)
	spec.Right = JoinSide{Table: "epoch_history", Alias: "e2"}
	if _, err := cat.Join(spec, JoinLimits{}, nil); err == nil {
		t.Fatal("a spec with both spellings was accepted")
	}
}

// TestChainRefusesAnUnqualifiedRowHalfPastTheFirstStep pins the scope rule on the equality
// halves. Past the first step the row holds several sides, so a bare `ClusterId` does not say
// which it means; it would read as an attribute of the composite row -- undefined -- putting
// every row in the no-key bucket and matching nothing, with no error.
func TestChainRefusesAnUnqualifiedRowHalfPastTheFirstStep(t *testing.T) {
	cat := chainFixture(t)
	spec := chainSpec(JoinInner, JoinInner)
	spec.Steps[1].Equi = []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}}
	_, err := cat.Join(spec, JoinLimits{}, nil)
	if err == nil {
		t.Fatal("an unqualified row half was accepted at the second step; it matches nothing silently")
	}
	if !strings.Contains(err.Error(), "names no side") {
		t.Errorf("error does not say what is wrong: %v", err)
	}
}

// TestChainReadsOnlyWhatItNeeds pins that the read projection is planned per side across the
// whole chain, not only for the two the old plan had. A side read whole here would be a
// silent cost, and a side read too narrowly a silent wrong answer.
func TestChainReadsOnlyWhatItNeeds(t *testing.T) {
	proj := planJoinRead(chainSpec(JoinInner, JoinInner, "m.Eff > 0"))
	if len(proj) != 3 {
		t.Fatalf("planned %d read projections for a three-table chain, want 3", len(proj))
	}
	for i, want := range [][]string{
		{"ClusterId"},
		{"ClusterId", "Size"},
		{"ClusterId", "Eff"},
	} {
		if proj[i] == nil {
			t.Fatalf("side %d is read whole; the chain accounts for every reference to it", i)
		}
		for _, name := range want {
			if !hasProjAttr(proj[i], name) {
				t.Errorf("side %d read projection %v is missing %s", i, proj[i], name)
			}
		}
	}
	// Owner is carried by nothing and read by nothing.
	if hasProjAttr(proj[0], "Owner") {
		t.Errorf("side 0 reads Owner, which nothing asked for: %v", proj[0])
	}
}

// TestChainAcceptsEitherRowHalfSpellingAtTheFirstStep pins that the first step takes a bare
// row half AND a qualified one, and answers the same.
//
// The trap this closes is silent. At the first step the row holds one side, so both spellings
// are unambiguous -- but a qualified half read against that side's OWN ad is undefined, an
// undefined key matches nothing, and the join returns no rows and no error. A caller writing a
// chain qualifies every step, which is exactly the spelling that would have failed.
func TestChainAcceptsEitherRowHalfSpellingAtTheFirstStep(t *testing.T) {
	cat := chainFixture(t)
	bare := chainSpec(JoinInner, JoinInner)
	qualified := chainSpec(JoinInner, JoinInner)
	qualified.Steps[0].Equi = []JoinEqui{{Left: "h.ClusterId", Right: "ClusterId"}}

	got := chainRows(t, cat, qualified)
	wantRows(t, "a qualified row half at the first step", got, []string{"1:1:1", "2:2:2"})
	wantRows(t, "the two spellings agree", got, chainRows(t, cat, bare))
}

// TestChainRefusesAForwardReferenceInAnEqualityHalf pins that the forward-reference rule
// covers the equality halves and not only the ON terms, at EVERY step including the first.
//
// A row half naming a side a later step folds in reads undefined, which matches nothing -- the
// same silent no-rows answer, reached a different way.
func TestChainRefusesAForwardReferenceInAnEqualityHalf(t *testing.T) {
	cat := chainFixture(t)
	spec := chainSpec(JoinInner, JoinInner)
	spec.Steps[0].Equi = []JoinEqui{{Left: "m.ClusterId", Right: "ClusterId"}}
	_, err := cat.Join(spec, JoinLimits{}, nil)
	if err == nil {
		t.Fatal("an equality half naming a later side was accepted; it matches nothing silently")
	}
	if !strings.Contains(err.Error(), "forward reference") {
		t.Errorf("error does not say what is wrong: %v", err)
	}
}
