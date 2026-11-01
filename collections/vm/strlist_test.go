package vm

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// TestStrListMemberSpecialized pins which calls get the set lookup and which stay a generic call.
// The declines matter as much as the accepts: each is a call the specialization would answer
// differently from the reference, so compiling it would be a wrong answer rather than a slow one.
func TestStrListMemberSpecialized(t *testing.T) {
	for _, src := range []string{
		`stringListMember(Owner, "alice,bob")`,
		`stringListMember(Owner, "")`,
		`stringListMember(Owner, "alice;bob", ";")`,
		`stringListMember(Owner, ("alice,bob"))`, // parentheses are transparent
		`STRINGLISTMEMBER(Owner, "alice")`,       // names fold
	} {
		q, err := Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		if !usesOp(q.Program(), OpStrListMember) {
			t.Errorf("%s: want OpStrListMember", src)
		}
		if usesOp(q.Program(), OpCallFunc) {
			t.Errorf("%s: compiled to a generic call as well", src)
		}
	}
	for src, why := range map[string]string{
		`stringListIMember(Owner, "ALICE")`:      "case-insensitive: strings.EqualFold is not a map keyed by ToLower",
		`stringListMember(Owner, Owner)`:         "the list is not constant, so it differs per record",
		`stringListMember(Owner, Cpus)`:          "likewise",
		`stringListMember(Owner, "a", Cpus)`:     "the delimiter is not constant",
		`stringListMember(Owner)`:                "wrong arity: ERROR without evaluating the arguments",
		`stringListMember(Owner, "a", ",", "x")`: "likewise",
		`stringListMember(Owner, 5)`:             "the list argument is not a string literal",
	} {
		q, err := Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		if usesOp(q.Program(), OpStrListMember) {
			t.Errorf("%s: must NOT be specialized (%s)", src, why)
		}
	}
}

// TestStrListMemberSemantics checks the compiled form against the tree-walking evaluator on the cases
// the specialization could plausibly get wrong, in all three executors.
//
// The element trimming and the default delimiter splitting on SPACE as well as comma are not
// reimplemented -- the split comes from classad.SplitStringList -- so these assertions are really
// checking that the right function is being called with the right arguments, and that the item
// coercion (undefined is the empty string, a non-string is ERROR) is not quietly simplified.
func TestStrListMemberSemantics(t *testing.T) {
	ad, err := classad.Parse(`[ Owner = "alice"; Spaced = " alice "; N = 5; Flag = true; Empty = "" ]`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, tc := range []struct{ src, want string }{
		{`stringListMember(Owner, "alice,bob")`, "true"},
		{`stringListMember(Owner, "bob,carol")`, "false"},
		{`stringListMember(Owner, " alice , bob ")`, "true"}, // elements are trimmed
		{`stringListMember(Spaced, "alice,bob")`, "false"},   // the ITEM is not trimmed
		{`stringListMember(Owner, "a b,alice")`, "true"},     // default delimiter splits on space too
		{`stringListMember(Owner, "alice bob")`, "true"},     // so this is two elements
		{`stringListMember(Owner, "alice;bob", ";")`, "true"},
		{`stringListMember(Owner, "alice;bob")`, "false"}, // ';' is not a default delimiter
		// The split uses FieldsFunc, which DROPS empty fields, so "a,,b" is two elements and the
		// empty string is not among them. An empty or undefined item therefore matches nothing,
		// which is the opposite of the obvious guess and is why both are pinned here.
		{`stringListMember(Empty, "a,,b")`, "false"},
		{`stringListMember(Empty, "a,b")`, "false"},
		{`stringListMember(Missing, "a,,b")`, "false"},
		{`stringListMember(Missing, "a,b")`, "false"},
		{`stringListMember(N, "5,6")`, "error"},     // a number is not a usable item
		{`stringListMember(Flag, "true")`, "error"}, // nor a boolean
		{`stringListMember(Owner, "")`, "false"},    // the empty list matches nothing
	} {
		expr, err := parser.ParseExpr(tc.src)
		if err != nil {
			t.Fatalf("parse %s: %v", tc.src, err)
		}
		p := CompileProgram(expr)
		if !usesOp(p, OpStrListMember) {
			t.Fatalf("%s: not specialized, so this test is not testing the specialization", tc.src)
		}
		got := Run(p, ad)
		if got.String() != tc.want {
			t.Errorf("%s = %s, want %s", tc.src, describe(got), tc.want)
		}
		if ref := refEval(ad, expr); !valuesEqual(ref, got) {
			t.Errorf("%s: evaluator=%s compiled=%s", tc.src, describe(ref), describe(got))
		}
		if reg := RunRegisters(LowerToRegisters(p), ad); !valuesEqual(got, reg) {
			t.Errorf("%s: stack=%s register=%s", tc.src, describe(got), describe(reg))
		}
	}
}
