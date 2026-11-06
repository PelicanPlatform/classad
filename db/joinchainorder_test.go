package db

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// withoutChainReorder runs f with the chain planner disabled, so a test can compare the plan
// it chooses against the one that was written. It must not be used from a parallel test; see
// forceNoChainReorder.
func withoutChainReorder(f func()) {
	forceNoChainReorder = true
	defer func() { forceNoChainReorder = false }()
	f()
}

// orderFixture builds three MUTABLE tables, which promise no row order, so the planner is
// free to re-plan a chain over them.
//
//	jobs   n rows, K = 0..n-1, Prio = K%10
//	hist   n rows, K = 0..n-1, Exit = K%3
//	small  m rows, K = 0, stride, 2*stride, ...
func orderFixture(tb testing.TB, n, m int) *Catalog {
	tb.Helper()
	cat, err := OpenCatalog(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { cat.Close() })
	mk := func(name string) *DB {
		d, cerr := cat.CreateTable(name)
		if cerr != nil {
			tb.Fatal(cerr)
		}
		return d
	}
	put := func(d *DB, key, text string) {
		ad, perr := classad.Parse(text)
		if perr != nil {
			tb.Fatal(perr)
		}
		if werr := d.Put(key, ad); werr != nil {
			tb.Fatal(werr)
		}
	}
	jobs, hist, small := mk("jobs"), mk("hist"), mk("small")
	for i := 0; i < n; i++ {
		put(jobs, fmt.Sprintf("j%d", i), fmt.Sprintf(`[ K = %d; Prio = %d ]`, i, i%10))
		put(hist, fmt.Sprintf("h%d", i), fmt.Sprintf(`[ K = %d; Exit = %d ]`, i, i%3))
	}
	// The offset matters: a key that is always a multiple of the stride is always Prio 0,
	// and the ON term comparing Prio to MinPrio would then drop every row -- a fixture that
	// joins nothing and passes any comparison of two plans.
	stride := max(1, n/m)
	for i := 0; i < m; i++ {
		put(small, fmt.Sprintf("s%d", i), fmt.Sprintf(`[ K = %d; MinPrio = 4 ]`, i*stride+i%10))
	}
	return cat
}

// orderSpec is `jobs j, hist h ON j.K == h.K AND h.Exit > 0, small s ON j.K == s.K AND
// j.Prio >= s.MinPrio`: a star on K, with one extra ON term reading a single side and one
// reading two. Where those two terms end up is the whole correctness question in a rewrite.
func orderSpec() JoinSpec {
	return JoinSpec{
		Left: JoinSide{Table: "jobs", Alias: "jobs", Project: []string{"K", "Prio"}},
		Steps: []JoinStep{
			{
				Kind: JoinInner,
				Side: JoinSide{Table: "hist", Alias: "hist", Project: []string{"K", "Exit"}},
				Cond: []string{"jobs.K == hist.K", "hist.Exit > 0"},
				Equi: []JoinEqui{{Left: "jobs.K", Right: "K"}},
			},
			{
				Kind: JoinInner,
				Side: JoinSide{Table: "small", Alias: "small", Project: []string{"K", "MinPrio"}},
				Cond: []string{"jobs.K == small.K", "jobs.Prio >= small.MinPrio"},
				Equi: []JoinEqui{{Left: "jobs.K", Right: "K"}},
			},
		},
	}
}

// joinKeys runs a join and returns its rows as the FROM key of each, sorted, so two plans
// over the same tables can be compared whatever order they produce.
func joinKeys(tb testing.TB, cat *Catalog, spec JoinSpec) ([]string, *JoinStats) {
	tb.Helper()
	var st JoinStats
	seq, err := cat.Join(spec, JoinLimits{}, &st)
	if err != nil {
		tb.Fatalf("Join: %v", err)
	}
	ex, perr := classad.ParseExpr("jobs.K")
	if perr != nil {
		tb.Fatal(perr)
	}
	var out []string
	for ad := range seq {
		n, verr := ex.Eval(ad).IntValue()
		if verr != nil {
			tb.Fatalf("a joined row has no jobs.K: %v", verr)
		}
		out = append(out, fmt.Sprint(n))
	}
	if st.Err != nil {
		tb.Fatalf("join stopped early: %v", st.Err)
	}
	sort.Strings(out)
	return out, &st
}

