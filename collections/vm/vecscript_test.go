package vm

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// THE VECTOR SCRIPT EXECUTOR, held to the law every other part of this executor is held to: the same
// compiled script, run over a batch and run per record, must agree value for value.
//
// What is new here is the BINDING. An expression's vectors live and die on the stack; a local
// outlives the statement that produced it and is read again later, so it is the first thing in this
// executor with a lifetime, and the first thing a stack that recycles by depth can silently clobber.
// The scalar side of the law is the register executor, which is itself checked against the stack
// interpreter, which is checked against classad.Eval, which is differentially fuzzed against the C++
// engine -- so a divergence lands on that chain rather than on a second opinion.

// scriptShape is one script, written as the statements that build it rather than as a syntax this
// package does not yet have.
type scriptShape struct {
	name string
	lets [][2]string // {local name, expression}
	res  string
}

// vecScriptShapes are the scripts the law runs. They are chosen for what they do to a BINDING, not
// for what they do to an expression -- the expressions themselves are already covered by vecExprs.
var vecScriptShapes = []scriptShape{
	// The binding is read once, which is the shape that would pass even if a load aliased the local.
	{"read once", [][2]string{{"t", "A + B"}}, "t > 5"},
	// Read MORE than once: the second read is what fails if a kernel consumed the binding in the
	// first. `t + t` with a destructive binop over an aliased local gives 4A rather than 2A.
	{"read twice", [][2]string{{"t", "A + B"}}, "t + t"},
	{"read three times", [][2]string{{"t", "A"}}, "t + t + t"},
	// A local whose value is a STRING, so the payload has to survive the copy and not just the state.
	{"string local", [][2]string{{"s", "S"}}, `s == "user3"`},
	{"string local twice", [][2]string{{"s", "S"}}, "s + s"},
	// A local bound to a CONSTANT, which is held as one element rather than a batch.
	{"const local", [][2]string{{"k", "7"}}, "A > k"},
	{"const local twice", [][2]string{{"k", "7"}}, "k + k"},
	// A local bound to a BOOLEAN, which the logical kernels hold as bitplanes rather than data: a copy
	// that only moved the data arrays would lose it.
	{"mask local", [][2]string{{"c", "A > 4"}}, "c && F"},
	{"mask local twice", [][2]string{{"c", "A > 4"}}, "c && c"},
	{"mask local as value", [][2]string{{"c", "A > 4"}}, "c"},
	// One local reading another, so a binding is both written and read as an operand.
	{"chained", [][2]string{{"t", "A + 1"}, {"u", "t * 2"}}, "u - t"},
	// REBINDING the same name, where the right-hand side reads the previous binding. The local is one
	// place written twice, and a read after it must see the second value.
	{"rebound", [][2]string{{"t", "A"}, {"t", "t + 1"}}, "t"},
	{"rebound then read", [][2]string{{"t", "A"}, {"t", "t + t"}}, "t > 4"},
	// A binding the result never reads: it still has to run, because its expression is where a fault
	// or an ERROR would come from, and it still must not disturb the stack.
	{"unread binding", [][2]string{{"t", "A / B"}, {"u", "B + 1"}}, "u"},
	// A local whose value is UNDEFINED or ERROR, carried through a copy.
	{"undefined local", [][2]string{{"t", "Missing"}}, "t =?= undefined"},
	{"error local", [][2]string{{"t", "A / B"}}, "t =?= error"},
	// A local in a position that routes to the parity hook rather than to a fast loop.
	{"hooked local", [][2]string{{"t", "S"}}, "t && true"},
	// Many locals, so the per-local vectors are allocated and reused rather than being one slot.
	{"five locals", [][2]string{{"a", "A"}, {"b", "B"}, {"c", "A + B"}, {"d", "A - B"}, {"e", "A * B"}},
		"a + b + c + d + e"},
}

// compileShape builds one script.
func compileShape(t *testing.T, sh scriptShape) *ScriptProgram {
	t.Helper()
	s := NewScript()
	for _, let := range sh.lets {
		e, err := parser.ParseExpr(let[1])
		if err != nil {
			t.Fatalf("%s: parse %q: %v", sh.name, let[1], err)
		}
		s = s.Let(let[0], e)
	}
	re, err := parser.ParseExpr(sh.res)
	if err != nil {
		t.Fatalf("%s: parse %q: %v", sh.name, sh.res, err)
	}
	sp, err := s.Result(re).Compile()
	if err != nil {
		t.Fatalf("%s: compile: %v", sh.name, err)
	}
	return sp
}

// TestVecScriptMatchesScalarPerRecord is the law.
func TestVecScriptMatchesScalarPerRecord(t *testing.T) {
	src := vecCorpus()
	n := len(src.recs)
	var scratch VecScratch
	for _, sh := range vecScriptShapes {
		sp := compileShape(t, sh)
		got, ok := sp.VecEval(src, n, &scratch)
		if !ok {
			t.Errorf("%s declined; a script of bindings and a result is meant to be servable", sh.name)
			continue
		}
		for i := 0; i < n; i++ {
			want := sp.Run(src.ad(i))
			if !sameValue(got.value(i), want) {
				t.Errorf("%s record %d: vector %s (state %d) != scalar %s",
					sh.name, i, got.value(i), got.St[i], want)
			}
		}
	}
	t.Logf("%d scripts x %d records verified against the register executor", len(vecScriptShapes), n)
}

