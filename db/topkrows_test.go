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

// TestTopKRowsRanksEveryRowWhateverTheOrderingHolds covers the rows whose ordering attribute is
// not a number. They are RANKED, not dropped: a string orders among strings and a missing
// attribute orders as undefined, by collections.CompareValues.
//
// That matters because the class order puts numbers first, so under DESC -- the spelling of "the
// newest five" -- the rows with no usable date come back AHEAD of the dated ones. It looks wrong
// and it is what a client sorting the same rows itself produces, which is the whole point of the
// pushdown: it answers the question the client code it replaces answers. Dropping those rows
// instead would quietly return a different set and look fast doing it.
func TestTopKRowsRanksEveryRowWhateverTheOrderingHolds(t *testing.T) {
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
	// Descending reverses the total order, so the classes come back undefined, then string,
	// then number. Five of six rows survive, and the one evicted is the SMALLEST number -- not
	// one of the rows that lack a date, which is what dropping the unrankable would have done.
	//
	// Rows tied within a class are compared by position rather than by identity: the ordering
	// cannot separate two undefineds, so which of them comes first is arbitrary and asserting
	// it would pin an accident.
	if len(cpus) != 5 {
		t.Fatalf("ranked %d rows, want 5: %v", len(cpus), cpus)
	}
	classOf := map[int]string{2: "undefined", 5: "undefined", 1: "string", 4: "string", 3: "number"}
	wantClasses := []string{"undefined", "undefined", "string", "string", "number"}
	for i, c := range cpus {
		if classOf[c] != wantClasses[i] {
			t.Fatalf("ranked rows = %v, want the classes %v (undefined, then string, then number)",
				cpus, wantClasses)
		}
	}
	for _, c := range cpus {
		if c == 0 {
			t.Errorf("kept the smallest-dated row (Cpus=0) at k=5; it is the one that should be evicted")
		}
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

// TestTopKRowsRanksBooleansLikeTheColumnPass is the divergence that the two conversions hid: a
// hand-rolled comparison that skipped booleans ranked a boolean order column differently
// depending on which rung answered. Both rungs now order through collections.CompareValues,
// which places false below true, and this pins it for the row rung.
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

// TestTopKRowsRanksNaNLastInTheOrder pins where a NaN lands. It is last among numbers in the
// total order -- rather than comparing false in both directions, which would leave it anywhere a
// sort happened to put it and is not an order at all -- so DESC brings it back first.
//
// This is the one place the pushdown and a client sorting the same rows genuinely differ: the
// client's comparison reports a NaN neither above nor below anything, so where it lands there
// depends on the input order. Being deterministic is worth more than matching something that is
// itself undetermined.
func TestTopKRowsRanksNaNLastInTheOrder(t *testing.T) {
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
	if len(cpus) != 3 || cpus[0] != 3 || cpus[1] != 4 || cpus[2] != 5 {
		t.Errorf("top three descending over a column holding NaNs = %v, want the NaN rows [3 4 5]", cpus)
	}

	// ASCENDING asks for the other end, and the NaNs must leave it: last in the order means
	// last under ASC. A NaN that came back at both ends would mean it was never ordered at all.
	asc, err := d.TopKRowsPerGroup("true", TopKRowsSpec{
		GroupCols: []GroupCol{{Attr: "Owner"}},
		OrderBy:   OrderOne("QDate", false), K: 3, Attrs: []string{"Cpus"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var acpus []int
	for _, row := range asc[0].Rows {
		c, _ := row[0].IntValue() //nolint:errcheck // the fixture writes integers
		acpus = append(acpus, int(c))
	}
	sort.Ints(acpus)
	if len(acpus) != 3 || acpus[0] != 0 || acpus[1] != 1 || acpus[2] != 2 {
		t.Errorf("bottom three over a column holding NaNs = %v, want the real rows [0 1 2]", acpus)
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

// TestTopKRowsConsultsEveryOrderingTerm pins that a later ordering term decides the rows the
// earlier ones tie on -- and that it does so across types, now that a term no longer has to be
// numeric for the row to rank at all.
//
// The fixture ties the FIRST term deliberately. With distinct first terms a rung that ignored
// everything after the first would still pass, because the first term alone would pick the same
// rows; a tie is the only shape that can tell the two apart. The older rows carry the largest
// second term in the fixture, so a rung that weighed the terms in the wrong order would pull
// them in.
func TestTopKRowsConsultsEveryOrderingTerm(t *testing.T) {
	d, err := OpenConfig(Config{SegmentSize: 1 << 16})
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
	// Four rows tied on QDate, separated only by Tie -- which spans the classes, so the
	// expected order is the class order: undefined, then string, then the numbers descending.
	add("Owner = \"o\"\nQDate = 1700000000\nTie = 1\nPayload = \"t1\"")
	add("Owner = \"o\"\nQDate = 1700000000\nTie = 3\nPayload = \"t3\"")
	add("Owner = \"o\"\nQDate = 1700000000\nTie = \"text\"\nPayload = \"tstr\"")
	add("Owner = \"o\"\nQDate = 1700000000\nTie = undefined\nPayload = \"tundef\"")
	// Older, and so out of the top four however large their second term is.
	add("Owner = \"o\"\nQDate = 1699999999\nTie = 99\nPayload = \"older-a\"")
	add("Owner = \"o\"\nQDate = 1699999999\nTie = 98\nPayload = \"older-b\"")
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
	want := []string{"tundef", "tstr", "t3", "t1"}
	check := func(name string, rows []string) {
		t.Helper()
		if len(rows) != len(want) {
			t.Fatalf("%s: kept %d rows, want %d: %v", name, len(rows), len(want), rows)
		}
		for i := range want {
			if rows[i] != want[i] {
				t.Errorf("%s: rows = %v, want %v (QDate ties, so Tie decides: undefined, "+
					"string, then numbers descending)", name, rows, want)
				return
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
