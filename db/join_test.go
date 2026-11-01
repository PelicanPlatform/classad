package db

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// joinFixture builds two tables with a deliberate shape: one job pairs with nothing (so a
// LEFT join has something to null-extend), one history row pairs with no job (so an inner
// join has something to drop from the hashed side), and one job has TWO history rows (so a
// bucket with several entries is exercised).
func joinFixture(t *testing.T) *Catalog {
	t.Helper()
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
	put(jobs, "1", `[ ClusterId = 1; Owner = "alice"; Cpus = 2 ]`)
	put(jobs, "2", `[ ClusterId = 2; Owner = "bob";   Cpus = 4 ]`)
	put(jobs, "3", `[ ClusterId = 3; Owner = "alice"; Cpus = 8 ]`) // pairs with nothing
	put(hist, "a", `[ ClusterId = 1; ExitCode = 0 ]`)
	put(hist, "b", `[ ClusterId = 2; ExitCode = 1 ]`)
	put(hist, "c", `[ ClusterId = 2; ExitCode = 7 ]`) // a second row for one key
	put(hist, "d", `[ ClusterId = 9; ExitCode = 0 ]`) // pairs with nothing
	return cat
}

// rows renders the joined rows as "owner/exit" pairs, sorted, so a comparison does not depend
// on scan order.
func drain(t *testing.T, cat *Catalog, spec JoinSpec) ([]string, *JoinStats) {
	t.Helper()
	var st JoinStats
	seq, err := cat.Join(spec, JoinLimits{}, &st)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	var out []string
	for row := range seq {
		owner := evalText(row, "j.Owner")
		exit := evalText(row, "h.ExitCode")
		out = append(out, owner+"/"+exit)
	}
	if st.Err != nil {
		t.Fatalf("join stopped early: %v", st.Err)
	}
	sort.Strings(out)
	return out, &st
}

func evalText(row *classad.ClassAd, path string) string {
	ex, err := classad.ParseExpr(path)
	if err != nil {
		return "!" + path
	}
	v := ex.Eval(row)
	switch {
	case v.IsString():
		s, _ := v.StringValue()
		return s
	case v.IsInteger():
		n, _ := v.IntValue()
		return fmt.Sprintf("%d", n)
	case v.IsBool():
		b, _ := v.BoolValue()
		return fmt.Sprintf("%t", b)
	}
	return "undefined"
}

func equiSpec(kind JoinKind) JoinSpec {
	return JoinSpec{
		Kind:  kind,
		Left:  JoinSide{Table: "jobs", Alias: "j"},
		Right: JoinSide{Table: "hist", Alias: "h"},
		Cond:  []string{"j.ClusterId == h.ClusterId"},
		Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}
}

func TestJoinInnerPairsOnTheEquality(t *testing.T) {
	cat := joinFixture(t)
	got, st := drain(t, cat, equiSpec(JoinInner))
	want := []string{"alice/0", "bob/1", "bob/7"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("rows = %v, want %v", got, want)
	}
	// The hashed side is held entirely; the other is streamed. Only one side at a time.
	if st.HashRows != 4 || st.ScanRows != 3 {
		t.Errorf("held %d hashed rows and streamed %d, want 4 and 3", st.HashRows, st.ScanRows)
	}
	// The hash narrowed the work: without it this would be 12 pairs.
	if st.Pairs != 3 {
		t.Errorf("examined %d pairs, want 3 (the hash should have narrowed them)", st.Pairs)
	}
}

