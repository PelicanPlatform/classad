package db

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// aggOf runs a join aggregate and renders it as sorted "group|group=value,value" lines, so a
// comparison does not depend on the first-seen order GROUP BY leaves undefined.
func aggOf(t *testing.T, cat *Catalog, spec JoinAggSpec) ([]string, *JoinStats) {
	t.Helper()
	var st JoinStats
	rows, err := cat.JoinAggregate(spec, JoinLimits{}, &st, nil)
	if err != nil {
		t.Fatalf("JoinAggregate: %v", err)
	}
	return renderAgg(rows), &st
}

func renderAgg(rows []AggRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, strings.Join(r.Group, "|")+"="+strings.Join(r.Values, ","))
	}
	sort.Strings(out)
	return out
}

func ownerCount(kind JoinKind) JoinAggSpec {
	return JoinAggSpec{
		Join:      equiSpec(kind),
		GroupCols: []GroupCol{{Attr: "j.Owner"}},
		Aggs:      []AggSpec{{Func: AggCount, Arg: "*"}},
	}
}

// The shape the whole thing exists for: one row per owner, whatever the join produced.
func TestJoinAggregateGroupsByADottedPath(t *testing.T) {
	cat := joinFixture(t)
	got, st := aggOf(t, cat, ownerCount(JoinInner))
	want := []string{"alice=1", "bob=2"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("rows = %v, want %v", got, want)
	}
	// The reduction ratio is the point: 3 joined rows in, 2 groups out.
	if st.Rows != 3 || st.Groups != 2 {
		t.Errorf("stats say %d joined rows and %d groups, want 3 and 2", st.Rows, st.Groups)
	}
}

// The oracle that cannot drift from the implementation by accident: the same join, drained as
// rows and grouped by hand in the test, must give the same answer.
func TestJoinAggregateAgreesWithGroupingTheRowsByHand(t *testing.T) {
	cat := joinFixture(t)
	for _, kind := range []JoinKind{JoinInner, JoinLeft, JoinCross} {
		name := map[JoinKind]string{JoinInner: "inner", JoinLeft: "left", JoinCross: "cross"}[kind]
		t.Run(name, func(t *testing.T) {
			spec := ownerCount(kind)
			if kind == JoinCross {
				spec.Join = JoinSpec{Kind: JoinCross,
					Left:  JoinSide{Table: "jobs", Alias: "j"},
					Right: JoinSide{Table: "hist", Alias: "h"}}
			}
			// By hand, from the rows.
			var st JoinStats
			seq, err := cat.joinWith(spec.Join, DefaultJoinAggLimits(), &st)
			if err != nil {
				t.Fatal(err)
			}
			byHand := map[string]int{}
			for row := range seq {
				byHand[evalText(row, "j.Owner")]++
			}
			if st.Err != nil {
				t.Fatal(st.Err)
			}
			var want []string
			for k, v := range byHand {
				want = append(want, fmt.Sprintf("%s=%d", k, v))
			}
			sort.Strings(want)

			got, _ := aggOf(t, cat, spec)
			if strings.Join(got, " ") != strings.Join(want, " ") {
				t.Errorf("reduced = %v, by hand = %v", got, want)
			}
		})
	}
}

