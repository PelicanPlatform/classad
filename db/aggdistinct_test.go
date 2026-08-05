package db

import (
	"fmt"
	"testing"
)

// distinctArchive seeds an archive where each owner repeats values, so a distinct count and
// a plain count differ.
//
//	alice: hosts a,a,b      statuses 4,4,2
//	bob:   hosts c,c        statuses 2,2
//	carol: host  a          status   4      (shares a host with alice)
func distinctArchive(t *testing.T) *ArchiveTable {
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
		owner, host string
		status      int
	}{
		{"alice", "a", 4}, {"alice", "a", 4}, {"alice", "b", 2},
		{"bob", "c", 2}, {"bob", "c", 2},
		{"carol", "a", 4},
	}
	for i, r := range rows {
		if err := a.AppendOld(fmt.Sprintf(
			"Owner = \"%s\"\nHost = \"%s\"\nJobStatus = %d\nID = %d\n",
			r.owner, r.host, r.status, i)); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

// TestCountDistinct is the core: distinct counts differ from row counts, per group and over
// the whole table.
func TestCountDistinct(t *testing.T) {
	a := distinctArchive(t)

	rows, err := a.Aggregate("true", []string{"Owner"}, []AggSpec{
		{Func: AggCount, Arg: "*"},
		{Func: AggCountDistinct, Arg: "Host"},
		{Func: AggCountDistinct, Arg: "JobStatus"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, r := range rows {
		got[r.Group[0]] = r.Values
	}
	want := map[string][]string{
		"alice": {"3", "2", "2"}, // 3 rows, hosts {a,b}, statuses {4,2}
		"bob":   {"2", "1", "1"},
		"carol": {"1", "1", "1"},
	}
	for owner, w := range want {
		g := got[owner]
		if len(g) != len(w) {
			t.Errorf("%s = %v, want %v", owner, g, w)
			continue
		}
		for i := range w {
			if g[i] != w[i] {
				t.Errorf("%s = %v, want %v", owner, g, w)
				break
			}
		}
	}

	// Ungrouped: distinct is over the whole table, so a host shared between owners counts
	// once -- not the sum of the per-owner distincts.
	rows, err = a.Aggregate("true", nil, []AggSpec{
		{Func: AggCount, Arg: "*"},
		{Func: AggCountDistinct, Arg: "Host"},
		{Func: AggCountDistinct, Arg: "Owner"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v := rows[0].Values; v[0] != "6" || v[1] != "3" || v[2] != "3" {
		t.Errorf("ungrouped = %v, want [6 3 3] (hosts a,b,c and three owners)", v)
	}
}

// TestCountDistinctIgnoresUndefined checks that a row missing the attribute contributes no
// value, matching COUNT(col), rather than counting "undefined" as a distinct value.
func TestCountDistinctIgnoresUndefined(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a, err := cat.CreateArchiveTable("h", ArchiveConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"Owner = \"alice\"\nHost = \"a\"\n",
		"Owner = \"alice\"\nHost = \"b\"\n",
		"Owner = \"alice\"\n", // no Host at all
	} {
		if err := a.AppendOld(text); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := a.Aggregate("true", nil, []AggSpec{
		{Func: AggCount, Arg: "*"},
		{Func: AggCount, Arg: "Host"},
		{Func: AggCountDistinct, Arg: "Host"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v := rows[0].Values; v[0] != "3" || v[1] != "2" || v[2] != "2" {
		t.Errorf("values = %v, want [3 2 2]: undefined is not a distinct value", v)
	}
}

// TestCountDistinctWithFilter checks that DISTINCT and FILTER compose -- the distinct set is
// built only from the rows the filter admits.
func TestCountDistinctWithFilter(t *testing.T) {
	a := distinctArchive(t)
	rows, err := a.Aggregate("true", nil, []AggSpec{
		{Func: AggCountDistinct, Arg: "Host"},
		{Func: AggCountDistinct, Arg: "Host", Filter: "JobStatus == 4"},
		{Func: AggCountDistinct, Arg: "Host", Filter: "JobStatus == 2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// All hosts {a,b,c}; completed rows are on {a}; running rows on {b,c}.
	if v := rows[0].Values; v[0] != "3" || v[1] != "1" || v[2] != "2" {
		t.Errorf("values = %v, want [3 1 2]", v)
	}
}

// TestCountDistinctStar is refused: there is no such thing as a distinct row.
func TestCountDistinctStar(t *testing.T) {
	a := distinctArchive(t)
	if _, err := a.Aggregate("true", nil, []AggSpec{{Func: AggCountDistinct, Arg: "*"}}); err == nil {
		t.Error("COUNT(DISTINCT *) should be refused")
	}
}

// TestCountDistinctNumericIdentity checks that values are keyed the way the group tuple keys
// them, so a distinct count and a GROUP BY over the same attribute agree on what "the same
// value" means.
func TestCountDistinctNumericIdentity(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a, err := cat.CreateArchiveTable("h", ArchiveConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"N = 1\n", "N = 1\n", "N = 2\n", "N = 2.0\n"} {
		if err := a.AppendOld(text); err != nil {
			t.Fatal(err)
		}
	}
	distinct, err := a.Aggregate("true", nil, []AggSpec{{Func: AggCountDistinct, Arg: "N"}})
	if err != nil {
		t.Fatal(err)
	}
	grouped, err := a.Aggregate("true", []string{"N"}, []AggSpec{{Func: AggCount, Arg: "*"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := distinct[0].Values[0], fmt.Sprint(len(grouped)); got != want {
		t.Errorf("COUNT(DISTINCT N) = %s but GROUP BY N produced %s groups; the two must agree",
			got, want)
	}
}
