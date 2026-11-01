package vm

import (
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// Hiding must evaluate, on an ad, exactly as the original query does on the same ad with every hidden
// attribute's value replaced by undefined -- which is what a reader with no key sees of a sealed value.
func TestHidingEvaluatesAsWithheld(t *testing.T) {
	hide := func(n string) bool { return strings.EqualFold(n, "Secret") || strings.EqualFold(n, "Pin") }
	src := `[ Secret = "s3cr3t"; Pin = 1234; Owner = "alice"; N = 7; Ref = Secret; R = [ Secret = 5 ] ]`
	withheldSrc := `[ Secret = undefined; Pin = undefined; Owner = "alice"; N = 7; Ref = Secret; R = [ Secret = 5 ] ]`
	ad, err := classad.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	withheld, err := classad.Parse(withheldSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, expr := range []string{
		`Secret == "s3cr3t"`,
		`Secret is undefined`,
		`Secret isnt undefined`,
		`MY.Secret == "s3cr3t"`,
		`TARGET.Secret is undefined`,
		`.Pin > 1000`,
		`Pin > 1000 || Owner == "alice"`,
		`Pin > 1000 && Owner == "alice"`,
		`!(Pin <= 1000)`,
		`ifThenElse(Pin > 1000, true, false)`,
		`isUndefined(Secret)`,
		`regexp("^s3", Secret)`,
		`size(Secret) > 0`,
		`strcat(Owner, Secret) == "alice"`,
		`Pin ?: 5`,
		`{Pin, N}[0] is undefined`,
		`[ Secret = 5; X = Secret ].X == 5`,
		`[ Y = Secret ].Y is undefined`,
		`R.Secret == 5`,
		`member(Pin, {1234, 1})`,
		`Owner == "alice"`,
		`N + 1 == 8`,
	} {
		q, err := Parse(expr)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		h := q.Hiding(hide)
		got, want := h.Eval(ad), q.Eval(withheld)
		if got.String() != want.String() {
			t.Errorf("%s: hiding gives %s on the ad, the query gives %s on the withheld ad", expr, got.String(), want.String())
		}
		// A record literal's own Secret is the record's, and stays; the read set names it either way.
		if strings.Contains(expr, "[ Secret") {
			continue
		}
		for _, a := range h.ReadAttrs() {
			if hide(a) {
				t.Errorf("%s: the hidden query still reads %s", expr, a)
			}
		}
	}
}

// A query that reads nothing hidden is returned as it is, so a store pays nothing for a reader whose
// query does not touch a sealed attribute.
func TestHidingUnchangedIsSame(t *testing.T) {
	q, err := Parse(`Owner == "alice" && N > 3`)
	if err != nil {
		t.Fatal(err)
	}
	if q.Hiding(func(n string) bool { return n == "Secret" }) != q {
		t.Fatal("Hiding rebuilt a query that references nothing hidden")
	}
	var nilq *Query
	if nilq.Hiding(func(string) bool { return true }) != nil {
		t.Fatal("Hiding of a nil query is not nil")
	}
}

// The rewritten query keeps the original's pinned CurrentTime, so a constraint planned for a keyless
// reader does not move its window.
func TestHidingKeepsPinnedTime(t *testing.T) {
	e, err := parser.ParseExpr(`CurrentTime - QDate < 3600 && Secret == "x"`)
	if err != nil {
		t.Fatal(err)
	}
	q := compileAt(e, 12345) // a time no clock reads now
	h := q.Hiding(func(n string) bool { return n == "Secret" })
	if h == q {
		t.Fatal("nothing hidden")
	}
	if h.prog.now != q.prog.now || h.ReadsCurrentTime() != q.ReadsCurrentTime() {
		t.Fatalf("pinned time %d (%v) became %d (%v)", q.prog.now, q.ReadsCurrentTime(), h.prog.now, h.ReadsCurrentTime())
	}
}
