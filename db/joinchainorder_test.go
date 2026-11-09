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
	out := cat.planJoinOrder(orderSpec(), DefaultJoinLimits(), false, &st)

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
			// Every table is reachable, but through two unrelated attributes: K joins jobs
			// to hist, Prio joins jobs to small, and nothing makes those equal. There is
			// no one attribute the tables could be re-attached by.
			name: "two unrelated join keys",
			build: func(t *testing.T) (*Catalog, JoinSpec) {
				spec := orderSpec()
				spec.Steps[1].Cond = []string{"jobs.Prio == small.MinPrio"}
				spec.Steps[1].Equi = []JoinEqui{{Left: "jobs.Prio", Right: "MinPrio"}}
				return orderFixture(t, 4000, 40), spec
			},
			want: "joins on more than one key",
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
			out := cat.planJoinOrder(spec, DefaultJoinLimits(), false, &st)
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
	cat.planJoinOrder(spec, DefaultJoinLimits(), false, &st)
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
	cat.planJoinOrder(spec, DefaultJoinLimits(), false, &st)
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

// pathChainSpec is the same join as probeChainSpec written as a PATH: each step hangs off
// the table the previous one folded in, rather than all of them off the FROM table. On one
// key the two spell the same relation, and the path is the more natural way to write it.
func pathChainSpec(from, s1, s2 string) JoinSpec {
	return JoinSpec{
		Left: JoinSide{Table: from, Alias: from},
		Steps: []JoinStep{
			{
				Kind: JoinInner,
				Side: JoinSide{Table: s1, Alias: s1},
				Cond: []string{from + ".ClusterId == " + s1 + ".ClusterId"},
				Equi: []JoinEqui{{Left: from + ".ClusterId", Right: "ClusterId"}},
			},
			{
				Kind: JoinInner,
				Side: JoinSide{Table: s2, Alias: s2},
				Cond: []string{s1 + ".ClusterId == " + s2 + ".ClusterId"},
				Equi: []JoinEqui{{Left: s1 + ".ClusterId", Right: "ClusterId"}},
			},
		},
	}
}

// A chain written as a path is re-centred like the star it is.
//
// This is the spelling a caller is most likely to write -- each step joined to the table
// before it -- and reading the equalities as a graph rather than as "every step must name
// the FROM table" is what makes it reachable. The second step's row half names big2, which
// is a STEP, so nothing about the syntax says sel could be read FROM.
func TestChainReorderTakesAPathSpelling(t *testing.T) {
	cat := probeChainFixture(t, 8192, 32, true)
	spec := pathChainSpec("big1", "big2", "sel")

	var want int
	withoutChainReorder(func() { want, _ = drainChain(t, cat, spec) })
	got, st := drainChain(t, cat, spec)
	if got != want {
		t.Fatalf("the re-planned path joined %d rows, the path as written %d", got, want)
	}
	if want == 0 {
		t.Fatal("the fixture joined nothing, so this would pass without a join")
	}
	if st.ChainFromAlias != "sel" {
		t.Fatalf("a path-spelled chain read FROM %q, want sel: %s",
			st.ChainFromAlias, st.ChainOrderWhy)
	}
	for _, p := range st.ProbeSteps {
		if p.Keys == 0 {
			t.Errorf("%s was not narrowed (%s)", p.Alias, p.Why)
		}
	}
	t.Logf("hashed %d, streamed %d, pairs %d", st.HashRows, st.ScanRows, st.Pairs)
}

// A chain where the steps join only to each other leaves the FROM table paired by nothing,
// which is a cross product however the tables are ordered. compileJoin refuses this spec
// outright -- both steps read a side the other folds in -- and the planner must not rewrite
// it on the way there.
func TestChainReorderDeclinesAFromTableJoinedToNothing(t *testing.T) {
	cat := orderFixture(t, 4000, 40)
	spec := orderSpec()
	spec.Steps[0].Cond = []string{"small.K == hist.K"}
	spec.Steps[0].Equi = []JoinEqui{{Left: "small.K", Right: "K"}}
	spec.Steps[1].Cond = []string{"hist.K == small.K"}
	spec.Steps[1].Equi = []JoinEqui{{Left: "hist.K", Right: "K"}}
	var st JoinStats
	out := cat.planJoinOrder(spec, DefaultJoinLimits(), false, &st)
	if !strings.Contains(st.ChainOrderWhy, "joined to no other table") {
		t.Errorf("declined with %q, want it to say jobs is joined to nothing", st.ChainOrderWhy)
	}
	if out.Left.Alias != "jobs" {
		t.Errorf("declined but moved the FROM side to %q", out.Left.Alias)
	}
}