// A re-planned chain must answer exactly what the chain as written answers.
//
// The two ON terms that are not hash equalities are the reason this is not obvious. They are
// re-attached to whichever step has everything they read in scope, and getting that wrong
// goes wrong in two different ways: a term placed too early reads a side that is not folded
// in yet, which is undefined and silently drops rows, and a term DROPPED on the way through
// lets rows past that the caller excluded. Both show up here as a row count.
func TestChainReorderAnswersTheSameRows(t *testing.T) {
	cat := orderFixture(t, 4000, 40)
	spec := orderSpec()

	var want []string
	var asWritten *JoinStats
	withoutChainReorder(func() { want, asWritten = joinKeys(t, cat, spec) })
	got, planned := joinKeys(t, cat, spec)

	if len(want) == 0 {
		t.Fatal("the fixture produced no rows, so this would pass without joining anything")
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the re-planned chain answered %d rows, the chain as written %d",
			len(got), len(want))
	}
	if planned.ChainFromAlias == "" {
		t.Fatalf("the chain was not re-planned, so this compared one plan with itself: %q",
			planned.ChainOrderWhy)
	}
	// The plans have to actually differ, or the comparison above is vacuous.
	if planned.Pairs == asWritten.Pairs {
		t.Errorf("both plans examined %d pairs; the re-planned chain is the written one",
			planned.Pairs)
	}
	t.Logf("as written %d pairs, re-planned %d (%s)", asWritten.Pairs, planned.Pairs,
		planned.ChainOrderWhy)
}

// Where each ON term lands is asserted directly, because the row count only catches a term
// placed too EARLY (a forward reference, which is refused) or dropped. A term pushed later
// than it needs to be answers the same rows and only costs more, which no answer shows.
func TestChainReorderPlacesEachOnTermAtItsEarliestStep(t *testing.T) {
	cat := orderFixture(t, 4000, 40)
	var st JoinStats
	out := cat.planJoinOrder(orderSpec(), false, &st)

	if st.ChainFromAlias != "jobs" {
		t.Fatalf("read FROM %q, want jobs: the largest table is streamed when no index "+
			"narrows any of them (%s)", st.ChainFromAlias, st.ChainOrderWhy)
	}
	var steps []string
	for _, s := range out.Steps {
		steps = append(steps, s.Side.Alias)
	}
	if strings.Join(steps, ",") != "small,hist" {
		t.Fatalf("steps fold in %v, want small then hist: the smaller table filters first",
			steps)
	}
	// small is folded first, so the term reading jobs and small belongs to ITS step, and
	// the term reading hist cannot be read until hist is folded in at the second.
	wantCond := [][]string{
		{"jobs.K == small.K", "jobs.Prio >= small.MinPrio"},
		{"jobs.K == hist.K", "hist.Exit > 0"},
	}
	for i, want := range wantCond {
		if got := out.Steps[i].Cond; strings.Join(got, " && ") != strings.Join(want, " && ") {
			t.Errorf("step %d (%s) has ON %v, want %v", i, out.Steps[i].Side.Alias, got, want)
		}
	}
}

// Re-centring the chain on a small table is what lets every other table be read through its
// index, which is the whole reason this planner exists.
func TestChainReorderReadsFromTheTableWithFewKeys(t *testing.T) {
	cat := probeChainFixture(t, 8192, 32, true)
	spec := probeChainSpec("big1", "big2", "sel")

	var st JoinStats
	seq, err := cat.Join(spec, JoinLimits{}, &st)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range seq {
		n++
	}
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	if st.ChainFromAlias != "sel" {
		t.Fatalf("read FROM %q, want sel: %s", st.ChainFromAlias, st.ChainOrderWhy)
	}
	for _, p := range st.ProbeSteps {
		if p.Keys == 0 {
			t.Errorf("%s was not narrowed (%s); re-centring the chain is what makes its "+
				"index reachable", p.Alias, p.Why)
		}
	}
	if n != 32 {
		t.Errorf("joined %d rows, want 32", n)
	}
	t.Logf("hashed %d, streamed %d, pairs %d (%s)", st.HashRows, st.ScanRows, st.Pairs,
		st.ChainOrderWhy)
}

