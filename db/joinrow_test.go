package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// THE ORACLE FOR CARRYING ROWS AS BYTES.
//
// Reading a row off its wire bytes instead of off a decoded ad is only safe if the two
// answer identically, and the ways they could differ are all silent: an attribute the byte
// lookup misses reads undefined rather than failing, so the condition stops holding and the
// join returns too few rows; a hash key read one way and the other way puts a pair in two
// buckets and the join returns none; an output member built from the bytes could carry an
// attribute's stored form where the ad carried its value.
//
// None of those is an error anywhere. So the test is the whole join, run both ways over the
// same fixture, compared row for row -- which is what forceDecodedRows exists for.

// withDecodedRows runs fn with every row decoded as it is read, which is what the join did
// before it carried bytes.
func withDecodedRows(t *testing.T, fn func()) {
	t.Helper()
	forceDecodedRows = true
	defer func() { forceDecodedRows = false }()
	fn()
}

// rowFormFixture is readJoinFixture with an EXPRESSION-valued attribute on each side.
//
// It has its own fixture because that attribute is the point: a stored expression is the
// case the bytes cannot answer, so a fixture of nothing but literals would take the fast
// path everywhere and never compare the two against each other where they differ.
func rowFormFixture(t *testing.T) *Catalog {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	hist, err := cat.CreateTable("history")
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := cat.CreateTable("epoch_history")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 24; i++ {
		ad, perr := classad.Parse(fmt.Sprintf(
			`[ ClusterId = %d; ProcId = 0; Owner = "user%02d"; JobStatus = 4; `+
				`RequestMemory = %d; Doubled = ClusterId * 2; Filler = "x" ]`,
			i, i%3, 1024+(i%4)*512))
		if perr != nil {
			t.Fatal(perr)
		}
		if werr := hist.Put(fmt.Sprintf("h%d", i), ad); werr != nil {
			t.Fatal(werr)
		}
		for e := 0; e < 3; e++ {
			ead, perr := classad.Parse(fmt.Sprintf(
				`[ ClusterId = %d; ProcId = 0; NumShadowStarts = %d; RequestMemory = %d; `+
					`MemoryUsage = %d; Headroom = RequestMemory - MemoryUsage; Filler = "y" ]`,
				i, e+1, 1024+(i%4)*512+e*256, 900+e*200))
			if perr != nil {
				t.Fatal(perr)
			}
			if werr := epoch.Put(fmt.Sprintf("e%d.%d", i, e), ead); werr != nil {
				t.Fatal(werr)
			}
		}
	}
	return cat
}

// TestWireRowsAnswerAsDecodedRowsDo is the oracle: every join shape, both ways, same rows.
func TestWireRowsAnswerAsDecodedRowsDo(t *testing.T) {
	cat := rowFormFixture(t)
	for _, tc := range rowFormSpecs() {
		t.Run(tc.name, func(t *testing.T) {
			var decoded []string
			withDecodedRows(t, func() { decoded = joinRowTexts(t, cat, tc.spec) })
			fromWire := joinRowTexts(t, cat, tc.spec)
			if len(decoded) != len(fromWire) {
				t.Fatalf("decoded rows %d, rows read off the wire %d", len(decoded), len(fromWire))
			}
			if len(decoded) == 0 {
				t.Fatal("the shape produced no rows, so it compares nothing")
			}
			for i := range decoded {
				if decoded[i] != fromWire[i] {
					t.Fatalf("row %d differs:\n decoded: %s\nfrom wire: %s", i, decoded[i], fromWire[i])
				}
			}
		})
	}
}

