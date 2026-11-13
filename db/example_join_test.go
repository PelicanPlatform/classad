package db_test

import (
	"fmt"
	"sort"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// joinFixture builds an in-memory catalog holding the tables these examples join.
//
// A catalog with no Dir keeps every table in RAM, which is what makes a join runnable in an
// example: nothing is written, nothing has to be cleaned up, and the plan the store picks is
// the same one it would pick over a directory.
func joinFixture() *db.Catalog {
	cat, err := db.OpenCatalog("")
	if err != nil {
		panic(err)
	}
	rows := map[string][]string{
		// Jobs, keyed by cluster. 4 is owned by someone with no row in owners, which is
		// what the LEFT example turns on.
		"jobs": {
			`[ ClusterId = 1; Owner = "alice"; RequestCpus = 2 ]`,
			`[ ClusterId = 2; Owner = "bob";   RequestCpus = 8 ]`,
			`[ ClusterId = 3; Owner = "alice"; RequestCpus = 4 ]`,
			`[ ClusterId = 4; Owner = "carol"; RequestCpus = 1 ]`,
		},
		"owners": {
			`[ Owner = "alice"; AcctGroup = "physics" ]`,
			`[ Owner = "bob";   AcctGroup = "chem" ]`,
		},
		"groups": {
			`[ AcctGroup = "physics"; Quota = 400 ]`,
			`[ AcctGroup = "chem";    Quota = 100 ]`,
		},
		// Job run windows, and events stamped with a time. The events do NOT arrive in
		// time order -- the band sorts the side itself.
		"runs": {
			`[ ClusterId = 1; Start = 100; End = 140 ]`,
			`[ ClusterId = 2; Start = 200; End = 220 ]`,
		},
		"events": {
			`[ Ts = 210; Kind = "evict" ]`,
			`[ Ts = 105; Kind = "start" ]`,
			`[ Ts = 500; Kind = "idle" ]`,
			`[ Ts = 139; Kind = "done" ]`,
			`[ Ts = 205; Kind = "start" ]`,
		},
	}
	for _, name := range []string{"jobs", "owners", "groups", "runs", "events"} {
		t, terr := cat.CreateTable(name)
		if terr != nil {
			panic(terr)
		}
		tx := t.Begin()
		for i, src := range rows[name] {
			ad, perr := classad.Parse(src)
			if perr != nil {
				panic(perr)
			}
			tx.NewClassAd(fmt.Sprintf("%s%d", name, i), ad)
		}
		if cerr := tx.Commit(); cerr != nil {
			panic(cerr)
		}
	}
	return cat
}

// show renders one joined row by evaluating each expression against it.
//
// A joined row is itself a ClassAd with one member per side, named by that side's Alias, so
// a caller reads it with ordinary ClassAd expressions -- `j.Owner`, `o.AcctGroup` -- and not
// with an accessor of the join's own.
func show(row *classad.ClassAd, exprs ...string) string {
	out := ""
	for i, src := range exprs {
		if i > 0 {
			out += " "
		}
		ex, err := classad.ParseExpr(src)
		if err != nil {
			panic(err)
		}
		out += src + "=" + ex.Eval(row).String()
	}
	return out
}

// drain collects every joined row, rendered, and panics on the error a limit reports.
//
// Sorted because row order is the scan's and the planner's, not the caller's: a join may walk
// its sides in parallel, and which worker finishes first is a race. Any caller that needs an
// order sorts, or asks for one with an ORDER BY of its own.
func drain(cat *db.Catalog, spec db.JoinSpec, exprs ...string) []string {
	var stats db.JoinStats
	seq, err := cat.Join(spec, db.JoinLimits{}, &stats)
	if err != nil {
		panic(err) // a table that does not exist, an expression that does not parse
	}
	var out []string
	for row := range seq {
		out = append(out, show(row, exprs...))
	}
	// Checked AFTER draining: a limit met part way through has already yielded rows, so it
	// cannot be reported by the call that returned the sequence. Skipping this check reads a
	// truncated answer as a complete one.
	if stats.Err != nil {
		panic(stats.Err)
	}
	sort.Strings(out)
	return out
}

// ExampleCatalog_Join pairs two tables on an equality.
//
// This is the two-table spelling: Left, Right, and the condition between them. Cond is the
// join condition in full and is what decides a pair; Equi names the equality conjuncts of it
// that may also drive the hash. Equi is an optimisation and Cond is the meaning, so a pair
// that the hash brought together is still checked against Cond.
func ExampleCatalog_Join() {
	cat := joinFixture()
	defer cat.Close()

	spec := db.JoinSpec{
		Kind:  db.JoinInner,
		Left:  db.JoinSide{Table: "jobs", Alias: "j"},
		Right: db.JoinSide{Table: "owners", Alias: "o"},
		Cond:  []string{"j.Owner == o.Owner"},
		// Left reads the row built so far, Right reads this step's own table -- so Right
		// is a bare attribute, not "o.Owner". See JoinEqui.
		Equi: []db.JoinEqui{{Left: "j.Owner", Right: "Owner"}},
	}
	for _, line := range drain(cat, spec, "j.ClusterId", "o.AcctGroup") {
		fmt.Println(line)
	}

	// Output:
	// j.ClusterId=1 o.AcctGroup="physics"
	// j.ClusterId=2 o.AcctGroup="chem"
	// j.ClusterId=3 o.AcctGroup="physics"
}

// ExampleCatalog_Join_leftOuter keeps the left row when the right side matches nothing.
//
// Cluster 4's owner has no row in owners. Under JoinInner it is absent from the answer;
// under JoinLeft it is present once, with every attribute of the right side undefined.
func ExampleCatalog_Join_leftOuter() {
	cat := joinFixture()
	defer cat.Close()

	spec := db.JoinSpec{
		Kind:  db.JoinLeft,
		Left:  db.JoinSide{Table: "jobs", Alias: "j"},
		Right: db.JoinSide{Table: "owners", Alias: "o"},
		Cond:  []string{"j.Owner == o.Owner"},
		Equi:  []db.JoinEqui{{Left: "j.Owner", Right: "Owner"}},
	}
	for _, line := range drain(cat, spec, "j.ClusterId", "o.AcctGroup") {
		fmt.Println(line)
	}

	// Output:
	// j.ClusterId=1 o.AcctGroup="physics"
	// j.ClusterId=2 o.AcctGroup="chem"
	// j.ClusterId=3 o.AcctGroup="physics"
	// j.ClusterId=4 o.AcctGroup=undefined
}

// ExampleCatalog_Join_chain folds in a third table.
//
// Steps is the chain spelling: Left is the FROM table and each step adds one more. A chain is
// LEFT-DEEP, so the row grows one member at a time and each step's condition reads the row as
// it is after that step.
//
// The second step is where the scope asymmetry shows. Its Equi.Left is "o.AcctGroup" -- it has
// to be qualified, because by then the row carries two sides and a bare AcctGroup would not
// say which. Equi.Right stays bare: it reads the table being folded in, which is unambiguous
// by construction.
func ExampleCatalog_Join_chain() {
	cat := joinFixture()
	defer cat.Close()

	spec := db.JoinSpec{
		Left: db.JoinSide{Table: "jobs", Alias: "j"},
		Steps: []db.JoinStep{
			{
				Kind: db.JoinInner,
				Side: db.JoinSide{Table: "owners", Alias: "o"},
				Cond: []string{"j.Owner == o.Owner"},
				Equi: []db.JoinEqui{{Left: "j.Owner", Right: "Owner"}},
			},
			{
				Kind: db.JoinInner,
				Side: db.JoinSide{Table: "groups", Alias: "g"},
				Cond: []string{"o.AcctGroup == g.AcctGroup"},
				Equi: []db.JoinEqui{{Left: "o.AcctGroup", Right: "AcctGroup"}},
			},
		},
		// Post are the WHERE conjuncts: applied to the joined row, after the pair is formed.
		Post: []string{"g.Quota > 200"},
	}
	for _, line := range drain(cat, spec, "j.ClusterId", "g.AcctGroup", "g.Quota") {
		fmt.Println(line)
	}

	// Output:
	// j.ClusterId=1 g.AcctGroup="physics" g.Quota=400
	// j.ClusterId=3 g.AcctGroup="physics" g.Quota=400
}

// ExampleCatalog_Join_band joins on a range instead of an equality.
//
// "Which events fall inside each job's run window" has no equality in it at all. Written as
// Cond alone it is a nested loop over every pair, bounded only by the pair budget. Band says
// the same thing as a window over one attribute of the folded side: the side is sorted on Ts
// once, and each run binary-searches the events its window covers.
//
// Cond still carries the predicate. The band narrows the candidates; it does not replace the
// condition, and dropping Cond here would mean something subtly different -- a band is over
// numbers, and declines what ClassAd's own comparison would answer differently.
func ExampleCatalog_Join_band() {
	cat := joinFixture()
	defer cat.Close()

	spec := db.JoinSpec{
		Kind:  db.JoinInner,
		Left:  db.JoinSide{Table: "runs", Alias: "r"},
		Right: db.JoinSide{Table: "events", Alias: "e"},
		Cond:  []string{"e.Ts >= r.Start", "e.Ts <= r.End"},
		// Attr names the folded side's attribute; Lo and Hi are expressions over the
		// sides already in hand. An empty end is unbounded, which is how a one-sided
		// inequality is written.
		Band: []db.JoinBand{{Attr: "Ts", Lo: "r.Start", Hi: "r.End"}},
	}

	var stats db.JoinStats
	seq, err := cat.Join(spec, db.JoinLimits{}, &stats)
	if err != nil {
		panic(err)
	}
	var out []string
	for row := range seq {
		out = append(out, show(row, "r.ClusterId", "e.Ts", "e.Kind"))
	}
	if stats.Err != nil {
		panic(stats.Err)
	}
	sort.Strings(out)
	for _, line := range out {
		fmt.Println(line)
	}

	// The pairs examined are the ones inside a window, not the product of the two sides.
	// Five events against two runs is ten pairs scanned; the band examines four.
	fmt.Printf("pairs examined: %d\n", stats.Pairs)

	// Output:
	// r.ClusterId=1 e.Ts=105 e.Kind="start"
	// r.ClusterId=1 e.Ts=139 e.Kind="done"
	// r.ClusterId=2 e.Ts=205 e.Kind="start"
	// r.ClusterId=2 e.Ts=210 e.Kind="evict"
	// pairs examined: 4
}

// ExampleCatalog_ExplainJoin reports the plan without running the join.
//
// It is the way to find out which of the three paths a spec will take -- and, for a band,
// whether the step is windowed at all. A band engages only on a step with no equality, so a
// spec carrying both answers the same rows by hashing, and Banded says so.
func ExampleCatalog_ExplainJoin() {
	cat := joinFixture()
	defer cat.Close()

	spec := db.JoinSpec{
		Kind:  db.JoinInner,
		Left:  db.JoinSide{Table: "runs", Alias: "r"},
		Right: db.JoinSide{Table: "events", Alias: "e"},
		Cond:  []string{"e.Ts >= r.Start", "e.Ts <= r.End"},
		Band:  []db.JoinBand{{Attr: "Ts", Lo: "r.Start", Hi: "r.End"}},
	}
	ex, err := cat.ExplainJoin(spec, db.JoinLimits{})
	if err != nil {
		panic(err)
	}
	for i := range ex.Steps {
		st := &ex.Steps[i]
		fmt.Printf("step %s banded=%v bands=%d\n", st.Alias, st.Banded, len(st.Band))
	}

	// Output:
	// step e banded=true bands=1
}