// TestVecScriptUnboundLocalIsUndefined reaches a shape the compiler cannot emit.
//
// In a straight-line script every OpLoadLocal is preceded by its OpStoreLocal -- a name is not a local
// until Let has bound it, so a read before the store compiles to an ATTRIBUTE reference instead. The
// read-before-store shape needs a branch, where a store on one path does not reach a read after the
// merge, and the vector executor declines branches today. So the program is built by hand: the
// executor's answer for an unbound local has to be pinned BEFORE the construct that produces one
// arrives, or it gets settled by whatever the first branch implementation happens to do.
//
// UNDEFINED is the answer, because it is what the register lowering's initializer already writes for
// the same case (localsNeedingInit).
func TestVecScriptUnboundLocalIsUndefined(t *testing.T) {
	src := vecCorpus()
	n := len(src.recs)
	p := &Program{code: []Instr{{Op: OpLoadLocal, A: 0}}, locals: []string{"t"}}
	var scratch VecScratch
	got, ok := execVec(p, scratch.evaluator(), src, n, &scratch)
	if !ok {
		t.Fatal("reading an unbound local declined; it has a defined answer")
	}
	for i := 0; i < n; i++ {
		if !got.value(i).IsUndefined() {
			t.Errorf("record %d: an unbound local read as %s, want undefined", i, got.value(i))
		}
	}
}

// TestVecScriptBindingsDoNotLeakAcrossBatches pins what reset is for. The per-local vectors SURVIVE
// reset on purpose, so a scan reuses them across blocks rather than allocating per block; the only
// thing standing between one batch's binding and the next batch's read of an unbound local is the
// localsSet flag being cleared.
//
// It uses hand-built programs for the same reason the test above does, and it is the only thing that
// makes that clearing load-bearing: no script the compiler can emit today reads a local it has not
// stored, so without this the flags could be left set and nothing would notice until the masked store
// made the shape reachable -- at which point the bug would be a stale value from another block, which
// is a wrong answer rather than a crash.
func TestVecScriptBindingsDoNotLeakAcrossBatches(t *testing.T) {
	src := vecCorpus()
	n := len(src.recs)
	var scratch VecScratch

	// Batch one binds local 0 to a constant and returns it.
	bind := &Program{
		code:   []Instr{{Op: OpPushConst, A: 0}, {Op: OpStoreLocal, A: 0}, {Op: OpLoadLocal, A: 0}},
		consts: []classad.Value{classad.NewIntValue(42)},
		locals: []string{"t"},
	}
	got, ok := execVec(bind, scratch.evaluator(), src, n, &scratch)
	if !ok {
		t.Fatal("the binding program declined")
	}
	if v := got.value(0); !sameValue(v, classad.NewIntValue(42)) {
		t.Fatalf("the binding program returned %s, want 42", v)
	}

	// Batch two reads local 0 through the SAME scratch without storing it.
	read := &Program{code: []Instr{{Op: OpLoadLocal, A: 0}}, locals: []string{"t"}}
	got, ok = execVec(read, scratch.evaluator(), src, n, &scratch)
	if !ok {
		t.Fatal("the reading program declined")
	}
	for i := 0; i < n; i++ {
		if !got.value(i).IsUndefined() {
			t.Errorf("record %d: read %s from a local this batch never stored; "+
				"the previous batch's binding leaked", i, got.value(i))
		}
	}
}

// TestVecScriptDeclinesControlFlow pins the boundary, which is now the BACK EDGE and not the branch.
// If is served (see vecpred_test.go); While is not, because its live mask narrows across iterations
// and the loop runs until no record is still true, which needs per-record trip accounting the
// element-step budget has not been built for. Serving one silently is the failure this guards
// against.
func TestVecScriptDeclinesControlFlow(t *testing.T) {
	src := vecCorpus()
	n := len(src.recs)
	one, err := parser.ParseExpr("1")
	if err != nil {
		t.Fatal(err)
	}
	cond, err := parser.ParseExpr("A > 4")
	if err != nil {
		t.Fatal(err)
	}
	res, err := parser.ParseExpr("t")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		build func() *Script
	}{
		{"while", func() *Script {
			return NewScript().While(cond, func(s *Script) { s.Let("t", one) })
		}},
	} {
		sp, err := tc.build().Result(res).Compile()
		if err != nil {
			t.Fatalf("%s: compile: %v", tc.name, err)
		}
		if _, ok := sp.VecEval(src, n, nil); ok {
			t.Errorf("%s was SERVED by the vector executor, which has no masked store; "+
				"if that changed on purpose, this test says so", tc.name)
		}
	}
}
