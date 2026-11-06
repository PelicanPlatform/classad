package vm

import (
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// subscriptScopes are the ads the element load is checked against. Between them they hold every
// container shape a subscript can be handed, and the cyclic attributes that make the LAZINESS of the
// index observable rather than merely documented.
var subscriptScopes = []string{
	`[L = {1, 2, 3}; Empty = {}; Rec = [K = 7; S = "x"]; N = 1; R = 1.5; Str = "abc"; B = true; Owner = "alice"]`,
	`[L = {Owner, Cpus, "lit"}; Cpus = 4; Owner = "alice"; Rec = [K = Cpus + 1]]`,
	// Cyc is cyclic, so EVALUATING it is an error and the engines must agree about when it is
	// evaluated at all. Lazy holds a self-referential element that indexing another element must
	// not touch.
	`[Cyc = Cyc + 1; L = {1, 2}; Lazy = {Cyc, 9}; Zero = 0; Empty = {}]`,
	`[]`,
}

// subscriptExprs is the parity corpus: every case classad.Evaluator.ApplySubscript distinguishes,
// and every case the lowering could get wrong.
//
// It is one list because both fuzz targets and the differential test seed from it, so a case added
// for one of them is checked by all three.
var subscriptExprs = []string{
	// a list at an integer index, in range and at both edges
	`L[0]`, `L[1]`, `L[2]`, `{1, 2, 3}[0]`, `{"a", "b"}[1]`,
	// out of range, high and negative: ERROR in the reference engine, not UNDEFINED
	`L[3]`, `L[100]`, `L[-1]`, `L[-100]`, `Empty[0]`, `{}[0]`,
	// a non-integer index into a list
	`L[1.0]`, `L[R]`, `L["0"]`, `L[true]`, `L[B]`, `L[{0}]`, `L[Rec]`,
	// an ad at a string key: present, absent, and a non-string index
	`Rec["K"]`, `Rec["k"]`, `Rec["S"]`, `Rec["missing"]`, `Rec[0]`, `Rec[Owner]`,
	`[K = 7]["K"]`, `[K = 7]["nope"]`,
	// a container that is neither a list nor an ad
	`N[0]`, `Str[0]`, `Str["a"]`, `B[0]`, `R[0]`, `Owner[0]`,
	// UNDEFINED and ERROR on either side
	`Missing[0]`, `Missing[Missing2]`, `undefined[0]`, `error[0]`, `L[undefined]`, `L[error]`,
	`L[Missing]`, `undefined[undefined]`, `error[error]`, `error[undefined]`, `undefined[error]`,
	// THE LAZINESS CASES. The container settles the result, so the index must not be evaluated:
	// evaluating Cyc panics, which an eager form would turn into ERROR for the whole expression.
	`Missing[Cyc]`, `undefined[Cyc]`, `error[Cyc]`, `Missing[Cyc + 1]`, `Missing[L[Cyc]]`,
	// a cyclic container, and a lazy list whose OTHER element is cyclic
	`Cyc[0]`, `Cyc[Cyc]`, `Lazy[1]`, `Lazy[0]`, `{Cyc, 9}[1]`,
	// computed indices, nesting, and a subscript under the native operators
	`L[N]`, `L[N - 1]`, `L[Zero]`, `L[L[0]]`, `{{1, 2}, {3}}[0][1]`, `Rec["K"] + 1`,
	`L[0] == 1`, `L[0] > 0 && L[1] > 0`, `L[9] ?: 42`, `Missing[0] ?: 42`,
	`-L[0]`, `toUpper(Rec["S"])`, `ifThenElse(L[0] > 0, L[1], L[2])`,
	`L[ifThenElse(true, 0, 1)]`, `isUndefined(Missing[0])`, `isError(L[9])`,
	// an index that short-circuits on its own, under a container that does not short the subscript
	`L[0 && 1]`, `L[Missing ?: 1]`,
}

// subscriptParity is the law: the compiled program and classad.Eval agree, in both instruction
// forms, for every container/index pair.
func subscriptParity(t *testing.T, scopeSrc, exprSrc string) {
	t.Helper()
	scope, err := classad.Parse(scopeSrc)
	if err != nil || scope == nil {
		t.Fatalf("scope %q: %v", scopeSrc, err)
	}
	expr, err := parser.ParseExpr(exprSrc)
	if err != nil || expr == nil {
		t.Fatalf("expr %q: %v", exprSrc, err)
	}
	want := refEval(scope, expr)
	p := CompileProgram(expr)
	if got := Run(p, scope); !valuesEqual(want, got) {
		t.Errorf("stack form: %s over %s: want %s, got %s",
			exprSrc, scopeSrc, describe(want), describe(got))
	}
	rp := LowerToRegisters(p)
	if got := RunRegisters(rp, scope); !valuesEqual(want, got) {
		t.Errorf("register form: %s over %s: want %s, got %s",
			exprSrc, scopeSrc, describe(want), describe(got))
	}
	if got := RunRegisters(Optimize(rp, AllPasses()), scope); !valuesEqual(want, got) {
		t.Errorf("optimized: %s over %s: want %s, got %s",
			exprSrc, scopeSrc, describe(want), describe(got))
	}
}

// TestSubscriptParity runs the corpus against every scope.
//
// Parity with the evaluator is the whole requirement for this opcode: a subscript that computes a
// different element, or ERROR where the evaluator says UNDEFINED, is worse than a subscript the
// compiler declines to lower, because the second is visible and the first is not.
func TestSubscriptParity(t *testing.T) {
	for _, scopeSrc := range subscriptScopes {
		for _, exprSrc := range subscriptExprs {
			subscriptParity(t, scopeSrc, exprSrc)
		}
	}
}

// FuzzSubscriptParity widens the corpus to generated container/index pairs.
func FuzzSubscriptParity(f *testing.F) {
	for _, scopeSrc := range subscriptScopes {
		for _, exprSrc := range subscriptExprs {
			f.Add(scopeSrc, exprSrc)
		}
	}
	f.Fuzz(func(t *testing.T, scopeSrc, exprSrc string) {
		scope, err := classad.Parse(scopeSrc)
		if err != nil || scope == nil {
			return
		}
		expr, err := parser.ParseExpr(exprSrc)
		if err != nil || expr == nil {
			return
		}
		want := refEval(scope, expr)
		p := CompileProgram(expr)
		if got := Run(p, scope); !valuesEqual(want, got) {
			t.Fatalf("stack form: expr=%q scope=%q: want=%s got=%s",
				exprSrc, scopeSrc, describe(want), describe(got))
		}
		if got := RunRegisters(LowerToRegisters(p), scope); !valuesEqual(want, got) {
			t.Fatalf("register form: expr=%q scope=%q: want=%s got=%s",
				exprSrc, scopeSrc, describe(want), describe(got))
		}
	})
}

// TestSubscriptDoesNotEvaluateTheIndexOfAnAbsentContainer is the one case where getting the opcode
// "right" and getting it eager give different ANSWERS rather than different work.
//
// Cyc is self-referential, so evaluating it panics and the interpreter turns that into ERROR for the
// whole program. The evaluator never evaluates the index of an undefined container, so the answer is
// UNDEFINED; an element load that evaluated both operands before testing either would say ERROR.
// This is what OpShortSubscript's jump is for.
func TestSubscriptDoesNotEvaluateTheIndexOfAnAbsentContainer(t *testing.T) {
	scope, err := classad.Parse(`[Cyc = Cyc + 1]`)
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{`Missing[Cyc]`, `undefined[Cyc]`} {
		expr, err := parser.ParseExpr(src)
		if err != nil {
			t.Fatal(err)
		}
		if got := Run(CompileProgram(expr), scope); !got.IsUndefined() {
			t.Errorf("%s = %s, want undefined: the index of an absent container must not be evaluated",
				src, describe(got))
		}
	}
	// The error container is the same jump with the other outcome.
	expr, _ := parser.ParseExpr(`error[Cyc]`)
	if got := Run(CompileProgram(expr), scope); !got.IsError() {
		t.Errorf("error[Cyc] = %s, want error", describe(got))
	}
}

// TestSubscriptIsNative pins the property the opcode exists for: a query containing a subscript
// stays Native(), which is what the columnar accelerators gate on.
func TestSubscriptIsNative(t *testing.T) {
	for _, src := range []string{`L[0]`, `L[0] == 1`, `Rec["K"] > 2`, `L[Cpus] == "x" && Cpus > 1`} {
		expr, err := parser.ParseExpr(src)
		if err != nil {
			t.Fatal(err)
		}
		if p := CompileProgram(expr); len(p.nodes) != 0 {
			t.Errorf("%s compiled %d delegated subtree(s); the element load must be native", src, len(p.nodes))
		}
	}
	// The operands are compiled on their own merits, so a delegated one still delegates. The list
	// literal is the delegated node here, and the element load above it is not.
	expr, _ := parser.ParseExpr(`{1, Cpus}[0]`)
	p := CompileProgram(expr)
	if len(p.nodes) != 1 {
		t.Fatalf("{1, Cpus}[0] compiled %d delegated subtrees, want 1 (the list literal)", len(p.nodes))
	}
	if got := opcodeCount(p, OpSubscript); got != 1 {
		t.Errorf("{1, Cpus}[0] emitted %d element loads, want 1", got)
	}
}

// TestSubscriptIsChargedOneStep states the step cost in a test rather than in a comment.
//
// The element load is a VALUE OPERATION, charged one step like a binary operator or a call: it
// produces a value from already-computed operands. The container test is a jump and is charged
// nothing, exactly as the Elvis test and the logical short-circuits are -- and it cannot make a loop
// free, because the container expression under it is itself charged.
func TestSubscriptIsChargedOneStep(t *testing.T) {
	cases := []struct {
		src  string
		want int
	}{
		{`L`, 1},          // the reference load alone
		{`L[0]`, 3},       // container, index, element load
		{`L[0] + 1`, 5},   // ... plus the literal and the operator
		{`Missing[0]`, 1}, // the reference alone: the jump skips the index AND the load
	}
	scope, err := classad.Parse(`[L = {1, 2, 3}]`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		expr, err := parser.ParseExpr(tc.src)
		if err != nil {
			t.Fatal(err)
		}
		p := CompileProgram(expr)
		if _, ok := RunLimited(p, scope, tc.want); !ok {
			t.Errorf("%s did not finish within %d steps", tc.src, tc.want)
		}
		if _, ok := RunLimited(p, scope, tc.want-1); ok {
			t.Errorf("%s finished within %d steps; it should cost %d", tc.src, tc.want-1, tc.want)
		}
	}
}

// TestSubscriptDeclinesVector is the vector decision, pinned.
//
// The vector executor has no element-load kernel: a Vec holds scalars, and both the container and
// the result of a subscript can be a list or an ad. It therefore declines the whole batch, and the
// caller falls back to the per-record path. A decline is an acceptable answer and a partial one is
// not, so this asserts that it declines rather than that it declines "for now".
func TestSubscriptDeclinesVector(t *testing.T) {
	src := newTestSource([]map[string]string{
		{"Cpus": "1", "L": "{1, 2}"},
		{"Cpus": "2", "L": "{3, 4}"},
	})
	for _, exprSrc := range []string{`L[0] == 1`, `Cpus > 0 && L[0] == 1`, `Missing[0] =?= undefined`} {
		expr, err := parser.ParseExpr(exprSrc)
		if err != nil {
			t.Fatal(err)
		}
		q := Compile(expr)
		if !q.Native() {
			t.Fatalf("%s is not Native, so this test is not exercising the decline", exprSrc)
		}
		if v, ok := q.VecEval(src, 2, nil); ok {
			t.Errorf("%s vectorized (%v); the element load has no kernel and must decline", exprSrc, v)
		}
	}
}

// TestSubscriptScriptDeclinesVector is the same decline for a SCRIPT, which reaches the executor by
// a different entry point.
func TestSubscriptScriptDeclinesVector(t *testing.T) {
	sp, err := ParseScript("x = MY.L\nresult = x[0]\n")
	if err != nil {
		t.Fatal(err)
	}
	src := newTestSource([]map[string]string{
		{"L": "{1, 2}"},
		{"L": "{3, 4}"},
	})
	if v, ok := sp.VecEval(src, 2, nil); ok {
		t.Errorf("a script with an element load vectorized (%v); it must decline", v)
	}
}

// TestLocalCanBeSubscripted is the first thing the opcode unblocks.
//
// `x = SomeList` followed by `x[0]` is refused while a subscript is delegated, because a delegated
// subtree is evaluated against the ad and would read the ATTRIBUTE x. With the element load native
// the local is read from its register.
func TestLocalCanBeSubscripted(t *testing.T) {
	scope, err := classad.Parse(`[L = {10, 20, 30}; Rec = [K = 7]]`)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		src  string
		want int64
	}{
		{"x = MY.L\nresult = x[0]\n", 10},
		{"x = MY.L\nresult = x[1] + x[2]\n", 50},
		{"x = MY.L\ni = 2\nresult = x[i]\n", 30},
		{"x = MY.Rec\nresult = x[\"K\"]\n", 7},
		{"x = {1, 2, 3}\nresult = x[1]\n", 2},
	}
	for _, tc := range cases {
		sp, err := ParseScript(tc.src)
		if err != nil {
			t.Fatalf("%q: %v", tc.src, err)
		}
		got := sp.Run(scope)
		if iv, err := got.IntValue(); err != nil || iv != tc.want {
			t.Errorf("%q = %s, want %d", tc.src, describe(got), tc.want)
		}
	}
}

