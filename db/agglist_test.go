package db

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// listAgg is the reduction whose answer is the VALUES of each group rather than a reduction of
// them: one group per job, each carrying its history rows' exit codes.
func listAgg(kind JoinKind) JoinAggSpec {
	return JoinAggSpec{
		Join:      equiSpec(kind),
		GroupCols: []GroupCol{{Attr: "j.Owner"}},
		Aggs: []AggSpec{
			{Func: AggCount, Arg: "*"},
			{Func: AggList, Arg: "h.ExitCode"},
		},
	}
}

// sortedListCell reads a list cell back and renders its elements sorted, because the ORDER
// inside the cell is the join's row order and a GROUP BY defines none -- see AggList. Every
// comparison of a cell's contents in this file goes through here.
func sortedListCell(t *testing.T, cell string) string {
	t.Helper()
	elems, err := ParseGroupValue(cell).ListValue()
	if err != nil {
		t.Fatalf("the cell %q did not read back as a list: %v", cell, err)
	}
	out := make([]string, 0, len(elems))
	for _, e := range elems {
		out = append(out, GroupValueText(e))
	}
	sort.Strings(out)
	return "{" + strings.Join(out, ",") + "}"
}

// TestJoinAggregateListReturnsTheValues is the claim: a group's cell holds the matched values,
// which no COUNT/SUM/MIN/MAX of them can reconstruct.
func TestJoinAggregateListReturnsTheValues(t *testing.T) {
	cat := joinFixture(t)
	var st JoinStats
	rows, err := cat.JoinAggregate(listAgg(JoinInner), JoinLimits{}, &st, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[strings.Join(r.Group, "|")] = r.Values[0] + "," + sortedListCell(t, r.Values[1])
	}
	want := map[string]string{`"alice"`: "1,{0}", `"bob"`: "2,{1,7}"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	// The reduction is still a reduction: 3 joined rows in, 2 groups out, with the
	// two-row group's values both in one cell.
	if st.Rows != 3 || st.Groups != 2 {
		t.Errorf("stats say %d joined rows and %d groups, want 3 and 2", st.Rows, st.Groups)
	}
}

// TestJoinAggregateListCellIsAClassAdLiteral pins the rendering contract the wire depends on:
// the cell is text a caller reads back as the value it is.
func TestJoinAggregateListCellIsAClassAdLiteral(t *testing.T) {
	cat := joinFixture(t)
	rows, err := cat.JoinAggregate(listAgg(JoinInner), JoinLimits{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, r := range rows {
		v := ParseGroupValue(r.Values[1])
		elems, lerr := v.ListValue()
		if lerr != nil {
			t.Fatalf("the cell %q did not read back as a list: %v", r.Values[1], lerr)
		}
		if GroupValueText(v) != r.Values[1] {
			t.Errorf("the cell %q round-tripped to %q", r.Values[1], GroupValueText(v))
		}
		seen += len(elems)
	}
	if seen != 3 {
		t.Fatalf("the lists held %d values in total, want the join's 3 rows", seen)
	}
}

// TestJoinAggregateListKeepsUndefinedSoTwoListsStayAligned is the rule that makes two list
// aggregates readable as pairs. A LEFT join's unmatched row contributes an undefined value to
// every list, so element i of each still came from row i.
func TestJoinAggregateListKeepsUndefinedSoTwoListsStayAligned(t *testing.T) {
	cat := joinFixture(t)
	spec := listAgg(JoinLeft)
	spec.GroupCols = []GroupCol{{Attr: "j.ClusterId"}}
	spec.Aggs = []AggSpec{
		{Func: AggCount, Arg: "*"},
		{Func: AggList, Arg: "h.ExitCode"},
		{Func: AggList, Arg: "j.Cpus"},
	}
	rows, err := cat.JoinAggregate(spec, JoinLimits{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	unmatched := 0
	for _, r := range rows {
		exits, err1 := ParseGroupValue(r.Values[1]).ListValue()
		cpus, err2 := ParseGroupValue(r.Values[2]).ListValue()
		if err1 != nil || err2 != nil {
			t.Fatalf("group %v: cells %q / %q did not read back as lists", r.Group, r.Values[1], r.Values[2])
		}
		if len(exits) != len(cpus) {
			t.Errorf("group %v: %d exit codes against %d cpu values; the lists are not aligned",
				r.Group, len(exits), len(cpus))
		}
		if strings.Contains(r.Values[1], "undefined") {
			unmatched++
		}
	}
	if unmatched == 0 {
		t.Fatal("no group held an undefined value, so 'undefined is kept' is vacuous here")
	}
}

// TestJoinAggregateListIsCutAtTheCapAndSaysSo is the degradation. A cut list is useful where a
// refusal is not -- the group is still the right group and its COUNT is still every row -- but
// only because the cut is reported rather than implied.
func TestJoinAggregateListIsCutAtTheCapAndSaysSo(t *testing.T) {
	cat := joinFixture(t)
	spec := listAgg(JoinInner)
	spec.MaxListValues = 1
	rows, err := cat.JoinAggregate(spec, JoinLimits{}, nil, nil)
	if err != nil {
		t.Fatalf("a cap on the list must cut rather than refuse: %v", err)
	}
	cut, whole := 0, 0
	for _, r := range rows {
		switch r.Values[0] {
		case "2":
			if len(r.Elided) != 2 || r.Elided[1] != 1 {
				t.Errorf("a 2-row group cut to 1 value reported Elided %v, want 1 dropped "+
					"from aggregate 1", r.Elided)
			}
			// WHICH value survives is whichever joined row reached the accumulator
			// first, which the join's plan decides; how MANY survive is the cap.
			if cell := sortedListCell(t, r.Values[1]); cell != "{1}" && cell != "{7}" {
				t.Errorf("the cut cell is %q, want one of the group's two values", cell)
			}
			cut++
		case "1":
			if len(r.Elided) != 0 {
				t.Errorf("a 1-row group under a cap of 1 reported Elided %v", r.Elided)
			}
			whole++
		}
	}
	if cut == 0 {
		t.Fatal("no group was cut, so the elision report proves nothing")
	}
	if whole == 0 {
		t.Fatal("no group fitted under the cap, so 'reports nothing elided' is vacuous")
	}
	// Uncapped, nothing is elided and the whole list is there: the report above is the cap
	// firing, not a field that is always set.
	spec.MaxListValues = 0
	rows, err = cat.JoinAggregate(spec, JoinLimits{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if len(r.Elided) != 0 {
			t.Errorf("group %v reported Elided %v with no cap", r.Group, r.Elided)
		}
	}
	for _, r := range rows {
		if r.Values[0] == "2" && sortedListCell(t, r.Values[1]) != "{1,7}" {
			t.Errorf("the uncapped 2-row group holds %q, want both values", r.Values[1])
		}
	}
}

// TestAggregateValuesRefusesAListAggregate pins the containment. Every reduction that is not a
// join aggregate goes through AggregateValues, which has no field for a cap -- so the refusal
// is structural rather than a check each caller has to remember.
func TestAggregateValuesRefusesAListAggregate(t *testing.T) {
	seq := func(yield func([]classad.Value) bool) {
		yield([]classad.Value{classad.NewIntValue(1)})
	}
	aggs := []AggSpec{{Func: AggList, Arg: "a"}}
	_, err := AggregateValues(seq, []string{"a"}, nil, aggs, nil, []int{0}, nil)
	if !errors.Is(err, ErrListAggregateNeedsBound) {
		t.Fatalf("AggregateValues with a list aggregate gave %v, want ErrListAggregateNeedsBound", err)
	}
	// The same call with a COUNT is served, so the refusal is the aggregate.
	rows, err := AggregateValues(seq, []string{"a"}, nil,
		[]AggSpec{{Func: AggCount, Arg: "*"}}, nil, []int{-1}, nil)
	if err != nil || len(rows) != 1 || rows[0].Values[0] != "1" {
		t.Fatalf("a COUNT over the same sequence gave %v, %v", rows, err)
	}
}

// TestJoinAggregateRefusesListOfEverything: the rows of a group are not a value, so there is
// nothing for LIST(*) to hold.
func TestJoinAggregateRefusesListOfEverything(t *testing.T) {
	cat := joinFixture(t)
	spec := listAgg(JoinInner)
	spec.Aggs = []AggSpec{{Func: AggList, Arg: "*"}}
	if _, err := cat.JoinAggregate(spec, JoinLimits{}, nil, nil); err == nil {
		t.Fatal("LIST(*) was served")
	} else if !strings.Contains(err.Error(), "LIST(*)") {
		t.Errorf("error = %v, want one naming LIST(*)", err)
	}
}

// TestJoinAggCapsAreTheRequestsWithinACeiling pins the clamp, which is the opposite shape to
// JoinLimits.clamp: a caller names one of these to ask for MORE, and the ceiling is what it
// may have.
func TestJoinAggCapsAreTheRequestsWithinACeiling(t *testing.T) {
	for _, c := range []struct {
		want, def, ceiling, out int
		why                     string
	}{
		{0, 100, 1000, 100, "unasked for takes the default"},
		{-5, 100, 1000, 100, "nonsense takes the default"},
		{50, 100, 1000, 50, "a cap below the default is the request's"},
		{500, 100, 1000, 500, "a cap ABOVE the default is the request's -- the point"},
		{1000, 100, 1000, 1000, "the ceiling itself is allowed"},
		{9999, 100, 1000, 1000, "past the ceiling is the ceiling"},
	} {
		if got := clampJoinAggCap(c.want, c.def, c.ceiling); got != c.out {
			t.Errorf("clampJoinAggCap(%d, %d, %d) = %d, want %d (%s)",
				c.want, c.def, c.ceiling, got, c.out, c.why)
		}
	}
	// The defaults and ceilings the caps actually run under, pinned because raising the
	// DEFAULT is the thing this design deliberately did not do.
	if maxJoinGroupsDefault != 100_000 {
		t.Errorf("the default group cap is %d; raising it hands the raise to every request "+
			"including the ones the cap exists to catch", maxJoinGroupsDefault)
	}
	if maxJoinGroupsCeiling <= maxJoinGroupsDefault {
		t.Errorf("the ceiling %d is not above the default %d, so no request can ask for more",
			maxJoinGroupsCeiling, maxJoinGroupsDefault)
	}
	if maxJoinListCeiling <= maxJoinListDefault {
		t.Errorf("the list ceiling %d is not above the default %d",
			maxJoinListCeiling, maxJoinListDefault)
	}
}

// TestJoinAggregateGroupCapCanBeRaisedPastTheDefault is the caller's case said as a query: a
// grouping key that IS the entity has one group per entity, and the default cap was written
// for a key that classifies.
func TestJoinAggregateGroupCapCanBeRaisedPastTheDefault(t *testing.T) {
	cat := joinFixture(t)
	spec := ownerCount(JoinInner)
	spec.GroupCols = []GroupCol{{Attr: "j.ClusterId"}}

	// A cap of 1 refuses, so the cap is live over this query.
	tight := spec
	tight.MaxGroups = 1
	if _, err := cat.JoinAggregate(tight, JoinLimits{}, nil, nil); err == nil {
		t.Fatal("a cap of 1 over a 2-group query was served; the cap is not live")
	} else if !strings.Contains(err.Error(), fmt.Sprint(maxJoinGroupsCeiling)) {
		t.Errorf("the refusal %v does not say how high the request may ask", err)
	}
	// Raised past it, the same query answers.
	wide := spec
	wide.MaxGroups = maxJoinGroupsCeiling
	rows, err := cat.JoinAggregate(wide, JoinLimits{}, nil, nil)
	if err != nil {
		t.Fatalf("the same query under the ceiling: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d groups, want 2", len(rows))
	}
}