// What the planner refuses, and that it says why. Each of these is a shape where re-centring
// would be unsound or pointless, and a silent decline is indistinguishable from a planner
// that is not running at all.
func TestChainReorderDeclines(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(t *testing.T) (*Catalog, JoinSpec)
		want  string
	}{
		{
			name: "an archive promising newest-first",
			build: func(t *testing.T) (*Catalog, JoinSpec) {
				return probeChainFixture(t, 8192, 32, false),
					probeChainSpec("big1", "big2", "sel")
			},
			want: "read in a fixed order",
		},
		{
			name: "a step that is not an inner join",
			build: func(t *testing.T) (*Catalog, JoinSpec) {
				spec := orderSpec()
				spec.Steps[1].Kind = JoinLeft
				return orderFixture(t, 4000, 40), spec
			},
			want: "not an inner join",
		},
		{
			name: "two join keys rather than one",
			build: func(t *testing.T) (*Catalog, JoinSpec) {
				spec := orderSpec()
				// The second step now hangs off hist, not jobs: a path, not a star.
				spec.Steps[1].Cond = []string{"hist.K == small.K"}
				spec.Steps[1].Equi = []JoinEqui{{Left: "hist.K", Right: "K"}}
				return orderFixture(t, 4000, 40), spec
			},
			want: "does not read exactly one attribute of jobs",
		},
		{
			// Both halves read the FROM side, so the star test above passes -- but on two
			// different attributes, which is two join keys. Promoting a spoke would leave
			// the other hanging off a table that no longer supplies the probe keys.
			name: "two attributes of the FROM side",
			build: func(t *testing.T) (*Catalog, JoinSpec) {
				spec := orderSpec()
				spec.Steps[1].Cond = []string{"jobs.Prio == small.MinPrio"}
				spec.Steps[1].Equi = []JoinEqui{{Left: "jobs.Prio", Right: "MinPrio"}}
				return orderFixture(t, 4000, 40), spec
			},
			want: "two join keys rather than one",
		},
		{
			name: "tables small enough to read in any order",
			build: func(t *testing.T) (*Catalog, JoinSpec) {
				return orderFixture(t, 100, 10), orderSpec()
			},
			want: "small enough to read in any order",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat, spec := tc.build(t)
			var st JoinStats
			out := cat.planJoinOrder(spec, false, &st)
			if !strings.Contains(st.ChainOrderWhy, tc.want) {
				t.Errorf("declined with %q, want it to mention %q", st.ChainOrderWhy, tc.want)
			}
			if st.ChainFromAlias != "" {
				t.Errorf("declined but named %q as the FROM side", st.ChainFromAlias)
			}
			if out.Left.Alias != spec.Left.Alias {
				t.Errorf("declined but moved the FROM side to %q", out.Left.Alias)
			}
			// A declined chain must come back byte-for-byte, or the decline is itself a
			// rewrite and the shapes above are being re-planned after all.
			if len(out.Steps) != len(spec.Steps) {
				t.Fatalf("declined but returned %d steps, not %d", len(out.Steps), len(spec.Steps))
			}
			for i := range out.Steps {
				if out.Steps[i].Side.Alias != spec.Steps[i].Side.Alias {
					t.Errorf("declined but step %d folds in %q, not %q", i,
						out.Steps[i].Side.Alias, spec.Steps[i].Side.Alias)
				}
			}
		})
	}
}

// An ON term the alias compiler cannot read whole is refused rather than guessed at: its
// references are not fully described, so there is no step it can be shown to be safe at.
func TestChainReorderRefusesAnOnTermItCannotPlace(t *testing.T) {
	cat := orderFixture(t, 4000, 40)
	spec := orderSpec()
	spec.Steps[0].Cond = append(spec.Steps[0].Cond, "size({hist, jobs}) > 0")
	var st JoinStats
	cat.planJoinOrder(spec, false, &st)
	if !strings.Contains(st.ChainOrderWhy, "no step it can safely be moved to") {
		t.Errorf("declined with %q, want it to refuse the term it cannot place", st.ChainOrderWhy)
	}
}

// A two-table join keeps going to the build-side planner, which decides it on a cost model
// measured on that shape.
func TestPlanJoinOrderLeavesTwoTablesToTheBuildSidePlanner(t *testing.T) {
	cat := orderFixture(t, 4000, 40)
	spec := JoinSpec{
		Left:  JoinSide{Table: "jobs", Alias: "jobs"},
		Right: JoinSide{Table: "small", Alias: "small"},
		Cond:  []string{"jobs.K == small.K"},
		Equi:  []JoinEqui{{Left: "K", Right: "K"}},
	}
	var st JoinStats
	cat.planJoinOrder(spec, false, &st)
	if st.BuildSideWhy == "" {
		t.Error("a two-table join reported no build-side decision")
	}
	if st.ChainOrderWhy != "" {
		t.Errorf("a two-table join reported a chain decision: %q", st.ChainOrderWhy)
	}
}

// A grouped aggregate's answer is one row per group, so the order the join's rows arrive in
// is not in it. A chain under one is therefore re-planned even over archives that do promise
// newest-first -- the same rule the build-side planner applies to two tables, and the reason
// the promise is about what a READER sees rather than about the scan.
func TestChainReordersUnderAnAggregateOverOrderedArchives(t *testing.T) {
	cat := probeChainFixture(t, 8192, 32, false) // archives keep newest-first
	spec := probeChainSpec("big1", "big2", "sel")
	agg := func(stats *JoinStats) []AggRow {
		rows, err := cat.JoinAggregate(JoinAggSpec{
			Join:      spec,
			GroupCols: []GroupCol{{Attr: "sel.Owner"}},
			Aggs:      []AggSpec{{Func: AggCount, Arg: "*"}},
		}, JoinLimits{}, stats, nil)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}

	var st JoinStats
	rows := agg(&st)
	if st.ChainFromAlias != "sel" {
		t.Fatalf("an aggregate over ordered archives read FROM %q, want sel: %s",
			st.ChainFromAlias, st.ChainOrderWhy)
	}
	if len(rows) == 0 {
		t.Fatal("the aggregate returned no groups, so nothing was joined")
	}

	// And it is the same answer the chain as written gives.
	var plain []AggRow
	withoutChainReorder(func() { plain = agg(&JoinStats{}) })
	got, want := aggRowKeys(rows), aggRowKeys(plain)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("re-planned groups %v, as written %v", got, want)
	}
}
