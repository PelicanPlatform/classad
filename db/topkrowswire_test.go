package db

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// THE WIRE RUNG IS TESTED AGAINST THE VALUE RUNG, NOT AGAINST ITS OWN IDEA OF THE ANSWER.
//
// The two rungs decide the same three things by different means, and each means is a place they
// can part company:
//
//	which group a row is in   the reference renders the group value to text; this keys on the
//	                          group columns' NODE BYTES, and two encodings of one value would
//	                          be two groups here and one there
//	whether a row ranks       the reference reads a classad.Value and asks numValueOf; this
//	                          reads the node and asks wire.NumberValue
//	which rows win, in order  shared code (topKWorse, a stable sort), so this one should hold
//	                          by construction -- which is worth confirming rather than assuming
//
// A unit test written against the wire rung alone would pass on all three while disagreeing with
// the reference, and the disagreement is the bug: the same query would answer differently
// depending on which rung the planner picked.

// decodeWireRows renders each wire row's named attributes, so a wire group can be compared with
// a value group cell by cell.
func decodeWireRows(t *testing.T, rows [][]byte, attrs []string) []string {
	t.Helper()
	out := make([]string, len(rows))
	for i, b := range rows {
		a, err := wire.DecodeInline(b)
		if err != nil {
			t.Fatalf("wire row %d did not decode: %v", i, err)
		}
		ad := classad.FromAST(a)
		var sb strings.Builder
		for j, name := range attrs {
			if j > 0 {
				sb.WriteByte('|')
			}
			sb.WriteString(GroupValueText(ad.EvaluateAttr(name)))
		}
		out[i] = sb.String()
	}
	return out
}

func valueRowsText(rows [][]classad.Value) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		parts := make([]string, len(r))
		for j, v := range r {
			parts[j] = GroupValueText(v)
		}
		out[i] = strings.Join(parts, "|")
	}
	return out
}

func groupLabel(vals []classad.Value) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = GroupValueText(v)
	}
	return strings.Join(parts, "\x00")
}

// assertRungsAgree runs both rungs over the same table and spec and requires the same groups with
// the same rows in the same order.
func assertRungsAgree(t *testing.T, d *DB, constraint string, spec TopKRowsSpec) {
	t.Helper()
	want, err := d.TopKRowsPerGroup(constraint, spec)
	if err != nil {
		t.Fatalf("value rung: %v", err)
	}
	got, err := d.TopKRowsPerGroupWire(constraint, spec)
	if err != nil {
		t.Fatalf("wire rung: %v", err)
	}
	if len(got) != len(want) {
		gl, wl := []string{}, []string{}
		for _, g := range got {
			gl = append(gl, groupLabel(g.Values))
		}
		for _, g := range want {
			wl = append(wl, groupLabel(g.Values))
		}
		sort.Strings(gl)
		sort.Strings(wl)
		t.Fatalf("groups: wire %d, value %d\nwire  %q\nvalue %q", len(got), len(want), gl, wl)
	}
	wantAt := map[string][]string{}
	for _, g := range want {
		wantAt[groupLabel(g.Values)] = valueRowsText(g.Rows)
	}
	for _, g := range got {
		label := groupLabel(g.Values)
		w, ok := wantAt[label]
		if !ok {
			t.Fatalf("wire rung invented group %q; value rung has %v", label, keysOf(wantAt))
		}
		// The wire row carries the group and order attributes too, so compare only the
		// attributes the caller asked for -- which is what the reference's rows hold.
		gotRows := decodeWireRows(t, g.Rows, spec.Attrs)
		if len(gotRows) != len(w) {
			t.Fatalf("group %q: wire kept %d rows, value kept %d", label, len(gotRows), len(w))
		}
		for i := range w {
			if gotRows[i] != w[i] {
				t.Fatalf("group %q row %d: wire %q, value %q\nwire %v\nvalue %v",
					label, i, gotRows[i], w[i], gotRows, w)
			}
		}
		delete(wantAt, label)
	}
	if len(wantAt) != 0 {
		t.Fatalf("wire rung missed groups %v", keysOf(wantAt))
	}
}