// TestSubscriptOfALocalDoesNotReadTheAttribute is the mistake the refusal used to stand in for: the
// script's x and the ad's x are different values, and the element load must read the script's.
func TestSubscriptOfALocalDoesNotReadTheAttribute(t *testing.T) {
	scope, err := classad.Parse(`[x = {100, 200}]`)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := ParseScript("x = {1, 2}\nresult = x[0]\n")
	if err != nil {
		t.Fatal(err)
	}
	got := sp.Run(scope)
	if iv, err := got.IntValue(); err != nil || iv != 1 {
		t.Errorf("result = %s, want 1 (the LOCAL x, not the attribute)", describe(got))
	}
}

// TestRegisteredScriptFunctionCanSubscript is the second thing the opcode unblocks.
//
// checkPure refuses any delegated subtree, so `def f(x): return x[0]` cannot be registered while a
// subscript is delegated -- and the refusal it gets first is the local capture, since the parameter
// is a local. With the element load native it compiles, passes the purity gate, and runs.
func TestRegisteredScriptFunctionCanSubscript(t *testing.T) {
	m, err := ParseScriptModule("def firstOf(seq):\n    return seq[0]\n")
	if err != nil {
		t.Fatal(err)
	}
	f, err := m.Compile("firstOf")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	list, err := parser.ParseExpr(`{11, 22}`)
	if err != nil {
		t.Fatal(err)
	}
	arg := classad.NewEvaluator(nil).Evaluate(list)
	got := f.Call([]classad.Value{arg})
	if iv, err := got.IntValue(); err != nil || iv != 11 {
		t.Errorf("firstOf({11, 22}) = %s, want 11", describe(got))
	}
	// The ad shapes the function must survive, all of which are the evaluator's own answers.
	for _, bad := range []classad.Value{classad.NewUndefinedValue(), classad.NewIntValue(3),
		classad.NewStringValue("abc")} {
		got := f.Call([]classad.Value{bad})
		if !got.IsUndefined() && !got.IsError() {
			t.Errorf("firstOf(%s) = %s, want undefined or error", describe(bad), describe(got))
		}
	}
}