// rowFormSpecs are the shapes the two row forms have to agree on. Each one is here because
// it reaches a different branch of what the bytes can and cannot answer.
func rowFormSpecs() []struct {
	name string
	spec JoinSpec
} {
	type tc = struct {
		name string
		spec JoinSpec
	}
	return []tc{
		{"a plain equi join", JoinSpec{
			Kind:  JoinInner,
			Left:  JoinSide{Table: "history", Alias: "h", Project: []string{"ClusterId", "Owner"}},
			Right: JoinSide{Table: "epoch_history", Alias: "e", Project: []string{"NumShadowStarts"}},
			Cond:  []string{"h.ClusterId == e.ClusterId"},
			Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
		}},
		{"a LEFT join, so some rows have no right side", JoinSpec{
			Kind:  JoinLeft,
			Left:  JoinSide{Table: "history", Alias: "h", Project: []string{"ClusterId", "Owner"}},
			Right: JoinSide{Table: "epoch_history", Alias: "e", Project: []string{"NumShadowStarts"}},
			Cond:  []string{"h.ClusterId == e.ClusterId && e.NumShadowStarts > 1"},
			Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
		}},
		{"a WHERE on an attribute no output carries", JoinSpec{
			Kind:  JoinInner,
			Left:  JoinSide{Table: "history", Alias: "h", Project: []string{"ClusterId"}},
			Right: JoinSide{Table: "epoch_history", Alias: "e", Project: []string{"NumShadowStarts"}},
			Cond:  []string{"h.ClusterId == e.ClusterId"},
			Post:  []string{"h.Owner == \"user01\""},
			Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
		}},
		{"an output carrying an expression-valued attribute", JoinSpec{
			Kind:  JoinInner,
			Left:  JoinSide{Table: "history", Alias: "h", Project: []string{"ClusterId", "Doubled"}},
			Right: JoinSide{Table: "epoch_history", Alias: "e", Project: []string{"NumShadowStarts", "Headroom"}},
			Cond:  []string{"h.ClusterId == e.ClusterId"},
			Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
		}},
		{"a condition reading an expression-valued attribute", JoinSpec{
			Kind:  JoinInner,
			Left:  JoinSide{Table: "history", Alias: "h", Project: []string{"ClusterId"}},
			Right: JoinSide{Table: "epoch_history", Alias: "e", Project: []string{"NumShadowStarts"}},
			Cond:  []string{"h.ClusterId == e.ClusterId && h.Doubled > 4 && e.Headroom > 0"},
			Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
		}},
		{"an equality half that is not a bare name", JoinSpec{
			Kind:  JoinInner,
			Left:  JoinSide{Table: "history", Alias: "h", Project: []string{"ClusterId", "Owner"}},
			Right: JoinSide{Table: "epoch_history", Alias: "e", Project: []string{"NumShadowStarts"}},
			Cond:  []string{"h.ClusterId + 0 == e.ClusterId"},
			Equi:  []JoinEqui{{Left: "ClusterId + 0", Right: "ClusterId"}},
		}},
		{"no equality at all, so every pair is examined", JoinSpec{
			Kind:  JoinInner,
			Left:  JoinSide{Table: "history", Alias: "h", Project: []string{"ClusterId", "Owner"}, Constraint: "ClusterId < 4"},
			Right: JoinSide{Table: "epoch_history", Alias: "e", Project: []string{"NumShadowStarts"}, Constraint: "ClusterId < 4"},
			Cond:  []string{"h.ClusterId == e.ClusterId"},
		}},
		{"a whole-ad output, which is not narrowed at all", JoinSpec{
			Kind:  JoinInner,
			Left:  JoinSide{Table: "history", Alias: "h", Constraint: "ClusterId < 4"},
			Right: JoinSide{Table: "epoch_history", Alias: "e", Constraint: "ClusterId < 4"},
			Cond:  []string{"h.ClusterId == e.ClusterId"},
			Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
		}},
		{"a term the alias compiler declines, so the composite runs", JoinSpec{
			Kind:  JoinInner,
			Left:  JoinSide{Table: "history", Alias: "h", Project: []string{"ClusterId", "Owner"}},
			Right: JoinSide{Table: "epoch_history", Alias: "e", Project: []string{"NumShadowStarts"}},
			Cond:  []string{`h.ClusterId == e.ClusterId && strcat(h.Owner, "!") == "user01!"`},
			Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
		}},
	}
}

// TestWireRowReadsALiteralWithoutDecoding pins the thing the win rests on: the ordinary case
// is answered from the bytes, and no ad is built.
//
// Without it, every branch here could take its fallback and the oracle above would still
// pass -- the fallback is the old behaviour, so a version of this that never reads a byte is
// CORRECT and pointless. This is what separates the two.
func TestWireRowReadsALiteralWithoutDecoding(t *testing.T) {
	cat := rowFormFixture(t)
	d, ok := cat.Table("history")
	if !ok {
		t.Fatal("no history table")
	}
	rows, err := d.QueryRawWireRefs("ClusterId == 1", []string{"ClusterId", "Owner", "Doubled"}, false)
	if err != nil {
		t.Fatal(err)
	}
	var r *joinRow
	for row := range wireRows(rows) {
		r = row
		break
	}
	if r == nil {
		t.Fatal("the fixture produced no row")
	}
	if _, fromWire := r.attr("ClusterId"); !fromWire {
		t.Error("ClusterId is an integer literal and should be read off the bytes")
	}
	if _, fromWire := r.attr("Owner"); !fromWire {
		t.Error("Owner is a string literal and should be read off the bytes")
	}
	if _, fromWire := r.attr("NoSuchAttribute"); !fromWire {
		t.Error("an absent attribute is undefined, which the bytes answer without decoding")
	}
	if r.ad != nil {
		t.Fatal("reading literals decoded the row; the whole point is that it does not")
	}
	// An expression-valued attribute is the case the bytes cannot answer.
	if _, fromWire := r.attr("Doubled"); fromWire {
		t.Error("Doubled holds an expression; the bytes must not claim to answer it")
	}
	if r.ad != nil {
		t.Fatal("attr decoded on its own; the decision to decode belongs to the caller")
	}
	if v := r.eval("Doubled"); !v.IsInteger() {
		t.Errorf("eval should have decoded and evaluated Doubled, got %v", v)
	}
	if r.ad == nil {
		t.Fatal("eval did not decode the row for a term the bytes could not answer")
	}
}

