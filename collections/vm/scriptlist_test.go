package vm

import (
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// compileFunc compiles one module function, failing the test if it will not compile.
func compileFunc(t *testing.T, src string) *ScriptFunc {
	t.Helper()
	m, err := ParseScriptModule(src)
	if err != nil {
		t.Fatalf("ParseScriptModule: %v", err)
	}
	f, err := m.Compile("f")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return f
}

func ints(vals ...int64) []classad.Value {
	out := make([]classad.Value, len(vals))
	for i, v := range vals {
		out[i] = classad.NewIntValue(v)
	}
	return out
}

// TestScriptFuncBuildsAList is the whole point of the eager lowering: a registered function can
// carry COMPOSITE state, which is what an aggregate holding a sum and a count needs and what a
// scalar one cannot express.
func TestScriptFuncBuildsAList(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		args      []classad.Value
		want      string
	}{
		{"of parameters", "def f(a, b):\n    return {a, b}\n", ints(3, 4), "{ 3,4 }"},
		{"read back", "def f(a, b):\n    s = {a, b}\n    return s[1]\n", ints(3, 4), "4"},
		{"nested", "def f(a, b):\n    return {{a, 1}, b}\n", ints(3, 4), "{ { 3,1 },4 }"},
		{"empty", "def f(a, b):\n    return {}\n", ints(3, 4), "{  }"},
		{"folded by a built-in", "def f(a, b):\n    return sum({a, b, a})\n", ints(3, 4), "10"},
		{"threaded through a call", "def f(a, b):\n    s = {a, b}\n    return size({s[0], s[1], a})\n", ints(3, 4), "3"},
		// An element may be any expression the function could otherwise compute, including one
		// reading a parameter inside a nested construct.
		{"computed elements", "def f(a, b):\n    return {a * 2, a > b ? a : b}\n", ints(3, 4), "{ 6,4 }"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := compileFunc(t, tc.src)
			got := classad.ApplyFunction(classad.NewEvaluator(nil), "string",
				[]classad.Value{f.Call(tc.args)})
			s, err := got.StringValue()
			if err != nil {
				t.Fatalf("result is not a string: %v", err)
			}
			if s != tc.want {
				t.Fatalf("f = %q, want %q", s, tc.want)
			}
		})
	}
}

// TestScriptListRoundTripsEveryValueKind pins that an element is carried through untouched. The
// kinds that matter are the awkward ones: a row can hold UNDEFINED or ERROR, and an aggregate's
// state has to be able to hold whatever Add was given.
func TestScriptListRoundTripsEveryValueKind(t *testing.T) {
	f := compileFunc(t, "def f(a):\n    s = {a}\n    return s[0]\n")
	for _, v := range []classad.Value{
		classad.NewIntValue(7), classad.NewRealValue(1.5), classad.NewStringValue("x"),
		classad.NewBoolValue(true), classad.NewUndefinedValue(), classad.NewErrorValue(),
		classad.NewListValue(ints(1, 2)),
	} {
		got := f.Call([]classad.Value{v})
		if got.String() != v.String() {
			t.Errorf("round trip of %s gave %s", v.String(), got.String())
		}
	}
}

// TestScriptListIsEagerOnlyWithoutAnAd pins the one place the two forms are distinguishable, in
// both directions, because it is the reason the eager lowering is gated rather than global.
//
// String coercion unparses a lazy list's element SOURCE and an eager list's element VALUES. Every
// other built-in that consumes a list materializes the elements first and cannot tell them apart,
// which the second half of this test states by checking the ones that could.
func TestScriptListIsEagerOnlyWithoutAnAd(t *testing.T) {
	const elements = "{1, 2+3}"

	f := compileFunc(t, "def f(a):\n    return string("+elements+")\n")
	eager, err := f.Call(ints(0)).StringValue()
	if err != nil {
		t.Fatalf("module result is not a string: %v", err)
	}
	if eager != "{ 1,5 }" {
		t.Fatalf("a module function coerced %s to %q, want the element values", elements, eager)
	}

	sp, err := ParseScript("result = string(" + elements + ")")
	if err != nil {
		t.Fatalf("ParseScript: %v", err)
	}
	lazy, err := sp.Run(classad.New()).StringValue()
	if err != nil {
		t.Fatalf("script result is not a string: %v", err)
	}
	if lazy != "{ 1,2 + 3 }" {
		t.Fatalf("a script coerced %s to %q, want the element source", elements, lazy)
	}

	// Everything else agrees, which is what makes the divergence one fact rather than a class of
	// them. The script's answer is the reference; the module function must match it.
	for _, fn := range []string{"size", "sum", "avg", "min", "max", "islist"} {
		mf := compileFunc(t, "def f(a):\n    return "+fn+"("+elements+")\n")
		msp, perr := ParseScript("result = " + fn + "(" + elements + ")")
		if perr != nil {
			t.Fatalf("ParseScript %s: %v", fn, perr)
		}
		got, want := mf.Call(ints(0)), msp.Run(classad.New())
		if got.String() != want.String() {
			t.Errorf("%s: a module function says %s, a script says %s", fn, got.String(), want.String())
		}
	}
}

