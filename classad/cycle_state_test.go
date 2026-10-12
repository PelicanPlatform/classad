package classad

import (
	"testing"

	"github.com/PelicanPlatform/classad/parser"
)

// TestCycleDetectionGolden pins cyclic-reference results, recorded from the engine
// as it was when the in-progress set lived on the ClassAd. Moving that set onto the
// evaluation must not change any of them -- in particular the cases where a builtin
// materializes a lazy list whose element refers back to the attribute being
// evaluated, which only see the cycle if the list is evaluated as part of the same
// evaluation.
func TestCycleDetectionGolden(t *testing.T) {
	cases := []struct{ src, want string }{
		{`[A = member(1, {A})]`, "false"},
		{`[A = sum({A, 1})]`, "error"},
		{`[A = size({A})]`, "1"},
		{`[A = join(",", {A})]`, "error"},
		{`[A = anyCompare("==", {A}, 1)]`, "error"},
		{`[A = identicalMember(1, {A})]`, "false"},
		{`[A = regexpMember("x", {A})]`, "false"},
		{`[A = min({A, 1})]`, "error"},
		{`[A = max({A,2})]`, "error"},
		{`[A = avg({A})]`, "error"},
		{`[A = quantize(3, {A})]`, "error"},
		{`[A = countMatches(B, {[C=1]}); B = A]`, "0"},
		{`[A = evalInEachContext(A, {[C=1]})]`, "[[undefined]]"},
		{`[A = {A}[0]]`, "error"},
		{`[A = {B}[0]; B = A]`, "error"},
		{`[A = member(1, {B}); B = {A}[0]]`, "false"},
		{`[A = [X = A].X]`, "error"},
		{`[A = [X = 1]["X"]]`, "1"},
		{`[A = strcat({A})]`, `"{ A }"`},
		{`[A = member(B, {1}); B = member(A, {1})]`, "error"},
		{`[L = {A}; A = member(1, L)]`, "false"},
		{`[A = allCompare("<", {A, 3}, 5)]`, "error"},
		{`[A = evalInEachContext(B, {[C=1]}); B = A]`, "[undefined]"},
		{`[A = B; B = C; C = D; D = E; E = F; F = G; G = H; H = I; I = A]`, "error"}, // deeper than the inline set
		{`[A = B; B = C; C = D; D = E; E = F; F = G; G = H; H = I; I = 7]`, "7"},
	}
	for _, tc := range cases {
		ad, err := Parse(tc.src)
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		if got := ad.EvaluateAttr("A").String(); got != tc.want {
			t.Errorf("%s: A = %s, want %s", tc.src, got, tc.want)
		}
	}
}

// TestCycleAcrossMyAndTarget: an attribute whose value runs through the other ad
// and back (job A -> TARGET.B, slot B -> TARGET.A) is a cycle, whether the pairing
// is recorded on the ads (NewMatchClassAd) or only for the evaluation
// (EvaluateExprAgainst). A non-cyclic path through both ads still resolves.
func TestCycleAcrossMyAndTarget(t *testing.T) {
	job, err := Parse(`[A = TARGET.B + 1; C = TARGET.D + 1; Mem = 10]`)
	if err != nil {
		t.Fatal(err)
	}
	slot, err := Parse(`[B = TARGET.A + 1; D = TARGET.Mem * 2]`)
	if err != nil {
		t.Fatal(err)
	}
	exprA, _ := parser.ParseExpr("A")
	exprC, _ := parser.ParseExpr("C")

	if v := job.EvaluateExprAgainst(exprA, slot); !v.IsError() {
		t.Errorf("EvaluateExprAgainst: A = %v, want error (cycle through TARGET)", v)
	}
	if v := job.EvaluateExprAgainst(exprC, slot); v.String() != "21" {
		t.Errorf("EvaluateExprAgainst: C = %v, want 21", v)
	}
	if job.GetTarget() != nil || slot.GetTarget() != nil {
		t.Fatal("EvaluateExprAgainst set TARGET on an ad")
	}

	m := NewMatchClassAd(job, slot)
	if v := m.EvaluateAttrLeft("A"); !v.IsError() {
		t.Errorf("MatchClassAd: A = %v, want error (cycle through TARGET)", v)
	}
	if v := m.EvaluateAttrRight("B"); !v.IsError() {
		t.Errorf("MatchClassAd: B = %v, want error (cycle through TARGET)", v)
	}
	if v := m.EvaluateAttrLeft("C"); v.String() != "21" {
		t.Errorf("MatchClassAd: C = %v, want 21", v)
	}
}

// TestLazyListKeepsTargetBinding: a list value created under an evaluation-scoped
// TARGET still resolves TARGET when materialized after that evaluation returned.
func TestLazyListKeepsTargetBinding(t *testing.T) {
	job, _ := Parse(`[L = {TARGET.Memory, 1}]`)
	slot, _ := Parse(`[Memory = 4096]`)
	expr, _ := parser.ParseExpr("L")
	v := job.EvaluateExprAgainst(expr, slot)
	elems, err := v.ListValue()
	if err != nil || len(elems) != 2 || elems[0].String() != "4096" {
		t.Fatalf("L = %v (%v), want {4096, 1}", elems, err)
	}
}