// TestWireRowMemberCarriesStoredFormNotValue pins that narrowing from the bytes keeps an
// attribute's stored form, as a projection off a decoded ad does. An expression that arrived
// as an expression and left as its value would be a different answer, and a quiet one.
func TestWireRowMemberCarriesStoredFormNotValue(t *testing.T) {
	cat := rowFormFixture(t)
	d, ok := cat.Table("history")
	if !ok {
		t.Fatal("no history table")
	}
	rows, err := d.QueryRawWireRefs("ClusterId == 1", []string{"ClusterId", "Doubled"}, false)
	if err != nil {
		t.Fatal(err)
	}
	var r *joinRow
	for row := range wireRows(rows) {
		r = row
		break
	}
	if r == nil {
		t.Fatal("the fixture produced no row")
	}
	got := r.member([]string{"ClusterId", "Doubled"}).String()
	if !strings.Contains(got, "ClusterId * 2") {
		t.Errorf("member flattened the expression to a value: %s", got)
	}
	// And it is the same text the decoded path produces.
	want := project(r.record(), []string{"ClusterId", "Doubled"}).String()
	if got != want {
		t.Errorf("member from bytes %s, from the decoded ad %s", got, want)
	}
}

// TestBareAttrNameAdmitsOnlyABareName pins which equality halves take the byte path. A half
// that reads MORE than one attribute must not be answered from that one attribute, and the
// failure would be a hash key built from the wrong value -- pairs in the wrong buckets, and a
// join that silently returns too few rows.
func TestBareAttrNameAdmitsOnlyABareName(t *testing.T) {
	for _, tc := range []struct{ src, want string }{
		{"ClusterId", "ClusterId"},
		{"(ClusterId)", "ClusterId"},
		{"MY.ClusterId", "ClusterId"},
		{"TARGET.ClusterId", ""},
		{"ClusterId + 0", ""},
		{"CurrentTime", ""},
		{`strcat(Owner, "x")`, ""},
		{"Nested.ClusterId", ""},
		{"", ""},
		{"((", ""},
	} {
		if got := bareAttrName(tc.src); got != tc.want {
			t.Errorf("bareAttrName(%q) = %q, want %q", tc.src, got, tc.want)
		}
	}
}

// TestNilRowReadsUndefined pins the absent right side of an unmatched LEFT row.
func TestNilRowReadsUndefined(t *testing.T) {
	var r *joinRow
	v, ok := r.attr("Anything")
	if !ok || !v.IsUndefined() {
		t.Errorf("an absent side should read undefined without decoding, got %v (ok=%v)", v, ok)
	}
	if r.record() != nil {
		t.Error("an absent side has no record")
	}
	if r.member([]string{"Anything"}) != nil {
		t.Error("an absent side contributes no member")
	}
}

// TestDecodedRowReadsThroughTheAd pins the other arm of attr: a row that arrived decoded --
// which is every row of a side read whole -- is read through its ad, not through bytes it
// does not have.
func TestDecodedRowReadsThroughTheAd(t *testing.T) {
	ad, err := classad.Parse(`[ ClusterId = 7; Doubled = ClusterId * 2 ]`)
	if err != nil {
		t.Fatal(err)
	}
	r := &joinRow{ad: ad}
	v, ok := r.attr("Doubled")
	if !ok {
		t.Fatal("a decoded row has no bytes to decline on")
	}
	n, verr := v.IntValue()
	if verr != nil || n != 14 {
		t.Errorf("Doubled = %v (%v), want 14: a decoded row evaluates its expressions", v, verr)
	}
}