func keysOf(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestTopKWireAgreesWithValueRung is the differential test, over shapes chosen for the three
// divergence sites above rather than for coverage.
func TestTopKWireAgreesWithValueRung(t *testing.T) {
	d, err := OpenConfig(Config{Dir: t.TempDir(), SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	rng := rand.New(rand.NewSource(20261116))
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
	for i := 0; i < 2000; i++ {
		owner := fmt.Sprintf("o%d", rng.Intn(12))
		// TIES on the order attribute, deliberately: a small QDate space means most groups
		// hold several rows with the same rank, which is where a sort that is not stable, or
		// a comparison that is not shared, shows up.
		add(fmt.Sprintf("Owner = \"%s\"\nSite = %d\nQDate = %d\nPayload = \"p%d\"",
			owner, rng.Intn(4), 1700000000+rng.Intn(40), i))
	}
	// A group column stored as an INTEGER in some ads and a REAL in others. The reference
	// keeps 2 and 2.0 apart (they are different ClassAd values); byte keying must too.
	for i := 0; i < 60; i++ {
		add(fmt.Sprintf("Owner = \"mixed\"\nSite = %d.0\nQDate = %d\nPayload = \"r%d\"",
			i%4, 1700000000+i, i))
	}
	// Rows with nothing to rank by: a missing order attribute, a string one, and a NaN. All
	// three leave the answer on both rungs, and they must leave it the same way.
	for i := 0; i < 30; i++ {
		add(fmt.Sprintf("Owner = \"noorder\"\nSite = 1\nPayload = \"m%d\"", i))
		add(fmt.Sprintf("Owner = \"noorder\"\nSite = 1\nQDate = \"notanumber\"\nPayload = \"s%d\"", i))
	}
	// A row whose group column is absent: in no group, on both rungs.
	for i := 0; i < 20; i++ {
		add(fmt.Sprintf("Site = 2\nQDate = %d\nPayload = \"g%d\"", 1700000000+i, i))
	}
	// Two NUMERIC group columns whose renderings concatenate to the same bytes: (1,23) and
	// (12,3) both spell "123". A key that is not length-delimited folds these two groups into
	// one, and nothing else in this fixture can show it -- a string renders quoted, which is
	// self-delimiting, so only numbers collide.
	for i := 0; i < 24; i++ {
		add(fmt.Sprintf("Owner = \"pair\"\nA = 1\nB = 23\nQDate = %d\nPayload = \"a%d\"",
			1700000000+i, i))
		add(fmt.Sprintf("Owner = \"pair\"\nA = 12\nB = 3\nQDate = %d\nPayload = \"b%d\"",
			1700000000+i, i))
	}
	// An EXPRESSION in the order attribute and in a group column.
	for i := 0; i < 20; i++ {
		add(fmt.Sprintf("Owner = \"expr\"\nSite = 1\nBase = %d\nQDate = Base + 1\nPayload = \"e%d\"", i, i))
	}
	// A BOOLEAN order attribute. numValueOf ranks booleans (false < true), so the reference
	// does, and a wire rung that read only ints and reals would drop this whole group while
	// every other case still agreed.
	for i := 0; i < 20; i++ {
		add(fmt.Sprintf("Owner = \"bool\"\nSite = 1\nQDate = %t\nPayload = \"t%d\"", i%2 == 0, i))
	}
	// NaN IN THE ORDER COLUMN, INSERTED LAST SO IT ARRIVES FIRST. The scan yields newest
	// first. Written the other way round the NaNs arrive last, a full keep of real values
	// rejects them without needing the guard, and the case passes while testing nothing.
	for i := 0; i < 6; i++ {
		ad := classad.New()
		ad.InsertAttrString("Owner", "nan")
		ad.InsertAttr("Site", int64(1))
		ad.InsertAttrFloat("QDate", float64(1700000000+i))
		ad.InsertAttrString("Payload", fmt.Sprintf("real%d", i))
		tx.NewClassAd(fmt.Sprintf("k%d", n), ad)
		n++
	}
	for i := 0; i < 6; i++ {
		ad := classad.New()
		ad.InsertAttrString("Owner", "nan")
		ad.InsertAttr("Site", int64(1))
		ad.InsertAttrFloat("QDate", math.NaN())
		ad.InsertAttrString("Payload", fmt.Sprintf("nan%d", i))
		tx.NewClassAd(fmt.Sprintf("k%d", n), ad)
		n++
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	d.EnableSchemaScan(1000, 8)

	for _, tc := range []struct {
		name string
		cons string
		spec TopKRowsSpec
	}{
		{"one string group", "true", TopKRowsSpec{
			GroupCols: []GroupCol{{Attr: "Owner"}}, OrderAttr: "QDate", Desc: true, K: 5,
			Attrs: []string{"Payload", "QDate"}}},
		{"ascending", "true", TopKRowsSpec{
			GroupCols: []GroupCol{{Attr: "Owner"}}, OrderAttr: "QDate", Desc: false, K: 3,
			Attrs: []string{"Payload", "QDate"}}},
		{"k larger than any group", "true", TopKRowsSpec{
			GroupCols: []GroupCol{{Attr: "Owner"}}, OrderAttr: "QDate", Desc: true, K: 10000,
			Attrs: []string{"Payload"}}},
		{"k of one", "true", TopKRowsSpec{
			GroupCols: []GroupCol{{Attr: "Owner"}}, OrderAttr: "QDate", Desc: true, K: 1,
			Attrs: []string{"Payload", "QDate"}}},
		{"numeric group column", "true", TopKRowsSpec{
			GroupCols: []GroupCol{{Attr: "Site"}}, OrderAttr: "QDate", Desc: true, K: 4,
			Attrs: []string{"Payload"}}},
		{"two numeric group columns", "true", TopKRowsSpec{
			GroupCols: []GroupCol{{Attr: "A"}, {Attr: "B"}}, OrderAttr: "QDate", Desc: true, K: 3,
			Attrs: []string{"Payload"}}},
		{"two group columns", "true", TopKRowsSpec{
			GroupCols: []GroupCol{{Attr: "Owner"}, {Attr: "Site"}}, OrderAttr: "QDate", Desc: true, K: 3,
			Attrs: []string{"Payload", "QDate"}}},
		{"no group columns", "true", TopKRowsSpec{
			OrderAttr: "QDate", Desc: true, K: 7, Attrs: []string{"Payload"}}},
		{"constrained", "Site == 1", TopKRowsSpec{
			GroupCols: []GroupCol{{Attr: "Owner"}}, OrderAttr: "QDate", Desc: true, K: 4,
			Attrs: []string{"Payload", "QDate"}}},
		{"order attribute not projected", "true", TopKRowsSpec{
			GroupCols: []GroupCol{{Attr: "Owner"}}, OrderAttr: "QDate", Desc: true, K: 3,
			Attrs: []string{"Payload"}}},
		{"group column also projected", "true", TopKRowsSpec{
			GroupCols: []GroupCol{{Attr: "Owner"}}, OrderAttr: "QDate", Desc: true, K: 3,
			Attrs: []string{"Owner", "Payload"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertRungsAgree(t, d, tc.cons, tc.spec)
		})
	}
}

// TestTopKWireRejectsBucketing pins the one spec the wire rung refuses rather than answers.
// Rounding is what makes two different nodes one group, and byte keying cannot do it; a rung
// that silently grouped per-node instead would return a different answer for the same query.
func TestTopKWireRejectsBucketing(t *testing.T) {
	d, err := OpenConfig(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	spec := TopKRowsSpec{
		GroupCols: []GroupCol{{Attr: "QDate", BucketWidth: 3600}},
		OrderAttr: "QDate", Desc: true, K: 2, Attrs: []string{"Payload"},
	}
	if _, err := d.TopKRowsPerGroupWire("true", spec); err == nil {
		t.Fatal("wire rung accepted a bucketed group column")
	}
	// The reference serves it, which is what the refusal points callers at.
	if _, err := d.TopKRowsPerGroup("true", spec); err != nil {
		t.Fatalf("value rung refused a bucketed group column too: %v", err)
	}
}

// TestTopKWireSpecValidationMatches pins that both rungs refuse the same specs: a spec one
// accepts and the other rejects is a planner that cannot swap rungs freely.
func TestTopKWireSpecValidationMatches(t *testing.T) {
	d, err := OpenConfig(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, spec := range []TopKRowsSpec{
		{OrderAttr: "QDate", K: 0},
		{OrderAttr: "QDate", K: -1},
		{OrderAttr: "", K: 3},
		{OrderAttr: "*", K: 3},
		{OrderAttr: "QDate", K: 3, GroupCols: []GroupCol{{Attr: ""}}},
		{OrderAttr: "QDate", K: 3, GroupCols: []GroupCol{{Attr: "*"}}},
		{OrderAttr: "QDate", K: 3, GroupCols: []GroupCol{{Attr: "Owner", BucketWidth: -1}}},
	} {
		_, verr := d.TopKRowsPerGroup("true", spec)
		_, werr := d.TopKRowsPerGroupWire("true", spec)
		if (verr == nil) != (werr == nil) {
			t.Fatalf("spec %+v: value rung err=%v, wire rung err=%v", spec, verr, werr)
		}
	}
}

// TestTopKWireRowsAreOwned pins the copy. ScanRawWire hands out a buffer it reuses, so a keep
// that retained the slice would come back holding whatever the scan last read -- and on a small
// fixture, where the buffer may not be reused before the scan ends, that bug passes.
func TestTopKWireRowsAreOwned(t *testing.T) {
	d, err := OpenConfig(Config{Dir: t.TempDir(), SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tx := d.Begin()
	for i := 0; i < 5000; i++ {
		ad, perr := classad.ParseOld(fmt.Sprintf(
			"Owner = \"o%d\"\nQDate = %d\nPayload = \"payload-%d\"", i%5, 1700000000+i, i))
		if perr != nil {
			t.Fatal(perr)
		}
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := d.TopKRowsPerGroupWire("true", TopKRowsSpec{
		GroupCols: []GroupCol{{Attr: "Owner"}}, OrderAttr: "QDate", Desc: true, K: 3,
		Attrs: []string{"Payload", "QDate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("groups = %d, want 5", len(got))
	}
	// Every kept row must still be the row that was kept: its QDate is one of the three
	// largest in its group, and its Payload is that row's.
	for _, g := range got {
		owner, verr := g.Values[0].StringValue()
		if verr != nil {
			t.Fatalf("group value: %v", verr)
		}
		rows := decodeWireRows(t, g.Rows, []string{"Payload", "QDate"})
		if len(rows) != 3 {
			t.Fatalf("group %s kept %d rows, want 3", owner, len(rows))
		}
		for _, r := range rows {
			parts := strings.Split(r, "|")
			var idx int
			var payload string
			// GroupValueText quotes a string, which is what keeps "4" out of 4's group.
			if _, serr := fmt.Sscanf(parts[0], "%q", &payload); serr != nil {
				t.Fatalf("group %s: row %q does not hold a quoted payload", owner, r)
			}
			if _, serr := fmt.Sscanf(payload, "payload-%d", &idx); serr != nil {
				t.Fatalf("group %s: row %q is not a payload this fixture wrote", owner, r)
			}
			if want := fmt.Sprint(1700000000 + idx); parts[1] != want {
				t.Fatalf("group %s: row %q pairs payload-%d with QDate %s, want %s -- the "+
					"kept slice was overwritten by a later record", owner, r, idx, parts[1], want)
			}
			if fmt.Sprintf("o%d", idx%5) != owner {
				t.Fatalf("group %s holds payload-%d, which belongs to o%d", owner, idx, idx%5)
			}
		}
	}
}
