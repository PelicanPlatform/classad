package db

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/collections"
)

// DB.AggregateColsStats is the mutable-table aggregate ladder, which used to live in the RPC
// handler. Nothing covered it except through RPC, so an embedded caller -- the one that gained
// anything by the move -- was exercised by nothing at all. These tests drive it directly.

// aggKey renders a row as a comparable line, so results can be compared as unordered sets. A
// grouped aggregate makes no ordering promise and the fast paths return groups sorted by value
// while the scan returns them first-seen, so comparing in order would fail on the ordering alone.
func aggKey(r AggRow) string {
	return strings.Join(r.Group, "\x1f") + " => " + strings.Join(r.Values, ",")
}

func aggLines(rows []AggRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, aggKey(r))
	}
	sort.Strings(out)
	return out
}

// TestDBAggregateColsStatsMatchesScan is the no-behaviour-change check for every shape the ladder
// routes differently: each one is answered by AggregateColsStats (taking whichever fast path it
// qualifies for) and by the projected record scan it would otherwise fall through to, and the two
// must agree down to the rendered text. A difference in VALUE TYPE -- an int sum printed as a real,
// say -- is as wrong as a difference in the number and far easier to ship unnoticed, so the
// comparison is on text, not on parsed numbers.
func TestDBAggregateColsStatsMatchesScan(t *testing.T) {
	d := mutableGroupFixture(t, 5000)

	for _, tc := range []struct {
		name       string
		constraint string
		ref        string // the constraint the reference scan uses, when it cannot take tc.constraint
		groupCols  []GroupCol
		aggs       []AggSpec
	}{
		// Ungrouped, no constraint: the Len() fast path on a non-chained table.
		{"count-star-matchall", "true", "", nil, []AggSpec{{Func: AggCount, Arg: "*"}}},
		// An EMPTY constraint is match-all to the ladder but is rejected by the constraint parser,
		// so only the fast path can serve it and the reference has to be told to match everything
		// a different way. That asymmetry predates the move: a fall-through here would error.
		{"count-star-empty-constraint", "", "true", nil, []AggSpec{{Func: AggCount, Arg: "*"}}},
		// Ungrouped with a constraint: the columnar CountConstraint fast path.
		{"count-star-where", "RequestMemory > 4096", "", nil, []AggSpec{{Func: AggCount, Arg: "*"}}},
		{"count-star-where-nomatch", "RequestMemory > 99999999", "", nil, []AggSpec{{Func: AggCount, Arg: "*"}}},
		// Ungrouped numeric aggregates: ColumnarAggregate, constrained and not.
		{"min", "true", "", nil, []AggSpec{{Func: AggMin, Arg: "RequestMemory"}}},
		{"max-where", "RequestCpus >= 4", "", nil, []AggSpec{{Func: AggMax, Arg: "RequestMemory"}}},
		{"sum", "true", "", nil, []AggSpec{{Func: AggSum, Arg: "RequestMemory"}}},
		{"avg-where", "RequestCpus >= 4", "", nil, []AggSpec{{Func: AggAvg, Arg: "RequestMemory"}}},
		{"count-attr", "true", "", nil, []AggSpec{{Func: AggCount, Arg: "RequestCpus"}}},
		{"max-where-nomatch", "RequestMemory > 99999999", "", nil, []AggSpec{{Func: AggMax, Arg: "RequestCpus"}}},
		// Grouped on one column: GroupedFromColumns, numeric and string group values.
		{"group-count", "true", "", []GroupCol{{Attr: "JobStatus"}}, []AggSpec{{Func: AggCount, Arg: "*"}}},
		{"group-count-where", "RequestMemory > 4096", "", []GroupCol{{Attr: "JobStatus"}},
			[]AggSpec{{Func: AggCount, Arg: "*"}}},
		{"group-mixed-aggs", "RequestCpus >= 4", "", []GroupCol{{Attr: "JobStatus"}}, []AggSpec{
			{Func: AggCount, Arg: "*"}, {Func: AggMin, Arg: "RequestMemory"},
			{Func: AggMax, Arg: "RequestMemory"}, {Func: AggSum, Arg: "RequestMemory"}}},
		{"group-by-string", "true", "", []GroupCol{{Attr: "Owner"}}, []AggSpec{{Func: AggCount, Arg: "*"}}},
		// Bucketed: the "per day" shape, which only the grouped columnar path serves.
		{"group-bucketed", "true", "", []GroupCol{{Attr: "RequestMemory", BucketWidth: 4096}},
			[]AggSpec{{Func: AggCount, Arg: "*"}}},
		{"group-bucketed-where", "RequestCpus >= 4", "", []GroupCol{{Attr: "ClusterId", BucketWidth: 1000}},
			[]AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggAvg, Arg: "RequestMemory"}}},
		// Shapes every fast path declines, so the ladder must fall through to the scan. Comparing
		// them against the scan proves the fall-through happened rather than something answering
		// approximately: the two reach the same code and cannot disagree.
		{"two-group-cols", "true", "", []GroupCol{{Attr: "JobStatus"}, {Attr: "RequestCpus"}},
			[]AggSpec{{Func: AggCount, Arg: "*"}}},
		{"agg-over-string", "true", "", []GroupCol{{Attr: "JobStatus"}}, []AggSpec{{Func: AggMax, Arg: "Owner"}}},
		{"count-distinct", "true", "", []GroupCol{{Attr: "JobStatus"}},
			[]AggSpec{{Func: AggCountDistinct, Arg: "RequestCpus"}}},
		{"filtered", "true", "", []GroupCol{{Attr: "JobStatus"}},
			[]AggSpec{{Func: AggCount, Arg: "*", Filter: "RequestCpus >= 4"}}},
		{"filtered-ungrouped", "true", "", nil,
			[]AggSpec{{Func: AggCount, Arg: "*", Filter: "RequestCpus >= 4"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := d.AggregateColsStats(tc.constraint, tc.groupCols, tc.aggs, nil, nil)
			if err != nil {
				t.Fatalf("AggregateColsStats: %v", err)
			}
			ref := tc.constraint
			if tc.ref != "" {
				ref = tc.ref
			}
			want := scanGroupedMutable(t, d, ref, tc.groupCols, tc.aggs)
			g, w := aggLines(got), aggLines(want)
			if fmt.Sprint(g) != fmt.Sprint(w) {
				t.Errorf("ladder = %v\nscan   = %v", g, w)
			}
			if len(w) == 0 {
				t.Error("the reference scan produced no rows, so this shape proves nothing")
			}
		})
	}
}