// chainProbeFixture is a star of three INDEXED tables that have given up newest-first: one
// small, two large. An index on the join key is what makes the candidates differ on records
// touched, which is what the chain planner's hashRows tie-break cannot otherwise be told
// apart from.
func chainProbeFixture(tb testing.TB, big, small int) *Catalog {
	tb.Helper()
	cat, err := OpenCatalog(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { cat.Close() })
	cfg := ArchiveConfig{SegmentSize: 4096, Unordered: true, ValueAttrs: []string{"K"}}
	mk := func(name string) *ArchiveTable {
		t, cerr := cat.CreateArchiveTable(name, cfg)
		if cerr != nil {
			tb.Fatal(cerr)
		}
		return t
	}
	app := func(t *ArchiveTable, k int) {
		ad := classad.New()
		ad.InsertAttr("K", int64(k))
		ad.InsertAttrString("Pad", strings.Repeat("x", 64))
		if aerr := t.Append(ad); aerr != nil {
			tb.Fatal(aerr)
		}
	}
	sm, b1, b2 := mk("small"), mk("big1"), mk("big2")
	for i := 0; i < big; i++ {
		app(b1, i)
		app(b2, i)
	}
	stride := big / small
	for k := 0; k < small; k++ {
		app(sm, k*stride)
	}
	return cat
}

// chainProbeSpec is that star written FROM the small table, so the chain as written holds
// both large ones.
func chainProbeSpec() JoinSpec {
	return JoinSpec{
		Left: JoinSide{Table: "small", Alias: "small"},
		Steps: []JoinStep{
			{
				Kind: JoinInner,
				Side: JoinSide{Table: "big1", Alias: "big1"},
				Cond: []string{"small.K == big1.K"},
				Equi: []JoinEqui{{Left: "small.K", Right: "K"}},
			},
			{
				Kind: JoinInner,
				Side: JoinSide{Table: "big2", Alias: "big2"},
				Cond: []string{"small.K == big2.K"},
				Equi: []JoinEqui{{Left: "small.K", Right: "K"}},
			},
		},
	}
}

// The chain planner prices `fits` against the limits the join will RUN under, the same as
// the build-side planner does. A chain holds every table but the one it reads FROM, so which
// table that is decides whether the chain fits at all -- and a plan chosen against the
// store's defaults can be one the caller's own budget then refuses.
//
// Written FROM the small table, both large tables are probed through their index and the
// chain touches the fewest records -- while holding the most. That is the only shape where
// this matters: wherever no index separates the candidates, every order touches the same
// records and the tie-break already hashes the least.
func TestChainOrderFitsAgainstTheLimitsTheJoinRunsUnder(t *testing.T) {
	cat := chainProbeFixture(t, 8192, 32)
	spec := chainProbeSpec()

	// Anti-vacuity: under the store's own limits every order fits, the index decides, and
	// the chain stays as written. Whatever moves below, the limit is what moved it.
	var asIs JoinStats
	cat.planJoinOrder(spec, DefaultJoinLimits(), false, &asIs)
	if asIs.ChainFromAlias != "" {
		t.Fatalf("the chain was re-planned under the default limits already: %s",
			asIs.ChainOrderWhy)
	}

	var st JoinStats
	seq, err := cat.Join(spec, JoinLimits{MaxHashRows: 10000}, &st)
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for range seq {
		rows++
	}
	if st.Err != nil {
		t.Fatalf("the chain did not run under its own limit: %v", st.Err)
	}
	if st.ChainFromAlias == "" || st.ChainFromAlias == "small" {
		t.Fatalf("the chain was read FROM %q, so it still holds both large tables: %s",
			st.ChainFromAlias, st.ChainOrderWhy)
	}
	if !strings.Contains(st.ChainOrderWhy, "more than one join may") {
		t.Errorf("the reason %q does not say the chain would have held too much",
			st.ChainOrderWhy)
	}
	if st.HashRows > 10000 {
		t.Fatalf("the chain held %d rows against a limit of 10000", st.HashRows)
	}
	if rows != 32 {
		t.Fatalf("the re-planned chain produced %d rows, expected one per row of the "+
			"small table", rows)
	}
}
