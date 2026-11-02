package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
)

// collidingGroupFixture builds a table whose GROUP BY column holds the integer 1 and the real 1.0 in
// different records. Those are two distinct group values and ONE rendered label, so storage produces
// two groups that the shaper has to fold into a single row -- which, for caller-supplied
// accumulators, means calling Merge.
//
// It exists because the ladder's shared fixture is all integers, so nothing there ever collides and
// the merge path went untested: removing the Merge call entirely left every test green.
func collidingGroupFixture(t *testing.T, n int) *DB {
	t.Helper()
	d, err := OpenConfig(Config{SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	tx := d.Begin()
	for i := 0; i < n; i++ {
		ad := classad.New()
		// A FEW records carry the bucket as a real of equal value rather than an integer. Rare
		// rather than half, because a column split evenly between two kinds stops being a column
		// the schema will serve at all -- at which point the rung declines and the test proves
		// nothing. This is the shape that actually occurs: a mostly-integer attribute with the
		// occasional real.
		if i%50 == 0 {
			ad.InsertAttrFloat("Bucket", float64(i%3))
		} else {
			ad.InsertAttr("Bucket", int64(i%3))
		}
		ad.InsertAttr("Cpus", int64(i%7))
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

// TestGroupedUserAggregateMergesCollidingGroups drives the columnar rung and the reference scan over
// a fixture whose group labels collide, and requires that the rung actually served -- without that
// the comparison would pass by both sides being the scan.
func TestGroupedUserAggregateMergesCollidingGroups(t *testing.T) {
	name := registerOnce(t, "collide_sum_col", ladderSumColAgg{})
	d := collidingGroupFixture(t, 3000)
	req := AggRequest{
		Constraint: "true",
		GroupCols:  []GroupCol{{Attr: "Bucket"}},
		Aggs: []AggSpec{
			{Func: AggCount, Arg: "*"},
			{Func: AggUser, UserFunc: name, Arg: "Cpus"},
		},
	}
	ladder := d.aggLadder(nil, nil, false)
	var rung AggStrategy
	for _, s := range ladder {
		if s.Name() == "grouped-user-columns" {
			rung = s
		}
	}
	if rung == nil {
		t.Fatal("no grouped-user-columns rung in the ladder")
	}
	got, ok, err := rung.Answer(req)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the columnar rung declined; this test would otherwise compare the scan with itself")
	}
	want, ok, err := ladder[len(ladder)-1].Answer(req)
	if err != nil || !ok {
		t.Fatalf("reference: ok=%v err=%v", ok, err)
	}
	// The premise: the labels really do collide, so storage saw more groups than there are rows.
	if len(want) != 3 {
		t.Fatalf("want 3 merged rows (buckets 0,1,2), got %d: %+v", len(want), want)
	}
	asSet := func(rows []AggRow) map[string]string {
		out := map[string]string{}
		for _, r := range rows {
			out[strings.Join(r.Group, "\x00")] = strings.Join(r.Values, "\x00")
		}
		return out
	}
	g, w := asSet(got), asSet(want)
	if len(g) != len(w) {
		t.Fatalf("columnar produced %d rows, reference %d:\n got %+v\nwant %+v", len(g), len(w), got, want)
	}
	for k, wv := range w {
		if g[k] != wv {
			t.Errorf("group %q: columnar %q, reference %q", k, g[k], wv)
		}
	}
}

// TestGroupedUserAggregatePanicFailsTheQuery: containment holds on the grouped rung too.
func TestGroupedUserAggregatePanicFailsTheQuery(t *testing.T) {
	name := registerOnce(t, "grouped_panic_col", colPanics{})
	d := collidingGroupFixture(t, 500)
	rows, ok, err := d.aggLadderAnswer(t, "grouped-user-columns", AggRequest{
		Constraint: "true",
		GroupCols:  []GroupCol{{Attr: "Bucket"}},
		Aggs:       []AggSpec{{Func: AggUser, UserFunc: name, Arg: "Cpus"}},
	})
	if err == nil {
		t.Fatalf("a panicking AddNum should fail the query, got rows=%+v ok=%v", rows, ok)
	}
	if rows != nil || ok {
		t.Fatalf("a failed query must return no rows: rows=%+v ok=%v", rows, ok)
	}
	if !strings.Contains(err.Error(), name) {
		t.Fatalf("the error should name the aggregate that panicked, got %v", err)
	}
}

// aggLadderAnswer drives one named rung of a table's ladder.
func (db *DB) aggLadderAnswer(t *testing.T, rung string, req AggRequest) ([]AggRow, bool, error) {
	t.Helper()
	for _, s := range db.aggLadder(nil, nil, false) {
		if s.Name() == rung {
			return s.Answer(req)
		}
	}
	t.Fatalf("no %s rung in the ladder", rung)
	return nil, false, nil
}

// refusingGroupValues fails the test if the rung runs a pass for a request it should have refused.
type refusingGroupValues struct{ t *testing.T }

func (r refusingGroupValues) GroupValuesConstraintAggs(string, []collections.GroupSpec,
	[]collections.GroupAggCol, []string, func() collections.GroupSink, collections.MergeGroupSink) ([]collections.GroupValues, bool) {
	r.t.Errorf("the grouped rung ran a pass for a request it should have refused")
	return nil, false
}

func (r refusingGroupValues) GroupValuesAllAggs([]collections.GroupSpec, []collections.GroupAggCol,
	[]string, func() collections.GroupSink, collections.MergeGroupSink) ([]collections.GroupValues, bool) {
	r.t.Errorf("the grouped rung ran a pass for a request it should have refused")
	return nil, false
}

// TestGroupedUserFromColumnsRefusals pins what this rung will not take. The grouped COUNT(*) case is
// the one worth stating: it is answerable here and still refused, because it belongs to the rung
// that already serves it -- answering it would move a shape onto a new path for no gain.
func TestGroupedUserFromColumnsRefusals(t *testing.T) {
	name := registerOnce(t, "grp_refuse_col", ladderSumColAgg{})
	scanName := registerOnce(t, "grp_refuse_scan", ladderSumScanAgg{})
	src := refusingGroupValues{t}
	g := []GroupCol{{Attr: "Owner"}}
	for _, c := range []struct {
		what   string
		groups []GroupCol
		aggs   []AggSpec
	}{
		{"no grouping", nil, []AggSpec{{Func: AggUser, UserFunc: name, Arg: "N"}}},
		{"grouped COUNT(*) with no user aggregate", g, []AggSpec{{Func: AggCount, Arg: "*"}}},
		{"beside a built-in value aggregate", g, []AggSpec{{Func: AggSum, Arg: "N"}, {Func: AggUser, UserFunc: name, Arg: "N"}}},
		{"beside COUNT(DISTINCT)", g, []AggSpec{{Func: AggCountDistinct, Arg: "N"}, {Func: AggUser, UserFunc: name, Arg: "N"}}},
		{"an aggregate that did not opt in", g, []AggSpec{{Func: AggUser, UserFunc: scanName, Arg: "N"}}},
		{"an unregistered name", g, []AggSpec{{Func: AggUser, UserFunc: "grp_nope", Arg: "N"}}},
		{"over star", g, []AggSpec{{Func: AggUser, UserFunc: name, Arg: "*"}}},
		{"a negative bucket width", []GroupCol{{Attr: "Owner", BucketWidth: -1}}, []AggSpec{{Func: AggUser, UserFunc: name, Arg: "N"}}},
		{"no aggregates", g, nil},
	} {
		t.Run(c.what, func(t *testing.T) {
			rows, ok, err := GroupedUserFromColumns(src, "true", c.groups, c.aggs)
			if ok || err != nil {
				t.Fatalf("served %s: rows=%+v ok=%v err=%v", c.what, rows, ok, err)
			}
		})
	}
}

// shortCountOfSource answers with fewer filtered counts than were asked for -- what an
// implementation predating per-aggregate FILTER would do. Reading past what it returned would panic
// rather than say so, so the rung has to check and decline.
type shortCountOfSource struct{}

func (shortCountOfSource) GroupValuesConstraintAggs(_ string, _ []collections.GroupSpec,
	_ []collections.GroupAggCol, _ []string, newSink func() collections.GroupSink, _ collections.MergeGroupSink) ([]collections.GroupValues, bool) {
	return []collections.GroupValues{{
		Values: []classad.Value{classad.NewStringValue("alice")}, Count: 1, Sink: newSink(),
	}}, true
}

func (s shortCountOfSource) GroupValuesAllAggs(_ []collections.GroupSpec, _ []collections.GroupAggCol,
	_ []string, newSink func() collections.GroupSink, _ collections.MergeGroupSink) ([]collections.GroupValues, bool) {
	return s.GroupValuesConstraintAggs("", nil, nil, nil, newSink, nil)
}

func TestGroupedUserDeclinesAShortCountOf(t *testing.T) {
	name := registerOnce(t, "grp_shortcount_col", ladderSumColAgg{})
	rows, ok, err := GroupedUserFromColumns(shortCountOfSource{}, "true",
		[]GroupCol{{Attr: "Owner"}}, []AggSpec{
			{Func: AggUser, UserFunc: name, Arg: "N"},
			{Func: AggCount, Arg: "*", Filter: "N > 1"},
		})
	if ok || err != nil || rows != nil {
		t.Fatalf("answered from a source that ignored the filters: rows=%+v ok=%v err=%v", rows, ok, err)
	}
}