// TestScriptListStillFacesThePurityGate pins that compiling the elements natively did not open a
// hole in the gate. Before this lowering a list literal was delegated and rule 4 refused it whole;
// now each element is compiled, so each element has to be caught by the rule that covers it.
func TestScriptListStillFacesThePurityGate(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"attribute in an element", "def f(a):\n    return {Cpus, a}\n", "reads the attribute Cpus"},
		{"scoped read in an element", "def f(a):\n    return {MY.Cpus, a}\n", "reads MY.Cpus"},
		{"nondeterministic element", "def f(a):\n    return {random(3), a}\n", "random(3)"},
		{"ad literal element", "def f(a):\n    return {[x = a], 1}\n", `reads the local "a"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseScriptModule(tc.src)
			if err != nil {
				t.Fatalf("ParseScriptModule: %v", err)
			}
			f, cerr := m.Compile("f")
			if cerr == nil {
				t.Fatalf("Compile succeeded (arity %d), want a refusal", f.Arity())
			}
			if !strings.Contains(cerr.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", cerr, tc.want)
			}
		})
	}
}

// TestScriptListRefusalSaysWhatActuallyHelps pins the advice in the refusal a SCRIPT still gets,
// because the advice it used to give does not work: binding the construct to a local is refused in
// exactly the same place, the elements being what reads the local.
func TestScriptListRefusalSaysWhatActuallyHelps(t *testing.T) {
	direct, err := ParseScript("x = 1\nresult = {x, 2}")
	if err == nil {
		t.Fatalf("ParseScript accepted a list literal reading a local: %v", direct)
	}
	bound, berr := ParseScript("x = 1\ny = {x, 2}\nresult = y")
	if berr == nil {
		t.Fatalf("ParseScript accepted the binding workaround: %v", bound)
	}
	// Both refusals are the same one, which is the fact the old advice contradicted. A script's
	// parser reports it with a line and column rather than as the typed error, so the comparison is
	// over the sentence; TestScriptLocalCaptureIsTyped covers the typed form through the builder.
	for _, e := range []error{err, berr} {
		if !strings.Contains(e.Error(), `reads the local "x"`) {
			t.Fatalf("%v is not the local-capture refusal", e)
		}
		if !strings.Contains(e.Error(), "does not help") {
			t.Errorf("the refusal still offers the workaround that does not work: %q", e)
		}
	}
}

// TestMakeListRunsInTheStackForm covers the stack interpreter's OpMakeList, which no compiled
// module function reaches -- a ScriptFunc runs the register form. Both forms implement every
// opcode, and an implementation with no test is one that can quietly stop matching the other.
func TestMakeListRunsInTheStackForm(t *testing.T) {
	// NESTED, because that is what makes the stack aliasing visible: the inner list is built in
	// the slots the outer one then writes over, so handing out a slice of the stack rather than a
	// copy would corrupt a list that has already been produced.
	expr, err := parser.ParseExpr("{{1, 2+3}, \"x\"}")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	c := &compiler{p: &Program{}, noAd: true}
	c.emit(expr)
	stackVal, _, _, ok := exec(c.p, classad.NewEvaluator(nil), nil, DefaultStepBudget)
	if !ok {
		t.Fatal("the stack form ran out of budget")
	}
	regVal, rok, _, _ := execReg(LowerToRegisters(c.p), classad.NewEvaluator(nil),
		make([]classad.Value, LowerToRegisters(c.p).nregs), DefaultStepBudget)
	if !rok {
		t.Fatal("the register form ran out of budget")
	}
	if stackVal.String() != regVal.String() {
		t.Fatalf("the two forms disagree: stack %s, register %s", stackVal.String(), regVal.String())
	}
	if got := stackVal.String(); got != `[[1 5] "x"]` {
		t.Fatalf("OpMakeList produced %s", got)
	}
}

// TestMakeListIsCharged keeps OpMakeList inside the step budget.
//
// The corpus-driven tripwire (TestEveryOpcodeIsClassified) cannot reach it: it walks the opcodes an
// EXPRESSION corpus and a few scripts emit, and neither compiles a module function, which is the
// only thing that emits this one. So the classification is asserted here directly. An uncharged
// instruction in a loop body is an execution that does not terminate, which is the failure mode
// worth a test of its own rather than a mask nobody reads.
func TestMakeListIsCharged(t *testing.T) {
	if !isValueOp(OpMakeList) {
		t.Error("OpMakeList is not a charged value operation, so a loop building lists need not terminate")
	}
	if !isRegValueOp(RMakeList) {
		t.Error("RMakeList is not a charged value operation, so a loop building lists need not terminate")
	}
	if !isClassified(OpMakeList) || !isRegClassified(RMakeList) {
		t.Error("the opcode is in neither mask")
	}

	// And it really is charged when it runs, in both forms, by the same count: the two forms must
	// agree on what an expression costs or a divergence between them cannot be attributed.
	expr, err := parser.ParseExpr("{1, 2}")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	c := &compiler{p: &Program{}, noAd: true}
	c.emit(expr)
	rp := LowerToRegisters(c.p)
	_, _, stackLeft, ok := exec(c.p, classad.NewEvaluator(nil), nil, DefaultStepBudget)
	if !ok {
		t.Fatal("the stack form ran out of budget")
	}
	_, rok, _, regLeft := execReg(rp, classad.NewEvaluator(nil), make([]classad.Value, rp.nregs), DefaultStepBudget)
	if !rok {
		t.Fatal("the register form ran out of budget")
	}
	if stackLeft == DefaultStepBudget {
		t.Error("building a list cost nothing")
	}
	if stackLeft != regLeft {
		t.Errorf("the two forms charge differently: %d steps left against %d", stackLeft, regLeft)
	}
}

// TestOptimizerHandlesMakeList covers the optimizer's two cases for RMakeList: the use list that
// keeps dead-code elimination from deleting what an element reads, and the rewrite that follows a
// copy it elided.
//
// Nothing on the module-function path runs Optimize today -- a ScriptFunc is lowered and run
// unoptimized -- so without this test both cases are unreachable code that the first caller to
// optimize such a program would find wrong. Optimize is exported, so that caller can be anybody.
//
// The fixture is a COMPILED FUNCTION rather than a bare expression, because the copy this exercises
// is the one a parameter read lowers to: `{a, b}` loads each parameter into a temporary, and copy
// propagation elides those temporaries and has to follow them into the list's operand list.
func TestOptimizerHandlesMakeList(t *testing.T) {
	f := compileFunc(t, "def f(a, b):\n    return {a, b}\n")
	args := ints(3, 4)
	run := func(t *testing.T, what string, rp *RegProgram) string {
		t.Helper()
		regs := make([]classad.Value, rp.nregs)
		for i, r := range f.paramRegs {
			regs[r] = args[i]
		}
		v, ok, _, _ := execReg(rp, classad.NewEvaluator(nil), regs, DefaultStepBudget)
		if !ok {
			t.Fatalf("the %s program ran out of budget", what)
		}
		return v.String()
	}
	before := run(t, "unoptimized", f.sp.rp)
	if before != "[3 4]" {
		t.Fatalf("the fixture produced %s, not the list of its parameters", before)
	}
	if after := run(t, "optimized", Optimize(f.sp.rp, AllPasses())); after != before {
		t.Fatalf("optimizing changed the answer: %s became %s", before, after)
	}
}
