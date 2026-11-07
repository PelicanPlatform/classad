package db

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestParseGroupValueInvertsEveryGroup pins ParseGroupValue as GroupValueText's inverse for
// every KIND of group, lists and nested ads among them: the text a group is rendered as reads
// back as a value that renders as the same text. A break here means a label and the value
// beside it name different groups.
//
// The list and the nested ad are here because they are the kinds that need not have been. They
// are grouped by their text rather than by a relation, and nothing forces that text to be
// text a parser accepts -- it was not, for the list, and so the inverse had one kind it
// answered with the label as a bare string instead of the value. It has none now.
func TestParseGroupValueInvertsEveryGroup(t *testing.T) {
	for _, text := range []string{
		`"ap1.example.org"`, `""`, `"4"`, `"he said \"hi\""`, `"back\\slash"`,
		`42`, `-7`, `0`, `9007199254740993`, `1.5`, `-0.25`,
		`true`, `false`, `undefined`, `error`,
		`{ 1,2 }`, `{  }`, `{ "a","b" }`, `{ 1,2.5,true }`, `{ { 1 },2 }`,
		`[a = 1]`, `[]`, `[a = 1; b = "x"]`,
	} {
		got := ParseGroupValue(text)
		if back := GroupValueText(got); back != text {
			t.Errorf("ParseGroupValue(%q) rendered back as %q, want the text it came from", text, back)
		}
	}
}

// TestGroupTextRoundTripsThroughItsValue is the invariant stated the other way round, from a
// VALUE rather than from a text, and over values a caller would actually write rather than
// over canonical renderings. It is the one that catches a renderer that emits text its own
// parser reads as a different group -- which is exactly what "[1 2]" did, reading back as a
// string whose group text was "\"[1 2]\"".
func TestGroupTextRoundTripsThroughItsValue(t *testing.T) {
	for _, src := range []string{
		`"alice"`, `1`, `1.0`, `2.5`, `true`, `undefined`, `error`,
		`{1,2}`, `{1,1+1}`, `{1,2.0}`, `{}`, `{"a","b"}`, `{1.5}`, `{{1},2}`,
		`[a = 1]`, `[]`, `[b = "x"; a = 1]`,
	} {
		e, err := classad.ParseExpr(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		v := e.Eval(classad.New())
		text := GroupValueText(v)
		if back := GroupValueText(ParseGroupValue(text)); back != text {
			t.Errorf("%s grouped as %q, which reads back as the group %q", src, text, back)
		}
	}
}

// TestListGroupsByItsElementsGroups pins the relation a list is grouped by: element-wise, by
// the same relation an element alone is grouped by. {1} and {1.0} are one group because 1 and
// 1.0 are, and {1,1+1} and {1,2} are one group because a list's group is its VALUE and not the
// expressions it was written as. The reference unparser agrees with neither.
func TestListGroupsByItsElementsGroups(t *testing.T) {
	same := [][]string{
		{`{1}`, `{1.0}`},
		{`{1,2}`, `{1,1+1}`, `{1.0,2.0}`},
		{`{}`, `{ }`},
	}
	for _, group := range same {
		var want string
		for i, src := range group {
			e, err := classad.ParseExpr(src)
			if err != nil {
				t.Fatalf("parse %q: %v", src, err)
			}
			got := GroupValueText(e.Eval(classad.New()))
			if i == 0 {
				want = got
				continue
			}
			if got != want {
				t.Errorf("%s and %s are one value but group as %q and %q", group[0], src, want, got)
			}
		}
	}
	// GroupValue must agree, and element-wise: two members of one group canonicalize to one
	// value, so a list group's representative does not depend on which row formed it. Only
	// the text is checked above, and the text would stay right even if the value did not.
	for _, pair := range [][2]string{{`{1}`, `{1.0}`}, {`{1,2}`, `{1,1+1}`}} {
		var kinds [2][]classad.ValueType
		for i, src := range pair {
			e, _ := classad.ParseExpr(src)
			elems, err := GroupValue(e.Eval(classad.New())).ListValue()
			if err != nil {
				t.Fatalf("%s: GroupValue is not a list: %v", src, err)
			}
			for _, el := range elems {
				kinds[i] = append(kinds[i], el.Type())
			}
		}
		if fmt.Sprint(kinds[0]) != fmt.Sprint(kinds[1]) {
			t.Errorf("%s and %s are one group but canonicalize to element kinds %v and %v",
				pair[0], pair[1], kinds[0], kinds[1])
		}
	}

	// And the collision that literal syntax removes: an empty list is not an empty ad.
	empties := map[string]string{}
	for _, src := range []string{`{}`, `[]`} {
		e, _ := classad.ParseExpr(src)
		empties[src] = GroupValueText(e.Eval(classad.New()))
	}
	if empties[`{}`] == empties[`[]`] {
		t.Errorf("the empty list and the empty nested ad share the group %q", empties[`{}`])
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
