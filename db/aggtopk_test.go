package db

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// topKFixture seeds owners with distinct QDates so the k-th newest per owner is known by hand:
// owner i holds QDates i*1000 + 0..rows-1, and a tail of records whose QDate is a string, which
// must not reach the cutoff.
func topKFixture(t *testing.T, owners, rows int) *DB {
	t.Helper()
	d, err := OpenConfig(Config{SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	tx := d.Begin()
	n := 0
	for o := 0; o < owners; o++ {
		for r := 0; r < rows; r++ {
			ad := classad.New()
			ad.InsertAttrString("Owner", fmt.Sprintf("o%d", o))
			ad.InsertAttr("QDate", int64(o*1000+r))
			tx.NewClassAd(fmt.Sprintf("k%d", n), ad)
			n++
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return d
}

func topKRows(t *testing.T, d *DB, k int, desc bool) map[string]string {
	t.Helper()
	req := AggRequest{
		Constraint: "true",
		GroupCols:  []GroupCol{{Attr: "Owner"}},
		Aggs:       []AggSpec{{Func: AggTopKThreshold, Arg: "QDate", K: k, Desc: desc}},
	}
	rows, err := d.AggregateColsStats(req.Constraint, req.GroupCols, req.Aggs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range rows {
		// The group label for a string column arrives QUOTED (GroupValueText quotes, so that
		// "4" and 4 are different groups); the fixture's owners are plain names, so unquote
		// for a readable key.
		key := strings.Join(r.Group, "\x00")
		if u, err := strconv.Unquote(key); err == nil {
			key = u
		}
		out[key] = strings.Join(r.Values, "\x00")
	}
	return out
}

// TestTopKThresholdIsTheKthValue is the arithmetic: owner o holds QDates o*1000 .. o*1000+9, so the
// 5th NEWEST is o*1000+5 and the 5th OLDEST is o*1000+4. Getting this off by one would still look
// plausible in every other test, which is why it is pinned against hand-computed numbers.
func TestTopKThresholdIsTheKthValue(t *testing.T) {
	d := topKFixture(t, 3, 10)

	got := topKRows(t, d, 5, true)
	for o := 0; o < 3; o++ {
		want := strconv.Itoa(o*1000 + 5)
		if got[fmt.Sprintf("o%d", o)] != want {
			t.Errorf("5th newest of o%d = %q, want %q", o, got[fmt.Sprintf("o%d", o)], want)
		}
	}

	got = topKRows(t, d, 5, false)
	for o := 0; o < 3; o++ {
		want := strconv.Itoa(o*1000 + 4)
		if got[fmt.Sprintf("o%d", o)] != want {
			t.Errorf("5th oldest of o%d = %q, want %q", o, got[fmt.Sprintf("o%d", o)], want)
		}
	}
}

// TestTopKThresholdShortGroupIsUndefined covers the case the caller has to branch on: a group with
// fewer than k rows has no cutoff, and must say so rather than report its minimum. Reporting the
// minimum would read as a valid cutoff and constrain away nothing -- harmless here -- but reporting
// its MAXIMUM would silently drop every row but one, and a caller cannot tell the two apart from a
// number alone.
func TestTopKThresholdShortGroupIsUndefined(t *testing.T) {
	d := topKFixture(t, 2, 3)
	got := topKRows(t, d, 5, true)
	for o := 0; o < 2; o++ {
		if v := got[fmt.Sprintf("o%d", o)]; v != "undefined" {
			t.Errorf("cutoff of a 3-row group at k=5 = %q, want undefined", v)
		}
	}
	// At exactly k the cutoff exists and is the whole group's worst value.
	got = topKRows(t, d, 3, true)
	for o := 0; o < 2; o++ {
		want := strconv.Itoa(o * 1000)
		if v := got[fmt.Sprintf("o%d", o)]; v != want {
			t.Errorf("cutoff of a 3-row group at k=3 = %q, want %q", v, want)
		}
	}
}

// TestTopKThresholdCutoffAdmitsTheTopK is the property the feature exists for, stated as the caller
// would use it: constrain the table by the cutoff and you get the k newest of each group, and no
// more than that bar ties.
func TestTopKThresholdCutoffAdmitsTheTopK(t *testing.T) {
	const owners, rows, k = 4, 20, 5
	d := topKFixture(t, owners, rows)
	cuts := topKRows(t, d, k, true)

	for o := 0; o < owners; o++ {
		owner := fmt.Sprintf("o%d", o)
		cut, err := strconv.Atoi(cuts[owner])
		if err != nil {
			t.Fatalf("cutoff for %s = %q: %v", owner, cuts[owner], err)
		}
		constraint := fmt.Sprintf(`Owner == %q && QDate >= %d`, owner, cut)
		seq, qerr := d.Query(constraint)
		if qerr != nil {
			t.Fatal(qerr)
		}
		var got []int
		for ad := range seq {
			v, verr := ad.EvaluateAttr("QDate").IntValue()
			if verr != nil {
				t.Fatal(verr)
			}
			got = append(got, int(v))
		}
		sort.Sort(sort.Reverse(sort.IntSlice(got)))
		if len(got) != k {
			t.Fatalf("%s: cutoff %d admitted %d rows, want %d: %v", owner, cut, len(got), k, got)
		}
		for i, q := range got {
			if want := o*1000 + rows - 1 - i; q != want {
				t.Errorf("%s: row %d = %d, want %d", owner, i, q, want)
			}
		}
	}
}

// TestTopKThresholdColumnarMatchesScan drives the columnar grouped rung and the reference scan over
// the same request and requires the rung to have SERVED. Without that check the comparison would be
// the scan against itself, which is the way a columnar path quietly stops being exercised.
func TestTopKThresholdColumnarMatchesScan(t *testing.T) {
	d := topKFixture(t, 5, 900)
	if !d.EnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to accelerate")
	}
	req := AggRequest{
		Constraint: "true",
		GroupCols:  []GroupCol{{Attr: "Owner"}},
		Aggs: []AggSpec{
			{Func: AggCount, Arg: "*"},
			{Func: AggTopKThreshold, Arg: "QDate", K: 7, Desc: true},
			{Func: AggTopKThreshold, Arg: "QDate", K: 3, Desc: false},
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
	for key, wv := range w {
		if gv, has := g[key]; !has || gv != wv {
			t.Errorf("group %q: columnar %q, reference %q", key, gv, wv)
		}
	}
}

// TestTopKThresholdObeysTheAccumulatorLaws runs the cutoff through the same checker a user-defined
// aggregate is held to. Merge has to be associative AND commutative here for the ordinary reason --
// the columnar pass fans out across segment windows and the fold order is a race.
func TestTopKThresholdObeysTheAccumulatorLaws(t *testing.T) {
	samples := []classad.Value{
		classad.NewIntValue(5), classad.NewIntValue(1), classad.NewIntValue(9),
		classad.NewRealValue(2.5), classad.NewIntValue(9), classad.NewIntValue(-3),
		classad.NewStringValue("nope"), classad.NewUndefinedValue(), classad.NewErrorValue(),
	}
	for _, c := range []struct {
		k    int
		desc bool
	}{{1, true}, {3, true}, {3, false}, {8, true}} {
		if err := CheckAggregate(topKAggregate{k: c.k, desc: c.desc}, samples); err != nil {
			t.Errorf("k=%d desc=%v: %v", c.k, c.desc, err)
		}
	}
}

// TestTopKThresholdSkipsNaN pins the one value that would turn a cutoff into a filter matching
// nothing: a NaN is never evicted by a comparison, so keeping it would make it the reported k-th.
func TestTopKThresholdSkipsNaN(t *testing.T) {
	a := topKAggregate{k: 2, desc: true}.NewAccumulator()
	a.Add(classad.NewRealValue(math.NaN()))
	a.Add(classad.NewIntValue(10))
	a.Add(classad.NewIntValue(20))
	a.Add(classad.NewRealValue(math.NaN()))
	if got := ValueText(a.Result()); got != "10" {
		t.Errorf("cutoff over a column holding NaNs = %q, want 10", got)
	}
}

// TestTopKThresholdIntegersCompareExactly covers the pair of integers whose FLOAT FORMS ARE EQUAL.
// 2^53+1 is not representable as a float64 and rounds to 2^53, so comparing on F calls the two
// values equal, declines to evict, and reports the smaller as the cutoff -- a cutoff that excludes
// the very row it should admit. Only the exact integer comparison separates them.
//
// 2^53 and 2^53+2 would NOT cover this: their float forms differ, so the float comparison gets
// that pair right and the test would pass with the exactness removed.
func TestTopKThresholdIntegersCompareExactly(t *testing.T) {
	const big = int64(1) << 53
	if float64(big) != float64(big+1) {
		t.Fatalf("premise broken: %d and %d have distinct float forms", big, big+1)
	}
	for _, order := range [][2]int64{{big, big + 1}, {big + 1, big}} {
		a := topKAggregate{k: 1, desc: true}.NewAccumulator()
		a.Add(classad.NewIntValue(order[0]))
		a.Add(classad.NewIntValue(order[1]))
		if got := ValueText(a.Result()); got != strconv.FormatInt(big+1, 10) {
			t.Errorf("largest of %d and %d (added in that order) = %q, want %d",
				order[0], order[1], got, big+1)
		}
	}
}

// TestTopKThresholdRefusals pins the requests that have no answer, refused rather than reported as
// undefined -- which is what a group of fewer than K rows says and would read as "no cutoff here"
// instead of "that request makes no sense".
func TestTopKThresholdRefusals(t *testing.T) {
	d := topKFixture(t, 2, 4)
	for _, c := range []struct {
		name string
		spec AggSpec
	}{
		{"k=0", AggSpec{Func: AggTopKThreshold, Arg: "QDate", K: 0, Desc: true}},
		{"negative k", AggSpec{Func: AggTopKThreshold, Arg: "QDate", K: -2, Desc: true}},
		{"star", AggSpec{Func: AggTopKThreshold, Arg: "*", K: 3, Desc: true}},
		{"no argument", AggSpec{Func: AggTopKThreshold, Arg: "", K: 3, Desc: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := d.AggregateColsStats("true",
				[]GroupCol{{Attr: "Owner"}}, []AggSpec{c.spec}, nil, nil)
			if err == nil {
				t.Fatal("accepted a request with no answer")
			}
		})
	}
}
