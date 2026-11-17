package db

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// topKRowsFixture seeds owners x rows jobs: owner o holds QDates o*1000 + 0..rows-1, with Cpus
// carrying the same index so a test can tell which row it got back.
func topKRowsFixture(t *testing.T, owners, rows int) *DB {
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
			ad.InsertAttr("Cpus", int64(r))
			tx.NewClassAd(fmt.Sprintf("k%d", n), ad)
			n++
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return d
}

// byOwner indexes the answer by its single group value, unquoted for readability.
func byOwner(t *testing.T, groups []TopKRowsGroup) map[string]TopKRowsGroup {
	t.Helper()
	out := map[string]TopKRowsGroup{}
	for _, g := range groups {
		s, err := g.Values[0].StringValue()
		if err != nil {
			t.Fatalf("group value %v is not a string: %v", g.Values[0], err)
		}
		out[s] = g
	}
	return out
}

// TestTopKRowsPerGroupKeepsTheBestRows is the whole feature: k rows per group, the right ones,
// in order, and nothing else.
func TestTopKRowsPerGroupKeepsTheBestRows(t *testing.T) {
	const owners, rows, k = 3, 20, 5
	d := topKRowsFixture(t, owners, rows)

	got, err := d.TopKRowsPerGroup("true", TopKRowsSpec{
		GroupCols: []GroupCol{{Attr: "Owner"}},
		OrderBy:   OrderOne("QDate", true),
		K:         k,
		Attrs:     []string{"Cpus"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != owners {
		t.Fatalf("groups = %d, want %d", len(got), owners)
	}
	for owner, g := range byOwner(t, got) {
		if len(g.Rows) != k {
			t.Fatalf("%s: %d rows, want %d", owner, len(g.Rows), k)
		}
		// Newest first, so Cpus counts down from rows-1.
		for i, row := range g.Rows {
			if len(row) != 1 {
				t.Fatalf("%s row %d has %d columns, want 1 (only the asked-for Cpus)", owner, i, len(row))
			}
			c, cerr := row[0].IntValue()
			if cerr != nil {
				t.Fatal(cerr)
			}
			if want := int64(rows - 1 - i); c != want {
				t.Errorf("%s row %d: Cpus = %d, want %d", owner, i, c, want)
			}
		}
	}
}

// TestTopKRowsAscendingTakesTheOtherEnd covers the direction flag against the same data, which is
// the cheapest way for a reversed comparison to go unnoticed.
func TestTopKRowsAscendingTakesTheOtherEnd(t *testing.T) {
	d := topKRowsFixture(t, 2, 20)
	got, err := d.TopKRowsPerGroup("true", TopKRowsSpec{
		GroupCols: []GroupCol{{Attr: "Owner"}},
		OrderBy:   OrderOne("QDate", false),
		K:         3,
		Attrs:     []string{"Cpus"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for owner, g := range byOwner(t, got) {
		var cpus []int64
		for _, row := range g.Rows {
			c, _ := row[0].IntValue() //nolint:errcheck // the fixture writes integers
			cpus = append(cpus, c)
		}
		if len(cpus) != 3 || cpus[0] != 0 || cpus[1] != 1 || cpus[2] != 2 {
			t.Errorf("%s oldest three = %v, want [0 1 2]", owner, cpus)
		}
	}
}

// TestTopKRowsShortGroupReturnsAllOfIt pins that a group with fewer than K ranked rows is returned
// whole rather than dropped -- the case the cutoff form has to report as undefined.
func TestTopKRowsShortGroupReturnsAllOfIt(t *testing.T) {
	d := topKRowsFixture(t, 2, 3)
	got, err := d.TopKRowsPerGroup("true", TopKRowsSpec{
		GroupCols: []GroupCol{{Attr: "Owner"}},
		OrderBy:   OrderOne("QDate", true),
		K:         10,
		Attrs:     []string{"Cpus"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for owner, g := range byOwner(t, got) {
		if len(g.Rows) != 3 {
			t.Errorf("%s: %d rows, want the whole 3-row group", owner, len(g.Rows))
		}
	}
}

// TestTopKRowsIsExactOnTies is the difference from the cutoff form, which admits every row tied
// with the k-th and so returns more than k. Here five rows share one QDate and k is 2, so exactly
// two come back.
func TestTopKRowsIsExactOnTies(t *testing.T) {
	d, err := OpenConfig(Config{SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tx := d.Begin()
	for i := 0; i < 5; i++ {
		ad := classad.New()
		ad.InsertAttrString("Owner", "alice")
		ad.InsertAttr("QDate", int64(100)) // all tied
		ad.InsertAttr("Cpus", int64(i))
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := d.TopKRowsPerGroup("true", TopKRowsSpec{
		GroupCols: []GroupCol{{Attr: "Owner"}},
		OrderBy:   OrderOne("QDate", true), K: 2, Attrs: []string{"Cpus"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Rows) != 2 {
		t.Fatalf("five tied rows at k=2 returned %d groups / %d rows, want 1 / 2",
			len(got), len(got[0].Rows))
	}
}

// TestTopKRowsSkipsUnrankableRows covers the rows that have nothing to order them by. They are
// not ranked and must not displace a row that is -- a string sorting as "greater than" every
// number would otherwise take the whole answer.
func TestTopKRowsSkipsUnrankableRows(t *testing.T) {
	d, err := OpenConfig(Config{SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tx := d.Begin()
	for i := 0; i < 6; i++ {
		ad := classad.New()
		ad.InsertAttrString("Owner", "alice")
		ad.InsertAttr("Cpus", int64(i))
		switch i % 3 {
		case 0:
			ad.InsertAttr("QDate", int64(i))
		case 1:
			ad.InsertAttrString("QDate", "not a time")
		case 2: // no QDate at all
		}
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := d.TopKRowsPerGroup("true", TopKRowsSpec{
		GroupCols: []GroupCol{{Attr: "Owner"}},
		OrderBy:   OrderOne("QDate", true), K: 5, Attrs: []string{"Cpus"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("groups = %d, want 1", len(got))
	}
	var cpus []int
	for _, row := range got[0].Rows {
		c, _ := row[0].IntValue() //nolint:errcheck // the fixture writes integers
		cpus = append(cpus, int(c))
	}
	sort.Ints(cpus)
	// Only i = 0 and 3 carry a numeric QDate.
	if len(cpus) != 2 || cpus[0] != 0 || cpus[1] != 3 {
		t.Errorf("ranked rows = %v, want the two with a numeric QDate ([0 3])", cpus)
	}
}

// TestTopKRowsNoGroupColsIsOnePartition covers the degenerate PARTITION BY, which SQL defines as
// one partition over the whole result and which a caller writing `OVER (ORDER BY ...)` asks for.
func TestTopKRowsNoGroupColsIsOnePartition(t *testing.T) {
	d := topKRowsFixture(t, 4, 10)
	got, err := d.TopKRowsPerGroup("true", TopKRowsSpec{
		OrderBy: OrderOne("QDate", true), K: 3, Attrs: []string{"QDate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("groups = %d, want 1", len(got))
	}
	// The highest QDates in the table belong to owner 3: 3009, 3008, 3007.
	want := []int64{3009, 3008, 3007}
	for i, row := range got[0].Rows {
		q, _ := row[0].IntValue() //nolint:errcheck // the fixture writes integers
		if q != want[i] {
			t.Errorf("row %d = %d, want %d", i, q, want[i])
		}
	}
}

// TestTopKRowsHonoursTheConstraint pins that the ranking is over the rows the WHERE matched. A
// ranking taken before the filter is a plausible answer made of the wrong rows.
func TestTopKRowsHonoursTheConstraint(t *testing.T) {
	d := topKRowsFixture(t, 2, 20)
	got, err := d.TopKRowsPerGroup("Cpus < 5", TopKRowsSpec{
		GroupCols: []GroupCol{{Attr: "Owner"}},
		OrderBy:   OrderOne("QDate", true), K: 2, Attrs: []string{"Cpus"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for owner, g := range byOwner(t, got) {
		var cpus []int64
		for _, row := range g.Rows {
			c, _ := row[0].IntValue() //nolint:errcheck // the fixture writes integers
			cpus = append(cpus, c)
		}
		if len(cpus) != 2 || cpus[0] != 4 || cpus[1] != 3 {
			t.Errorf("%s newest two under Cpus < 5 = %v, want [4 3]", owner, cpus)
		}
	}
}

// TestTopKRowsRanksBooleansLikeTheColumnPass is the divergence that the two conversions hid: the
// column pass offers a boolean as NumBool, so a hand-rolled Add that skipped booleans ranked a
// boolean order column differently depending on which rung answered. Both now go through
// numValueOf, and this pins it for the row rung.
func TestTopKRowsRanksBooleansLikeTheColumnPass(t *testing.T) {
	d, err := OpenConfig(Config{SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tx := d.Begin()
	for i := 0; i < 4; i++ {
		ad := classad.New()
		ad.InsertAttrString("Owner", "alice")
		ad.InsertAttrBool("Held", i%2 == 0)
		ad.InsertAttr("Cpus", int64(i))
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := d.TopKRowsPerGroup("true", TopKRowsSpec{
		GroupCols: []GroupCol{{Attr: "Owner"}},
		OrderBy:   OrderOne("Held", true), K: 2, Attrs: []string{"Cpus"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Rows) != 2 {
		t.Fatalf("booleans were not ranked: %d groups, %v", len(got), got)
	}
	// true coerces to 1, so the two true rows (Cpus 0 and 2) win.
	var cpus []int
	for _, row := range got[0].Rows {
		c, _ := row[0].IntValue() //nolint:errcheck // the fixture writes integers
		cpus = append(cpus, int(c))
	}
	sort.Ints(cpus)
	if cpus[0] != 0 || cpus[1] != 2 {
		t.Errorf("top two by a boolean = %v, want the true rows [0 2]", cpus)
	}
}

// TestTopKRowsSkipsNaN is the value that would hold a slot against every real row: a NaN loses
// every comparison, so it is never the worst and is never evicted. Three NaNs at k=3 would take
// the whole answer and report it as a full one.
//
// THE NaNs ARE INSERTED LAST ON PURPOSE. The scan yields newest first, so they arrive FIRST and
// fill the keep before any real row is offered -- which is the only order in which the guard
// matters. Written the other way round the NaNs arrive last, a full keep of real values rejects
// them on its own, and the test passes with the guard deleted. It did: mutation testing is what
// caught it.
func TestTopKRowsSkipsNaN(t *testing.T) {
	d, err := OpenConfig(Config{SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tx := d.Begin()
	for i := 0; i < 6; i++ {
		ad := classad.New()
		ad.InsertAttrString("Owner", "alice")
		ad.InsertAttr("Cpus", int64(i))
		if i < 3 {
			ad.InsertAttr("QDate", int64(i))
		} else {
			ad.InsertAttrFloat("QDate", math.NaN())
		}
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := d.TopKRowsPerGroup("true", TopKRowsSpec{
		GroupCols: []GroupCol{{Attr: "Owner"}},
		OrderBy:   OrderOne("QDate", true), K: 3, Attrs: []string{"Cpus"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("groups = %d, want 1", len(got))
	}
	var cpus []int
	for _, row := range got[0].Rows {
		c, _ := row[0].IntValue() //nolint:errcheck // the fixture writes integers
		cpus = append(cpus, int(c))
	}
	sort.Ints(cpus)
	if len(cpus) != 3 || cpus[0] != 0 || cpus[1] != 1 || cpus[2] != 2 {
		t.Errorf("top three over a column holding NaNs = %v, want the real rows [0 1 2]", cpus)
	}
}

// TestTopKRowsRefusals pins the requests with no answer.
func TestTopKRowsRefusals(t *testing.T) {
	d := topKRowsFixture(t, 2, 4)
	for _, c := range []struct {
		name string
		spec TopKRowsSpec
	}{
		{"k=0", TopKRowsSpec{OrderBy: OrderOne("QDate", true), K: 0}},
		{"negative k", TopKRowsSpec{OrderBy: OrderOne("QDate", true), K: -1}},
		{"no ordering at all", TopKRowsSpec{K: 3}},
		{"empty ordering", TopKRowsSpec{OrderBy: []TopKOrderTerm{}, K: 3}},
		{"no order attribute", TopKRowsSpec{OrderBy: OrderOne("", true), K: 3}},
		{"star order attribute", TopKRowsSpec{OrderBy: OrderOne("*", true), K: 3}},
		{"second term names nothing", TopKRowsSpec{
			OrderBy: []TopKOrderTerm{{Attr: "QDate", Desc: true}, {Attr: ""}}, K: 3}},
		{"star group column", TopKRowsSpec{
			GroupCols: []GroupCol{{Attr: "*"}}, OrderBy: OrderOne("QDate", true), K: 3}},
		{"negative bucket", TopKRowsSpec{
			GroupCols: []GroupCol{{Attr: "QDate", BucketWidth: -5}}, OrderBy: OrderOne("QDate", true), K: 3}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := d.TopKRowsPerGroup("true", c.spec); err == nil {
				t.Fatal("accepted a request with no answer")
			}
		})
	}
}

// TestTopKRowsNeedsEveryOrderingTerm pins that a row is ranked only when EVERY ordering term
// gives it a number, not just the first.
//
// A row whose second term is missing has no place in a total order: it cannot be compared with
// the rows that have one, and ranking it as though the term were zero invents a position. The
// one-term rule already said "absent, non-numeric or NaN is not ranked"; this is that rule
// applied to a list, and it is the case a differential test between the two rungs cannot see,
// because both rungs would be wrong together.
func TestTopKRowsNeedsEveryOrderingTerm(t *testing.T) {
	d, err := OpenConfig(Config{Dir: t.TempDir(), SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tx := d.Begin()
	n := 0
	add := func(text string) {
		ad, perr := classad.ParseOld(text)
		if perr != nil {
			t.Fatalf("fixture %q: %v", text, perr)
		}
		tx.NewClassAd(fmt.Sprintf("k%d", n), ad)
		n++
	}
	// Rankable: both terms numeric. These are the only rows that may come back.
	for i := 0; i < 6; i++ {
		add(fmt.Sprintf("Owner = \"o\"\nQDate = %d\nTie = %d\nPayload = \"ok%d\"", 1700000000+i, i, i))
	}
	// Not rankable, each for a different reason, and each with a PERFECTLY GOOD first term --
	// a newer QDate than any row above, so a rung that ranked on the first term alone would
	// put every one of them ahead of the rows that belong.
	add("Owner = \"o\"\nQDate = 1800000000\nPayload = \"no-second-term\"")
	add("Owner = \"o\"\nQDate = 1800000001\nTie = \"text\"\nPayload = \"second-term-string\"")
	add("Owner = \"o\"\nQDate = 1800000002\nTie = undefined\nPayload = \"second-term-undefined\"")
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}

	spec := TopKRowsSpec{
		GroupCols: []GroupCol{{Attr: "Owner"}},
		OrderBy: []TopKOrderTerm{
			{Attr: "QDate", Desc: true},
			{Attr: "Tie", Desc: true},
		},
		K:     4,
		Attrs: []string{"Payload"},
	}
	check := func(name string, rows []string) {
		t.Helper()
		if len(rows) != 4 {
			t.Fatalf("%s: kept %d rows, want 4", name, len(rows))
		}
		for _, p := range rows {
			if !strings.HasPrefix(p, "ok") {
				t.Errorf("%s: kept %q, which has no usable second ordering term", name, p)
			}
		}
	}

	got, err := d.TopKRowsPerGroup("true", spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("value rung: %d groups, want 1", len(got))
	}
	var vrows []string
	for _, r := range got[0].Rows {
		p, perr := r[0].StringValue()
		if perr != nil {
			t.Fatal(perr)
		}
		vrows = append(vrows, p)
	}
	check("value rung", vrows)

	wgot, err := d.TopKRowsPerGroupWire("true", spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(wgot) != 1 {
		t.Fatalf("wire rung: %d groups, want 1", len(wgot))
	}
	var wrows []string
	for _, r := range decodeWireRows(t, wgot[0].Rows, []string{"Payload"}) {
		wrows = append(wrows, strings.Trim(r, "\""))
	}
	check("wire rung", wrows)
}