// A join aggregate and the same aggregate over one table must group identically, because they
// are the same reduce engine reached two ways. Pinned against a 1:1 join, where the joined
// rows correspond one for one with the left table's.
func TestJoinAggregateGroupsAsTheSingleTableAggregateDoes(t *testing.T) {
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
	// Owners that differ only in case, and a numeric column stored as both an int and a real,
	// which are the two places ClassAd's == and a group key could disagree.
	rows := []struct {
		key, owner string
		cpus       string
	}{
		{"1", "alice", "2"}, {"2", "Alice", "2.0"}, {"3", "bob", "4"}, {"4", "bob", "4.0"},
	}
	for _, r := range rows {
		ad, perr := classad.Parse(fmt.Sprintf(`[ ClusterId = %s; Owner = %q; Cpus = %s ]`, r.key, r.owner, r.cpus))
		if perr != nil {
			t.Fatal(perr)
		}
		if werr := jobs.Put(r.key, ad); werr != nil {
			t.Fatal(werr)
		}
		h, _ := classad.Parse(fmt.Sprintf(`[ ClusterId = %s; ExitCode = 0 ]`, r.key))
		if werr := hist.Put(r.key, h); werr != nil {
			t.Fatal(werr)
		}
	}
	for _, c := range []struct{ joinPath, tablePath string }{
		{"j.Owner", "Owner"}, {"j.Cpus", "Cpus"},
	} {
		t.Run(c.tablePath, func(t *testing.T) {
			joined, _ := aggOf(t, cat, JoinAggSpec{
				Join:      equiSpec(JoinInner),
				GroupCols: []GroupCol{{Attr: c.joinPath}},
				Aggs:      []AggSpec{{Func: AggCount, Arg: "*"}},
			})
			single, serr := jobs.AggregateColsStats("true", []GroupCol{{Attr: c.tablePath}},
				[]AggSpec{{Func: AggCount, Arg: "*"}}, nil, nil)
			if serr != nil {
				t.Fatal(serr)
			}
			want := renderAgg(single)
			if strings.Join(joined, " ") != strings.Join(want, " ") {
				t.Errorf("grouping by %s over a join = %v, over the table = %v",
					c.tablePath, joined, want)
			}
			// Pinned so the agreement above cannot be two paths agreeing on nothing.
			// Owner: "alice" and "Alice" are SEPARATE groups, although ClassAd's ==
			// reports them equal -- a group key renders the value and two spellings are
			// two values. That is the single-table engine's rule and the one thing a
			// join aggregate must not quietly decide differently, since the spelling it
			// dropped is the one it would have to put in the output. Cpus: 4 and 4.0
			// are ONE group, which is where a rendering could have disagreed with ==
			// and does not.
			pinned := map[string][]string{
				"Owner": {"Alice=1", "alice=1", "bob=2"},
				"Cpus":  {"2=2", "4=2"},
			}[c.tablePath]
			if strings.Join(joined, " ") != strings.Join(pinned, " ") {
				t.Errorf("grouping by %s = %v, want %v", c.tablePath, joined, pinned)
			}
		})
	}
}