// TestSubscriptCarriesAScriptFunctionCall is the other half of the same unblocking: a call inside a
// subscript is expanded in place, where a delegated subscript would have had to refuse it.
func TestSubscriptCarriesAScriptFunctionCall(t *testing.T) {
	sp, err := ParseScript("def pick():\n    return 1\n\nresult = {7, 8, 9}[pick()]\n")
	if err != nil {
		t.Fatalf("ParseScript: %v", err)
	}
	got := sp.Run(nil)
	if iv, err := got.IntValue(); err != nil || iv != 8 {
		t.Errorf("result = %s, want 8", describe(got))
	}
}

// TestSubscriptStillRefusesADelegatedOperandReadingALocal pins the guard that is still there: the
// element load is native, but a LIST LITERAL under it is not, so a local read from inside one is
// still a wrong answer waiting to be computed.
func TestSubscriptStillRefusesADelegatedOperandReadingALocal(t *testing.T) {
	_, err := ParseScript("x = 1\nresult = {x, 2}[0]\n")
	if err == nil {
		t.Fatal("expected a refusal: the list literal under the element load reads the local x")
	}
	if !strings.Contains(err.Error(), "reads the local") {
		t.Errorf("refusal %q does not explain the capture", err)
	}
}

// opcodeCount counts how many times op appears in p.
func opcodeCount(p *Program, op Opcode) int {
	n := 0
	for _, in := range p.code {
		if in.Op == op {
			n++
		}
	}
	return n
}

