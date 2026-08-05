package db

import (
	"fmt"
	"testing"
)

// aggFilterArchive seeds a history table with a status mix, so one pass can answer several
// differently-conditioned questions -- the case per-aggregate filters exist for.
//
//	alice: 2 completed (status 4), 1 running (2), 1 held (5)
//	bob:   1 completed,             2 running
func aggFilterArchive(t *testing.T) *ArchiveTable {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{})
	if err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		owner  string
		status int
		cpus   int
	}{
		{"alice", 4, 1}, {"alice", 4, 2}, {"alice", 2, 4}, {"alice", 5, 8},
		{"bob", 4, 1}, {"bob", 2, 2}, {"bob", 2, 16},
	}
	for i, r := range rows {
		if err := a.AppendOld(fmt.Sprintf(
			"Owner = \"%s\"\nJobStatus = %d\nCpus = %d\nID = %d\n", r.owner, r.status, r.cpus, i)); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

// TestAggregateFilterPivot is the shape the feature is for: total plus per-status counts in
// ONE aggregation, where each filtered count sees only its own rows.
func TestAggregateFilterPivot(t *testing.T) {
	a := aggFilterArchive(t)
	rows, err := a.Aggregate("true", []string{"Owner"}, []AggSpec{
		{Func: AggCount, Arg: "*"},
		{Func: AggCount, Arg: "*", Filter: "JobStatus == 4"},
		{Func: AggCount, Arg: "*", Filter: "JobStatus == 2"},
		{Func: AggSum, Arg: "Cpus", Filter: "JobStatus == 2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, r := range rows {
		got[r.Group[0]] = r.Values
	}
	// alice: 4 rows, 2 completed, 1 running (4 cpus); bob: 3 rows, 1 completed, 2 running (18).
	want := map[string][]string{
		"alice": {"4", "2", "1", "4"},
		"bob":   {"3", "1", "2", "18"},
	}
	for owner, w := range want {
		g := got[owner]
		if len(g) != len(w) {
			t.Errorf("%s: %v, want %v", owner, g, w)
			continue
		}
		for i := range w {
			if g[i] != w[i] {
				t.Errorf("%s: %v, want %v", owner, g, w)
				break
			}
		}
	}
}

// TestAggregateFilterNarrowsAggregateNotGroup pins the distinction from WHERE: a group whose
// rows all fail the filter still appears, with COUNT 0 -- it is not removed from the result.
func TestAggregateFilterNarrowsAggregateNotGroup(t *testing.T) {
	a := aggFilterArchive(t)
	rows, err := a.Aggregate("true", []string{"Owner"}, []AggSpec{
		{Func: AggCount, Arg: "*", Filter: "JobStatus == 5"}, // only alice has one
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Group[0]] = r.Values[0]
	}
	if len(got) != 2 {
		t.Fatalf("groups = %v, want both owners to survive", got)
	}
	if got["alice"] != "1" || got["bob"] != "0" {
		t.Errorf("counts = %v, want alice 1 and bob 0", got)
	}
}

// TestAggregateFilterWithoutGrouping covers the single implicit group.
func TestAggregateFilterWithoutGrouping(t *testing.T) {
	a := aggFilterArchive(t)
	rows, err := a.Aggregate("true", nil, []AggSpec{
		{Func: AggCount, Arg: "*"},
		{Func: AggCount, Arg: "*", Filter: "JobStatus == 2"},
		{Func: AggCount, Arg: "*", Filter: "JobStatus == 99"}, // matches nothing
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want one", rows)
	}
	if v := rows[0].Values; v[0] != "7" || v[1] != "3" || v[2] != "0" {
		t.Errorf("values = %v, want [7 3 0]", v)
	}
}

// TestAggregateFilterCombinesWithConstraint checks that the query constraint and the
// per-aggregate filter compose: the constraint picks the rows, the filter narrows one
// aggregate within them.
func TestAggregateFilterCombinesWithConstraint(t *testing.T) {
	a := aggFilterArchive(t)
	rows, err := a.Aggregate(`Owner == "alice"`, nil, []AggSpec{
		{Func: AggCount, Arg: "*"},
		{Func: AggCount, Arg: "*", Filter: "JobStatus == 4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v := rows[0].Values; v[0] != "4" || v[1] != "2" {
		t.Errorf("values = %v, want [4 2] (alice's rows, then her completed ones)", v)
	}
}

// TestAggregateFilterExpressions checks that a filter is a full ClassAd expression, not a
// restricted comparison grammar.
func TestAggregateFilterExpressions(t *testing.T) {
	a := aggFilterArchive(t)
	rows, err := a.Aggregate("true", nil, []AggSpec{
		{Func: AggCount, Arg: "*", Filter: "JobStatus == 4 || JobStatus == 5"},
		{Func: AggCount, Arg: "*", Filter: "Cpus >= 4 && JobStatus != 5"},
		{Func: AggCount, Arg: "*", Filter: `Owner == "bob"`},
		{Func: AggMax, Arg: "Cpus", Filter: `Owner == "bob"`},
		{Func: AggCount, Arg: "*", Filter: "Missing is undefined"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// completed+held: 3+1 = 4; cpus>=4 and not held: alice's 4, bob's 16 = 2; bob: 3;
	// bob's max cpus: 16; a filter on an absent attribute holds for every row: 7.
	if v := rows[0].Values; v[0] != "4" || v[1] != "2" || v[2] != "3" || v[3] != "16" || v[4] != "7" {
		t.Errorf("values = %v, want [4 2 3 16 7]", v)
	}
}

// TestAggregateFilterBadExpression checks that an unparseable filter is reported rather than
// silently ignored -- ignoring it would return an unfiltered count that looks plausible.
func TestAggregateFilterBadExpression(t *testing.T) {
	a := aggFilterArchive(t)
	_, err := a.Aggregate("true", nil, []AggSpec{
		{Func: AggCount, Arg: "*", Filter: "JobStatus =="},
	})
	if err == nil {
		t.Fatal("expected an error for a malformed filter")
	}
}

// TestAggregateUnfilteredUnchanged is the regression guard: specs with no filter must behave
// exactly as before.
func TestAggregateUnfilteredUnchanged(t *testing.T) {
	a := aggFilterArchive(t)
	rows, err := a.Aggregate("true", []string{"Owner"}, []AggSpec{
		{Func: AggCount, Arg: "*"},
		{Func: AggSum, Arg: "Cpus"},
		{Func: AggMax, Arg: "Cpus"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, r := range rows {
		got[r.Group[0]] = r.Values
	}
	if g := got["alice"]; g[0] != "4" || g[1] != "15" || g[2] != "8" {
		t.Errorf("alice = %v, want [4 15 8]", g)
	}
	if g := got["bob"]; g[0] != "3" || g[1] != "19" || g[2] != "16" {
		t.Errorf("bob = %v, want [3 19 16]", g)
	}
}
