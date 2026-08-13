package db

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// A grouped aggregate on a MUTABLE table fell to a record scan. Every columnar fast path in the
// aggregate is gated on there being no grouping, and the grouped path that #190/#192 added was reachable
// only through ArchiveTable -- so `select JobStatus, count(*) from jobs group by JobStatus`, which a
// dashboard asks constantly, decoded every matching record even on a table carrying the accelerator.

// mutableGroupFixture is an in-memory table with the columnar accelerator enabled: small segments so the
// data seals into several, which is what the accelerator covers.
func mutableGroupFixture(t *testing.T, n int) *DB {
	t.Helper()
	d, err := OpenConfig(Config{SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	tx := d.Begin()
	for i := 0; i < n; i++ {
		ad, err := classad.ParseOld(fmt.Sprintf(
			"ClusterId = %d\nProcId = %d\nJobStatus = %d\nRequestMemory = %d\nRequestCpus = %d\n"+
				"Owner = \"user%d\"\nCmd = \"/home/user%d/run.sh\"",
			i, i%10, 1+i%5, 1024+(i%32)*512, 1+i%8, i%64, i%64))
		if err != nil {
			t.Fatal(err)
		}
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if !d.EnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to accelerate")
	}
	return d
}

// scanGroupedMutable is the reference: the same scan-and-reduce the aggregate falls back to.
func scanGroupedMutable(t *testing.T, d *DB, constraint string, groupCols []GroupCol, aggs []AggSpec) []AggRow {
	t.Helper()
	attrs, groupCol, aggCol := AggProjection(groupCols, aggs)
	seq, err := d.QueryProject(constraint, attrs)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := AggregateValues(seq, attrs, groupCols, aggs, groupCol, aggCol, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestMutableGroupedFromColumnsMatchesScan(t *testing.T) {
	d := mutableGroupFixture(t, 5000)

	for _, tc := range []struct {
		constraint string
		group      string
		aggs       []AggSpec
		wantServed bool
		why        string
	}{
		{"RequestMemory > 4096", "JobStatus", []AggSpec{{Func: AggCount, Arg: "*"}}, true, ""},
		{"true", "JobStatus", []AggSpec{{Func: AggCount, Arg: "*"}}, true, "match-all"},
		{"RequestMemory > 4096", "ProcId",
			[]AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggMax, Arg: "RequestCpus"}}, true, ""},
		{"RequestCpus >= 4", "JobStatus",
			[]AggSpec{{Func: AggMin, Arg: "RequestMemory"}, {Func: AggSum, Arg: "RequestMemory"}}, true, ""},
		{`Owner == "user3"`, "JobStatus", []AggSpec{{Func: AggCount, Arg: "*"}}, true,
			"a string predicate is lowered by the vector executor"},
		{"RequestMemory > 4096", "Owner", []AggSpec{{Func: AggCount, Arg: "*"}}, false,
			"a string group column is not a numeric histogram"},
		{"RequestMemory > 4096", "JobStatus",
			[]AggSpec{{Func: AggCountDistinct, Arg: "RequestCpus"}}, false, "COUNT(DISTINCT) needs the values"},
		{"RequestMemory > 4096", "JobStatus",
			[]AggSpec{{Func: AggCount, Arg: "*", Filter: "RequestCpus >= 4"}}, false, "a FILTER is invisible here"},
	} {
		groupCols := []GroupCol{{Attr: tc.group}}
		got, served := GroupedFromColumns(d, tc.constraint, groupCols, tc.aggs)
		if served != tc.wantServed {
			t.Errorf("%s GROUP BY %s %v: served = %v, want %v (%s)",
				tc.constraint, tc.group, tc.aggs, served, tc.wantServed, tc.why)
		}
		if !served {
			continue
		}
		want := scanGroupedMutable(t, d, tc.constraint, groupCols, tc.aggs)
		gotM, wantM := groupedRows(t, got), groupedRows(t, want)
		if len(gotM) != len(wantM) {
			t.Errorf("%s GROUP BY %s: %d groups, scan found %d",
				tc.constraint, tc.group, len(gotM), len(wantM))
		}
		for g, v := range wantM {
			if fmt.Sprint(gotM[g]) != fmt.Sprint(v) {
				t.Errorf("%s GROUP BY %s: group %q = %v, scan = %v", tc.constraint, tc.group, g, gotM[g], v)
			}
		}
	}
}

// groupedRows keys whole rows by their group label, and fails on a duplicated label.
func groupedRows(t *testing.T, rows []AggRow) map[string][]string {
	t.Helper()
	out := make(map[string][]string, len(rows))
	for _, r := range rows {
		if len(r.Group) != 1 {
			t.Fatalf("unexpected row shape %+v", r)
		}
		if _, dup := out[r.Group[0]]; dup {
			t.Errorf("group %q appears twice: one group was split into two rows", r.Group[0])
		}
		out[r.Group[0]] = r.Values
	}
	return out
}

// TestMutableGroupedRespectsUncommittedAndUpdates checks the grouped columnar answer tracks the table's
// CURRENT state: a mutable table is updated in place, unlike an archive, so a path that read stale or
// superseded records would still look right on a freshly loaded fixture.
func TestMutableGroupedRespectsUncommittedAndUpdates(t *testing.T) {
	d := mutableGroupFixture(t, 5000)
	countStar := []AggSpec{{Func: AggCount, Arg: "*"}}
	groupCols := []GroupCol{{Attr: "JobStatus"}}

	before, served := GroupedFromColumns(d, "RequestMemory > 4096", groupCols, countStar)
	if !served {
		t.Fatal("not served columnar")
	}

	// Move every JobStatus 1 record to status 7, so the group set itself changes.
	tx := d.Begin()
	moved := 0
	for i := 0; i < 5000; i++ {
		if 1+i%5 != 1 {
			continue
		}
		ad, err := classad.ParseOld(fmt.Sprintf(
			"ClusterId = %d\nProcId = %d\nJobStatus = 7\nRequestMemory = %d\nRequestCpus = %d\n"+
				"Owner = \"user%d\"\nCmd = \"/x\"", i, i%10, 1024+(i%32)*512, 1+i%8, i%64))
		if err != nil {
			t.Fatal(err)
		}
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
		moved++
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if moved == 0 {
		t.Fatal("fixture moved no records: the update below proves nothing")
	}

	after, served := GroupedFromColumns(d, "RequestMemory > 4096", groupCols, countStar)
	if !served {
		t.Fatal("not served columnar after the update")
	}
	want := scanGroupedMutable(t, d, "RequestMemory > 4096", groupCols, countStar)
	gotM, wantM := groupedRows(t, after), groupedRows(t, want)
	for g, v := range wantM {
		if fmt.Sprint(gotM[g]) != fmt.Sprint(v) {
			t.Errorf("after update, group %q = %v, scan = %v", g, gotM[g], v)
		}
	}
	if fmt.Sprint(groupedRows(t, before)) == fmt.Sprint(gotM) {
		t.Error("the result did not change across an update that moved records between groups, so this " +
			"test cannot tell a live read from a stale one")
	}
	if _, ok := gotM["7"]; !ok {
		t.Error("no group 7 after moving records into it")
	}
}

// BenchmarkMutableGroupedCount is what the change buys on a mutable table.
func BenchmarkMutableGroupedCount(b *testing.B) {
	d, err := OpenConfig(Config{SegmentSize: 1 << 16})
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	const n = 60000
	tx := d.Begin()
	for i := 0; i < n; i++ {
		ad, err := classad.ParseOld(fmt.Sprintf(
			"ClusterId = %d\nProcId = %d\nJobStatus = %d\nRequestMemory = %d\nRequestCpus = %d\n"+
				"Owner = \"user%d\"\nCmd = \"/home/user%d/run.sh\"",
			i, i%10, 1+i%5, 1024+(i%32)*512, 1+i%8, i%64, i%64))
		if err != nil {
			b.Fatal(err)
		}
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	if !d.EnableSchemaScan(4000, 8) {
		b.Skip("no sealed segments")
	}
	groupCols := []GroupCol{{Attr: "JobStatus"}}
	aggs := []AggSpec{{Func: AggCount, Arg: "*"}}
	if _, ok := GroupedFromColumns(d, "RequestMemory > 4096", groupCols, aggs); !ok {
		b.Fatal("not served columnar")
	}
	b.Run("columnar", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, ok := GroupedFromColumns(d, "RequestMemory > 4096", groupCols, aggs); !ok {
				b.Fatal("declined")
			}
		}
	})
	b.Run("scan", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			attrs, groupCol, aggCol := AggProjection(groupCols, aggs)
			seq, err := d.QueryProject("RequestMemory > 4096", attrs)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := AggregateValues(seq, attrs, groupCols, aggs, groupCol, aggCol, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
}