// TestSubscriptJumpIsInTheControlFlowGraph pins the pair of successor tables.
//
// A jump the CFG does not know about makes a DOMINANCE answer wrong rather than conservative: the
// graph then believes everything between the container test and the join is on every path to the
// join, so a store made only on the index's path looks like it reaches a read after it. Nothing the
// compiler emits today puts such a store there -- an inlined call's registers are read inside its
// own expansion -- which is exactly why this is pinned against the tables and not against a program
// whose behaviour happens to differ.
func TestSubscriptJumpIsInTheControlFlowGraph(t *testing.T) {
	expr, err := parser.ParseExpr(`L[0]`)
	if err != nil {
		t.Fatal(err)
	}
	p := CompileProgram(expr)
	var buf []int
	found := false
	for ip, in := range p.code {
		if in.Op != OpShortSubscript {
			continue
		}
		found = true
		succs := stackSuccs(p, ip, buf)
		if len(succs) != 2 || succs[0] != int(in.A) || succs[1] != ip+1 {
			t.Errorf("stackSuccs at the container test = %v, want the jump target %d and the fall-through %d",
				succs, in.A, ip+1)
		}
	}
	if !found {
		t.Fatal("L[0] emitted no container test")
	}
	rp := LowerToRegisters(p)
	found = false
	for ip, in := range rp.code {
		if in.Op != RShortSubscript {
			continue
		}
		found = true
		succs := regSuccs(rp, ip, buf)
		if len(succs) != 2 || succs[0] != int(in.T) || succs[1] != ip+1 {
			t.Errorf("regSuccs at the container test = %v, want the jump target %d and the fall-through %d",
				succs, in.T, ip+1)
		}
	}
	if !found {
		t.Fatal("the lowering emitted no container test")
	}
}