// A LEFT join's null-extended row has no member for the missing side, so every path through it
// is undefined -- which the aggregates must treat exactly as the single-table path treats a
// missing attribute: COUNT(*) counts the row, COUNT(path) does not, and SUM skips it.
func TestJoinAggregateHandlesAnUndefinedArgument(t *testing.T) {
	cat := joinFixture(t)
	spec := JoinAggSpec{
		Join:      equiSpec(JoinLeft),
		GroupCols: []GroupCol{{Attr: "j.Owner"}},
		Aggs: []AggSpec{
			{Func: AggCount, Arg: "*"},
			{Func: AggCount, Arg: "h.ExitCode"},
			{Func: AggSum, Arg: "h.ExitCode"},
			{Func: AggMax, Arg: "h.ExitCode"},
		},
	}
	got, _ := aggOf(t, cat, spec)
	// alice has two left rows: cluster 1 (exit 0) and cluster 3, which paired with nothing.
	want := []string{"alice=2,1,0,0", "bob=2,2,8,7"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

// A per-aggregate FILTER is an expression over the COMPOSITE row, which is the one thing the
// single-table filter plumbing cannot bind: it analyses `h.ExitCode == 0` as reading the
// attribute `h`, the nested ad, and a nested ad binds as undefined -- so a filter wired that
// way would exclude every row and report zero.
func TestJoinAggregateFilterSeesTheJoinedRow(t *testing.T) {
	cat := joinFixture(t)
	spec := JoinAggSpec{
		Join:      equiSpec(JoinInner),
		GroupCols: []GroupCol{{Attr: "j.Owner"}},
		Aggs: []AggSpec{
			{Func: AggCount, Arg: "*"},
			{Func: AggCount, Arg: "*", Filter: "h.ExitCode == 0"},
			{Func: AggCount, Arg: "*", Filter: `j.Owner == "bob"`},
		},
	}
	got, _ := aggOf(t, cat, spec)
	// A filter narrows its own aggregate, never the group: bob keeps its row with a 0.
	want := []string{"alice=1,1,0", "bob=2,0,2"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

// Two filtered aggregates in one request must not share a synthetic column, and a filter must
// not leak into an unfiltered aggregate beside it.
func TestJoinAggregateFiltersAreIndependent(t *testing.T) {
	cat := joinFixture(t)
	spec := JoinAggSpec{
		Join: equiSpec(JoinInner),
		Aggs: []AggSpec{
			{Func: AggCount, Arg: "*", Filter: "h.ExitCode == 0"},
			{Func: AggCount, Arg: "*", Filter: "h.ExitCode == 7"},
			{Func: AggCount, Arg: "*"},
			{Func: AggSum, Arg: "h.ExitCode", Filter: "h.ExitCode > 0"},
		},
	}
	got, _ := aggOf(t, cat, spec)
	want := []string{"=1,1,3,8"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

// A filter whose path reaches a side that is not there evaluates to undefined, which excludes
// the row rather than keeping it -- the same rule the rest of ClassAd follows.
func TestJoinAggregateFilterOverAMissingSideExcludesTheRow(t *testing.T) {
	cat := joinFixture(t)
	spec := JoinAggSpec{
		Join:      equiSpec(JoinLeft),
		GroupCols: []GroupCol{{Attr: "j.Owner"}},
		Aggs: []AggSpec{
			{Func: AggCount, Arg: "*"},
			{Func: AggCount, Arg: "*", Filter: "h.ExitCode >= 0"},
		},
	}
	got, _ := aggOf(t, cat, spec)
	// alice's unmatched row has no h at all, so the filter is undefined for it and it is out.
	want := []string{"alice=2,1", "bob=2,2"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

// With no group columns the whole join reduces to one row, and it is still one row when the
// join produced nothing.
func TestJoinAggregateWithNoGroupColumns(t *testing.T) {
	cat := joinFixture(t)
	aggs := []AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggCountDistinct, Arg: "h.ExitCode"}}
	got, _ := aggOf(t, cat, JoinAggSpec{Join: equiSpec(JoinInner), Aggs: aggs})
	if strings.Join(got, " ") != "=3,3" {
		t.Errorf("rows = %v, want [=3,3]", got)
	}

	empty := equiSpec(JoinInner)
	empty.Post = []string{"h.ExitCode == 999"} // nothing survives
	got, _ = aggOf(t, cat, JoinAggSpec{Join: empty, Aggs: aggs})
	if strings.Join(got, " ") != "=0,0" {
		t.Errorf("over an empty join rows = %v, want [=0,0]", got)
	}
}

// A bucketed group column floors a numeric path, so a joined row joins a time series.
func TestJoinAggregateBucketsANumericPath(t *testing.T) {
	cat := joinFixture(t)
	got, _ := aggOf(t, cat, JoinAggSpec{
		Join:      equiSpec(JoinInner),
		GroupCols: []GroupCol{{Attr: "h.ExitCode", BucketWidth: 4}},
		Aggs:      []AggSpec{{Func: AggCount, Arg: "*"}},
	})
	// Exit codes 0 and 1 floor to 0; 7 floors to 4.
	want := []string{"0=2", "4=1"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

// A truncated reduction is a WRONG answer, not a partial one: a COUNT that stopped early is
// indistinguishable from a smaller COUNT, so it must not come back as rows at all.
func TestJoinAggregateRefusesATruncatedJoin(t *testing.T) {
	cat := joinFixture(t)
	spec := JoinAggSpec{
		Join: JoinSpec{Kind: JoinCross,
			Left:  JoinSide{Table: "jobs", Alias: "j"},
			Right: JoinSide{Table: "hist", Alias: "h"}},
		GroupCols: []GroupCol{{Attr: "j.Owner"}},
		Aggs:      []AggSpec{{Func: AggCount, Arg: "*"}},
	}
	rows, err := cat.JoinAggregate(spec, JoinLimits{MaxPairs: 5}, nil, nil)
	if err == nil {
		t.Fatalf("a 5-pair budget over 12 pairs returned %v and no error", renderAgg(rows))
	}
	if rows != nil {
		t.Errorf("a truncated aggregate returned %v; it must return no rows", renderAgg(rows))
	}
	if !strings.Contains(err.Error(), "candidate pairs") {
		t.Errorf("error = %v, want one naming the pair budget", err)
	}
}

// Likewise for a cancelled scan: the groups that happened to accumulate first are not an
// answer, and the reduce engine returns them with no error of its own.
func TestJoinAggregateRefusesACancelledScan(t *testing.T) {
	cat := joinFixture(t)
	rows, err := cat.JoinAggregate(ownerCount(JoinInner), JoinLimits{}, nil, func() bool { return true })
	if err == nil {
		t.Fatalf("a cancelled aggregate returned %v and no error", renderAgg(rows))
	}
	if rows != nil {
		t.Errorf("a cancelled aggregate returned %v; it must return no rows", renderAgg(rows))
	}
}

// The budget that bounds the wire is not the budget that bounds a reduction. MaxRows caps
// joined rows PRODUCED, which is what a streaming join sends; here nothing is sent, so it is
// raised to meet the budget that does price the work.
func TestJoinAggregateLimitsRaiseTheRowBudgetOnly(t *testing.T) {
	base, agg := DefaultJoinLimits(), DefaultJoinAggLimits()
	if agg.MaxRows <= base.MaxRows {
		t.Errorf("the aggregate's row budget is %d, no larger than the streaming join's %d",
			agg.MaxRows, base.MaxRows)
	}
	if agg.MaxRows != base.MaxPairs {
		t.Errorf("the aggregate's row budget is %d, want the pair budget %d", agg.MaxRows, base.MaxPairs)
	}
	if agg.MaxHashRows != base.MaxHashRows || agg.MaxPairs != base.MaxPairs {
		t.Errorf("the aggregate changed a budget that bounds real work: %+v vs %+v", agg, base)
	}

	// And a caller still only asks for less: a row budget below the join's output refuses.
	cat := joinFixture(t)
	if _, err := cat.JoinAggregate(ownerCount(JoinInner), JoinLimits{MaxRows: 1}, nil, nil); err == nil {
		t.Errorf("a 1-row budget over a 3-row join reported no error")
	}
}

// The raise, stated as behaviour rather than as arithmetic over two constructors: a join
// whose output is past the streaming budget aggregates, and the identical join streamed does
// not. Without it the aggregate would refuse exactly the queries it exists for -- a history
// join big enough to be worth reducing in the store is routinely past a million rows.
func TestJoinAggregateRunsPastTheStreamingRowBudget(t *testing.T) {
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
	// 1001 x 1000 = 1,001,000 pairs, just past DefaultJoinLimits().MaxRows.
	const left, right, owners = 1001, 1000, 4
	for i := 0; i < left; i++ {
		ad := classad.New()
		ad.InsertAttrString("Owner", fmt.Sprintf("u%d", i%owners))
		if werr := jobs.Put(fmt.Sprintf("%d", i), ad); werr != nil {
			t.Fatal(werr)
		}
	}
	for i := 0; i < right; i++ {
		ad := classad.New()
		ad.InsertAttr("ExitCode", int64(i%3))
		if werr := hist.Put(fmt.Sprintf("%d", i), ad); werr != nil {
			t.Fatal(werr)
		}
	}
	cross := JoinSpec{Kind: JoinCross,
		Left:  JoinSide{Table: "jobs", Alias: "j"},
		Right: JoinSide{Table: "hist", Alias: "h"}}

	var st JoinStats
	rows, err := cat.JoinAggregate(JoinAggSpec{Join: cross,
		GroupCols: []GroupCol{{Attr: "j.Owner"}},
		Aggs:      []AggSpec{{Func: AggCount, Arg: "*"}}}, JoinLimits{}, &st, nil)
	if err != nil {
		t.Fatalf("aggregating %d joined rows: %v", left*right, err)
	}
	if st.Rows <= DefaultJoinLimits().MaxRows {
		t.Fatalf("the join produced %d rows, not past the streaming budget of %d -- this "+
			"test proves nothing at that size", st.Rows, DefaultJoinLimits().MaxRows)
	}
	if len(rows) != owners || st.Groups != owners {
		t.Errorf("reduced %d rows to %v (%d groups), want %d groups", st.Rows, renderAgg(rows), st.Groups, owners)
	}

	// The same join streamed stops at the budget the aggregate was raised past.
	var sst JoinStats
	seq, err := cat.Join(cross, JoinLimits{}, &sst)
	if err != nil {
		t.Fatal(err)
	}
	for range seq { //nolint:revive // draining is the point
	}
	if sst.Err == nil {
		t.Errorf("streaming the same %d-row join met no limit; the aggregate's raise is "+
			"then indistinguishable from no raise at all", st.Rows)
	}
}

// A request that returns as many groups as it read rows is a row stream with a GROUP BY
// written on it, and answering it would hand back the thing this exists not to send.
func TestJoinAggregateRefusesTooManyGroups(t *testing.T) {
	cat := joinFixture(t)
	old := maxJoinGroups
	maxJoinGroups = 1
	defer func() { maxJoinGroups = old }()

	rows, err := cat.JoinAggregate(ownerCount(JoinInner), JoinLimits{}, nil, nil)
	if err == nil {
		t.Fatalf("2 groups under a cap of 1 returned %v and no error", renderAgg(rows))
	}
	if !strings.Contains(err.Error(), "groups") {
		t.Errorf("error = %v, want one naming the group cap", err)
	}
	// One group is under the cap and still answers.
	one := ownerCount(JoinInner)
	one.GroupCols = []GroupCol{{Attr: `"all"`}}
	if _, err := cat.JoinAggregate(one, JoinLimits{}, nil, nil); err != nil {
		t.Errorf("1 group under a cap of 1: %v", err)
	}
}

// A path that does not parse is a setup problem, reported before any row is read rather than
// as a column of undefineds.
func TestJoinAggregateRejectsAPathThatDoesNotParse(t *testing.T) {
	cat := joinFixture(t)
	cases := map[string]JoinAggSpec{
		"group": {Join: equiSpec(JoinInner), GroupCols: []GroupCol{{Attr: "j.("}},
			Aggs: []AggSpec{{Func: AggCount, Arg: "*"}}},
		"arg": {Join: equiSpec(JoinInner), Aggs: []AggSpec{{Func: AggSum, Arg: "h.("}}},
		"filter": {Join: equiSpec(JoinInner),
			Aggs: []AggSpec{{Func: AggCount, Arg: "*", Filter: "h.("}}},
		"countdistinct-star": {Join: equiSpec(JoinInner),
			Aggs: []AggSpec{{Func: AggCountDistinct, Arg: "*"}}},
		"table": {Join: JoinSpec{Kind: JoinInner,
			Left:  JoinSide{Table: "nope", Alias: "j"},
			Right: JoinSide{Table: "hist", Alias: "h"}},
			Aggs: []AggSpec{{Func: AggCount, Arg: "*"}}},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			if rows, err := cat.JoinAggregate(spec, JoinLimits{}, nil, nil); err == nil {
				t.Errorf("returned %v and no error", renderAgg(rows))
			}
		})
	}
}

// The same path written twice, or in two cases, is one column rather than two evaluations --
// and still lands in the right place for each consumer.
func TestJoinAggregateInternsAPathOnce(t *testing.T) {
	cat := joinFixture(t)
	spec := JoinAggSpec{
		Join:      equiSpec(JoinInner),
		GroupCols: []GroupCol{{Attr: "j.Owner"}},
		Aggs: []AggSpec{
			{Func: AggCount, Arg: "j.owner"},
			{Func: AggMax, Arg: "h.ExitCode"},
			{Func: AggMin, Arg: "h.ExitCode"},
		},
	}
	p, err := compileJoinAgg(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.attrs) != 2 {
		t.Errorf("projected %v, want j.Owner and h.ExitCode once each", p.attrs)
	}
	got, _ := aggOf(t, cat, spec)
	want := []string{"alice=1,0,0", "bob=2,7,1"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

// The synthetic column a composite-row filter rides in must not be one a caller can land on.
func TestJoinAggregateFilterColumnDoesNotCollideWithAPath(t *testing.T) {
	cat := joinFixture(t)
	spec := JoinAggSpec{
		Join:      equiSpec(JoinInner),
		GroupCols: []GroupCol{{Attr: "__joinfilter1_0"}}, // the name aggregate 1 would want
		Aggs: []AggSpec{
			{Func: AggCount, Arg: "*"},
			{Func: AggCount, Arg: "*", Filter: "h.ExitCode == 0"},
		},
	}
	p, err := compileJoinAgg(spec)
	if err != nil {
		t.Fatal(err)
	}
	if p.aggs[1].Filter == "__joinfilter1_0" {
		t.Fatalf("the filter took the column a group path already holds")
	}
	// The group path is an undefined attribute of the composite row, so there is one group;
	// the filter still counts the one row whose exit code is 0.
	got, _ := aggOf(t, cat, spec)
	if strings.Join(got, " ") != "undefined=3,1" {
		t.Errorf("rows = %v, want [undefined=3,1]", got)
	}
}