func TestJoinLeftNullExtendsAnUnmatchedRow(t *testing.T) {
	cat := joinFixture(t)
	got, _ := drain(t, cat, equiSpec(JoinLeft))
	// Cluster 3 pairs with nothing, so h.ExitCode reads as undefined -- ClassAd's own NULL.
	want := []string{"alice/0", "alice/undefined", "bob/1", "bob/7"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

func TestJoinCrossPairsEverything(t *testing.T) {
	cat := joinFixture(t)
	spec := JoinSpec{
		Kind:  JoinCross,
		Left:  JoinSide{Table: "jobs", Alias: "j"},
		Right: JoinSide{Table: "hist", Alias: "h"},
	}
	got, st := drain(t, cat, spec)
	if len(got) != 12 {
		t.Errorf("produced %d rows, want 12 (3 jobs x 4 history rows)", len(got))
	}
	if st.Pairs != 12 {
		t.Errorf("examined %d pairs, want 12", st.Pairs)
	}
}

// An inner join is a subset of a cross join over the same tables, and a left join is a
// superset of the inner one. Stated as a property because it holds whatever the fixture is.
func TestJoinKindsNest(t *testing.T) {
	cat := joinFixture(t)
	inner, _ := drain(t, cat, equiSpec(JoinInner))
	left, _ := drain(t, cat, equiSpec(JoinLeft))
	if len(left) < len(inner) {
		t.Errorf("left join produced %d rows, fewer than the inner join's %d", len(left), len(inner))
	}
	for _, r := range inner {
		if !slices.Contains(left, r) {
			t.Errorf("inner row %q is missing from the left join", r)
		}
	}
}

// A row whose join key is undefined pairs with nothing, on either side: ClassAd's == does not
// report two undefineds equal, it propagates undefined, and only a true boolean keeps a pair.
func TestJoinUndefinedKeyPairsWithNothing(t *testing.T) {
	cat := joinFixture(t)
	jobs, _ := cat.Table("jobs")
	hist, _ := cat.Table("hist")
	add := func(d *DB, key, text string) {
		ad, _ := classad.Parse(text)
		if err := d.Put(key, ad); err != nil {
			t.Fatal(err)
		}
	}
	add(jobs, "4", `[ Owner = "carol"; Cpus = 1 ]`) // no ClusterId at all
	add(hist, "e", `[ ExitCode = 42 ]`)             // likewise

	got, _ := drain(t, cat, equiSpec(JoinInner))
	for _, r := range got {
		if strings.HasPrefix(r, "carol/") || strings.HasSuffix(r, "/42") {
			t.Errorf("a row with an undefined key paired: %q", r)
		}
	}
	if len(got) != 3 {
		t.Errorf("produced %d rows, want the same 3 as before the keyless rows were added", len(got))
	}
}

// The projection is what bounds the result, since the rows do not cross a wire until they are
// joined. A projected side carries only what was asked for.
func TestJoinProjectionNarrowsTheJoinedRow(t *testing.T) {
	cat := joinFixture(t)
	spec := equiSpec(JoinInner)
	spec.Left.Project = []string{"Owner"}
	spec.Right.Project = []string{"ExitCode"}

	var st JoinStats
	seq, err := cat.Join(spec, JoinLimits{}, &st)
	if err != nil {
		t.Fatal(err)
	}
	for row := range seq {
		// Asked for: present.
		if evalText(row, "j.Owner") == "undefined" {
			t.Errorf("projected attribute j.Owner is missing")
		}
		// Not asked for: absent, which reads as undefined rather than as an error.
		if got := evalText(row, "j.Cpus"); got != "undefined" {
			t.Errorf("j.Cpus = %q, want undefined: it was not projected", got)
		}
		if got := evalText(row, "h.ClusterId"); got != "undefined" {
			t.Errorf("h.ClusterId = %q, want undefined: it was not projected", got)
		}
	}
}

// A limit the store sets cannot be raised by a caller asking for more.
func TestJoinLimitsClampToTheStores(t *testing.T) {
	cat := joinFixture(t)
	spec := JoinSpec{
		Kind:  JoinCross,
		Left:  JoinSide{Table: "jobs", Alias: "j"},
		Right: JoinSide{Table: "hist", Alias: "h"},
	}
	var st JoinStats
	seq, err := cat.Join(spec, JoinLimits{MaxPairs: 5}, &st)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range seq {
		n++
	}
	if st.Err == nil {
		t.Errorf("a 5-pair budget over 12 pairs reported no error; a truncated answer must say so")
	}

	// And asking for more than the store allows does not get it.
	huge := DefaultJoinLimits()
	huge.MaxPairs *= 1000
	clamped := DefaultJoinLimits().clamp(huge)
	if clamped.MaxPairs != DefaultJoinLimits().MaxPairs {
		t.Errorf("a caller raised MaxPairs to %d; the store's own limit must stand", clamped.MaxPairs)
	}
}