// TestDBAggregateColsStatsBadConstraint checks the error the ladder reports for a constraint that
// does not parse. The RPC handler turns it straight into the error frame, so its text is wire-
// visible: it must stay the one QueryProject already produced.
func TestDBAggregateColsStatsBadConstraint(t *testing.T) {
	d := mutableGroupFixture(t, 64)
	// A shape no fast path can serve, so the ladder reaches the scan that parses the constraint.
	_, err := d.AggregateColsStats("JobStatus ===", []GroupCol{{Attr: "JobStatus"}},
		[]AggSpec{{Func: AggMax, Arg: "Owner"}}, nil, nil)
	if err == nil {
		t.Fatal("a malformed constraint was accepted")
	}
	_, want := d.QueryProject("JobStatus ===", []string{"JobStatus"})
	if want == nil {
		t.Fatal("QueryProject accepted the same malformed constraint")
	}
	if err.Error() != want.Error() {
		t.Errorf("error = %q, QueryProject reports %q", err, want)
	}
}

// TestDBAggregateColsStatsStats checks the stats argument reaches the scan and only the scan. A
// fast path reassembles no records, so leaving stats at zero is the signal that the aggregate was
// served without a per-record scan -- which is the whole reason an embedder would pass one.
func TestDBAggregateColsStatsStats(t *testing.T) {
	d := mutableGroupFixture(t, 5000)

	var fast collections.ScanStats
	if _, err := d.AggregateColsStats("RequestMemory > 4096", []GroupCol{{Attr: "JobStatus"}},
		[]AggSpec{{Func: AggCount, Arg: "*"}}, &fast, nil); err != nil {
		t.Fatal(err)
	}
	if fast.RecordsVisited != 0 || fast.RecordsReassembled != 0 {
		t.Errorf("a columnar-served aggregate reported a record scan: %+v", fast)
	}

	// MAX over a string column is not a shape the columnar accelerator covers, so this falls
	// through to the projected scan and the stats must show the work.
	var slow collections.ScanStats
	if _, err := d.AggregateColsStats("RequestMemory > 4096", []GroupCol{{Attr: "JobStatus"}},
		[]AggSpec{{Func: AggMax, Arg: "Owner"}}, &slow, nil); err != nil {
		t.Fatal(err)
	}
	if slow.RecordsVisited == 0 {
		t.Errorf("the scan fall-through reported no work: %+v", slow)
	}
}

// TestDBAggregateColsStatsStop checks the stop callback still abandons a scan. The RPC server
// passes its connection's cancellation through it, so a scan whose client has gone must stop
// rather than run to completion against a dead socket.
func TestDBAggregateColsStatsStop(t *testing.T) {
	d := mutableGroupFixture(t, 5000)
	groupCols := []GroupCol{{Attr: "JobStatus"}}
	aggs := []AggSpec{{Func: AggMax, Arg: "Owner"}} // forced to the scan, which is what polls stop

	full, err := d.AggregateColsStats("true", groupCols, aggs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	stopped, err := d.AggregateColsStats("true", groupCols, aggs, nil, func() bool {
		rows++
		return rows > 1 // let one row in, then abandon
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stopped) != 1 {
		t.Errorf("abandoned scan produced %d groups, want the 1 row it was allowed", len(stopped))
	}
	if len(full) <= 1 {
		t.Fatal("the unstopped aggregate has only one group, so stopping proves nothing")
	}
}
