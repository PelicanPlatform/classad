package vm

import (
	"strings"
	"testing"
)

// WHAT A SELECT YIELDS AS A PROBE TARGET.
//
// These live in vm because that is where the shape is decided: collections only ever sees
// the Probe that comes out. The cases below are the ones where a select must NOT become a
// path -- a scoped root, an alias root, a chain rooted in something that is not a reference
// -- because each of them would index a value the query is not asking about.
func TestNestedProbeTargets(t *testing.T) {
	for _, tc := range []struct {
		expr string
		want string // the path, dot-joined; "" means no probe at all
	}{
		{`Res.Mem >= 4096`, "Res.Mem"},
		{`Res.Deep.X == 1`, "Res.Deep.X"},
		{`MY.Res.Mem >= 4096`, "Res.Mem"},
		{`Memory >= 4096`, ""},      // flat: a target, but not a nested one
		{`TARGET.Res.Mem == 1`, ""}, // the other ad in a match, not this one
		{`Res[0].Mem == 1`, ""},     // a subscript is not a select chain
		{`f(x).Mem == 1`, ""},       // rooted in a call
		{`(Res).Mem >= 4096`, "Res.Mem"},
	} {
		q, err := Parse(tc.expr)
		if err != nil {
			t.Fatalf("%s: %v", tc.expr, err)
		}
		var got string
		for _, p := range q.Probes() {
			if p.Nested() {
				got = strings.Join(p.Path, ".")
			}
		}
		if got != tc.want {
			t.Errorf("%s: nested probe %q, want %q", tc.expr, got, tc.want)
		}
	}
}

// Nested() is the one question every consumer asks of a Probe, and the flat case has to
// answer false -- a flat probe reaching a path index would read the wrong column.
func TestProbeNested(t *testing.T) {
	flat, err := Parse(`Memory >= 4096`)
	if err != nil {
		t.Fatal(err)
	}
	ps := flat.Probes()
	if len(ps) != 1 {
		t.Fatalf("a flat comparison yielded %d probes, want 1", len(ps))
	}
	if ps[0].Nested() {
		t.Errorf("%q reports itself nested with path %v", ps[0].Attr, ps[0].Path)
	}
	nested, err := Parse(`Res.Mem >= 4096`)
	if err != nil {
		t.Fatal(err)
	}
	ps = nested.Probes()
	if len(ps) != 1 {
		t.Fatalf("a nested comparison yielded %d probes, want 1", len(ps))
	}
	if !ps[0].Nested() {
		t.Errorf("%q does not report itself nested", ps[0].Attr)
	}
}

// An alias-qualified select is a join's reference to another side, not a path into this
// ad's own nested record. Indexing it would answer with the wrong table's values.
func TestNestedProbeDeclinesAnAlias(t *testing.T) {
	q, err := ParseAliases(`h.ClusterId == 5`, []string{"h", "e"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range q.Probes() {
		if p.Nested() {
			t.Errorf("alias-qualified %q became a nested path %v", p.Attr, p.Path)
		}
	}
}
