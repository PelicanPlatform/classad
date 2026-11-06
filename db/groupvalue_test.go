package db

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestParseGroupValueInvertsScalarGroups pins ParseGroupValue as GroupValueText's inverse for
// every scalar group: the text a group is rendered as reads back as a value that renders as
// the same text. A break here means a label and the value beside it name different groups.
func TestParseGroupValueInvertsScalarGroups(t *testing.T) {
	for _, text := range []string{
		`"ap1.example.org"`, `""`, `"4"`, `"he said \"hi\""`, `"back\\slash"`,
		`42`, `-7`, `0`, `9007199254740993`, `1.5`, `-0.25`,
		`true`, `false`, `undefined`, `error`,
	} {
		got := ParseGroupValue(text)
		if back := GroupValueText(got); back != text {
			t.Errorf("ParseGroupValue(%q) rendered back as %q, want the text it came from", text, back)
		}
	}
}

// TestParseGroupValueFastPathAgreesWithTheParser pins the fast path against the real parser:
// every text one of them accepts, both must read the same way. A fast path that disagrees is
// worse than none, because it is the one that runs.
func TestParseGroupValueFastPathAgreesWithTheParser(t *testing.T) {
	for _, text := range []string{
		`"ap1"`, `""`, `"4"`, `"a b"`, `"he said \"hi\""`, `"back\\slash"`,
		`0`, `42`, `-7`, `+42`, `1_0`, `007`, `1.5`, `true`, `undefined`, `{1,2}`, `not a group`,
	} {
		fast, ok := parseGroupValueFast(text)
		if !ok {
			continue // declined: the parser is the only answer, nothing to compare
		}
		e, err := classad.ParseExpr(text)
		if err != nil {
			t.Errorf("fast path accepted %q, which the parser rejects", text)
			continue
		}
		slow := GroupValue(e.Eval(classad.New()))
		if GroupValueText(fast) != GroupValueText(slow) {
			t.Errorf("%q: fast path gave %q, parser gave %q", text, GroupValueText(fast), GroupValueText(slow))
		}
	}
}

// TestGroupValueNamesTheSameGroupAsItsText pins the invariant AggRow.GroupValues rests on:
// the canonical value of a group renders as that group's text. 1 and 1.0 are deliberately one
// group, so both must land on one value.
func TestGroupValueNamesTheSameGroupAsItsText(t *testing.T) {
	for _, text := range []string{`1`, `1.0`, `"x"`, `2.5`, `-0.0`, `true`, `undefined`} {
		e, err := classad.ParseExpr(text)
		if err != nil {
			t.Fatalf("parse %q: %v", text, err)
		}
		v := e.Eval(classad.New())
		if got, want := GroupValueText(GroupValue(v)), GroupValueText(v); got != want {
			t.Errorf("%q: GroupValue renders as %q, its own text is %q", text, got, want)
		}
	}
	one, _ := classad.ParseExpr(`1`)
	oneReal, _ := classad.ParseExpr(`1.0`)
	a := GroupValue(one.Eval(classad.New()))
	b := GroupValue(oneReal.Eval(classad.New()))
	if a.Type() != b.Type() {
		t.Errorf("1 and 1.0 are one group but canonicalize to %v and %v", a.Type(), b.Type())
	}
}
