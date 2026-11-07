package vm

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// runScript parses, compiles and runs src against ad, failing the test on any refusal.
func runScript(t *testing.T, src, ad string) classad.Value {
	t.Helper()
	sp, err := ParseScript(src)
	if err != nil {
		t.Fatalf("compile:\n%s\n%v", src, err)
	}
	scope, err := classad.Parse(ad)
	if err != nil {
		t.Fatalf("parse ad %q: %v", ad, err)
	}
	return sp.Run(scope)
}

// refuses compiles src and returns the refusal, failing the test if there is none.
func refuses(t *testing.T, src string) string {
	t.Helper()
	sp, err := ParseScript(src)
	if err == nil {
		t.Fatalf("expected a refusal, got a program (%s) for:\n%s", sp, src)
	}
	return err.Error()
}

func wants(t *testing.T, got classad.Value, want string) {
	t.Helper()
	if got.String() != want {
		t.Errorf("got %s, want %s", got.String(), want)
	}
}

// TestScriptFuncCapsAnArgument is the worked example: a function of two parameters, three returns
// and a branch, called from a binding.
func TestScriptFuncCapsAnArgument(t *testing.T) {
	src := `def over_cap(requested, cap):
    if requested is undefined:
        return cap
    if requested > cap:
        return cap
    return requested

want = over_cap(MY.RequestMemory, 8192)
result = want
`
	wants(t, runScript(t, src, `[RequestMemory = 100]`), "100")
	wants(t, runScript(t, src, `[RequestMemory = 99999]`), "8192")
	wants(t, runScript(t, src, `[Other = 1]`), "8192")
}

// TestScriptFuncIsNotPure pins the part a reader will get wrong. A bare name the body does not bind
// is an ATTRIBUTE of the subject ad, so a function with no parameters at all is still a function of
// the ad -- and that is the point, not an accident.
func TestScriptFuncIsNotPure(t *testing.T) {
	src := `def big():
    return RequestMemory > 4096

result = big()
`
	wants(t, runScript(t, src, `[RequestMemory = 8192]`), "true")
	wants(t, runScript(t, src, `[RequestMemory = 1]`), "false")
	wants(t, runScript(t, src, `[]`), "undefined")
}

// TestScriptFuncBodyIsFunctionScoped pins that the caller's locals are invisible inside a body: a
// bare name there is a parameter, a name the body binds, or an attribute -- never a caller local.
func TestScriptFuncBodyIsFunctionScoped(t *testing.T) {
	src := `def peek():
    return hidden

hidden = 99
result = peek()
`
	// `hidden` in the body is the ATTRIBUTE, which this ad has, and not the local bound above.
	wants(t, runScript(t, src, `[hidden = 7]`), "7")
	wants(t, runScript(t, src, `[Other = 1]`), "undefined")

	// And the caller's local survives the call unchanged, even when the body binds the same name.
	src2 := `def shadow():
    hidden = 1
    return hidden

outer = 99
inner = shadow()
result = outer + inner
`
	wants(t, runScript(t, src2, `[]`), "100")
}

// TestScriptFuncArgumentsAreEvaluatedOnceInOrder pins call by value: left to right, exactly once
// each, before the body runs -- and evaluated even where the parameter is never read.
//
// The trace is a registered UDF rather than an inferred one, because "was this evaluated" is not
// something a value can report: an unused argument's value is discarded by definition, so only a
// side effect can tell the difference between evaluating it and not.
func TestScriptFuncArgumentsAreEvaluatedOnceInOrder(t *testing.T) {
	registerTraceUDF(t)

	traceReset()
	wants(t, runScript(t, `def both(a, b):
    return a + b

result = both(zzTraceBB(1), zzTraceBB(2))
`, `[]`), "3")
	if got := traceSeen(); got != "1,2" {
		t.Errorf("argument trace %q, want \"1,2\" (left to right, once each)", got)
	}

	traceReset()
	wants(t, runScript(t, `def ignore(a):
    return 7

result = ignore(zzTraceBB(5))
`, `[]`), "7")
	if got := traceSeen(); got != "5" {
		t.Errorf("unused argument trace %q, want \"5\": every argument is evaluated", got)
	}

	// A parameter read twice is still evaluated once: the argument lands in a register.
	traceReset()
	wants(t, runScript(t, `def twice(a):
    return a + a

result = twice(zzTraceBB(4))
`, `[]`), "8")
	if got := traceSeen(); got != "4" {
		t.Errorf("trace %q, want \"4\": a parameter read twice is one evaluation", got)
	}
}

// TestScriptFuncNoParameters pins that an empty parameter list is a function and not a syntax hole.
func TestScriptFuncNoParameters(t *testing.T) {
	wants(t, runScript(t, "def four():\n    return 4\n\nresult = four() + four()\n", `[]`), "8")
}

// TestScriptFuncFallsOffTheEnd pins both ways a body produces UNDEFINED.
func TestScriptFuncFallsOffTheEnd(t *testing.T) {
	wants(t, runScript(t, `def nothing(a):
    if a > 0:
        return 1

result = nothing(-1)
`, `[]`), "undefined")

	wants(t, runScript(t, `def bare(a):
    if a > 0:
        return 1
    return

result = bare(-1)
`, `[]`), "undefined")
}

// TestScriptFuncCallBeforeDefinition pins that a definition is visible to the whole script, not
// only to the lines below it.
func TestScriptFuncCallBeforeDefinition(t *testing.T) {
	wants(t, runScript(t, "x = twice(21)\nresult = x\n\ndef twice(v):\n    return v + v\n", `[]`), "42")
}

// TestScriptFuncCallsFunction pins that a body may call another function, which is what makes the
// expansion bound matter.
func TestScriptFuncCallsFunction(t *testing.T) {
	src := `def dbl(v):
    return v * 2

def quad(v):
    return dbl(dbl(v))

result = quad(3)
`
	wants(t, runScript(t, src, `[]`), "12")
}

// TestScriptFuncInALoopResetsItsRegisters pins that two runs of one expansion are two independent
// calls.
//
// Registers belong to the program, not to a call, so an expansion inside a while body writes the
// same registers every trip. A call that returns nothing this time round must still be UNDEFINED
// and not whatever it returned last time -- which is exactly what a program-start initializer
// cannot provide, since it runs once.
func TestScriptFuncInALoopResetsItsRegisters(t *testing.T) {
	// pick(1) returns 1 on the first trip; pick(0) returns nothing on the second.
	src := `def pick(v):
    if v > 0:
        return 1

i = 2
last = 0
while i > 0:
    last = pick(i - 1)
    i = i - 1
result = last
`
	wants(t, runScript(t, src, `[]`), "undefined")

	// The same for a body LOCAL bound on one path only.
	src2 := `def pick(v):
    if v > 0:
        y = 9
    return y

i = 2
last = 0
while i > 0:
    last = pick(i - 1)
    i = i - 1
result = last
`
	wants(t, runScript(t, src2, `[]`), "undefined")
}

// TestScriptFuncTwoCallsAreIndependent pins that two expansions of one function do not share
// registers.
func TestScriptFuncTwoCallsAreIndependent(t *testing.T) {
	src := `def addOne(v):
    t = v + 1
    return t

result = addOne(10) * 100 + addOne(1)
`
	wants(t, runScript(t, src, `[]`), "1102")
}

// --------------------------------------------------------------------------
// The guards. Each one is a refusal, and each refusal has to say enough for the
// author to act on it.
// --------------------------------------------------------------------------

// TestScriptFuncMayNotShadowABuiltin pins guard one.
func TestScriptFuncMayNotShadowABuiltin(t *testing.T) {
	msg := refuses(t, "def strcat(a, b):\n    return 1\n\nresult = 1\n")
	if !strings.Contains(msg, "already a built-in") {
		t.Errorf("refusal %q does not say the name is a built-in", msg)
	}
	// Case does not matter, because a call does not care either.
	msg = refuses(t, "def TOUPPER(a):\n    return 1\n\nresult = 1\n")
	if !strings.Contains(msg, "already a built-in") {
		t.Errorf("refusal %q does not say the name is a built-in", msg)
	}
}

// TestScriptFuncMayNotShadowAUDF pins the other half of guard one: a registered user-defined
// function is as much a taken name as a built-in, and for the same reason.
func TestScriptFuncMayNotShadowAUDF(t *testing.T) {
	registerTraceUDF(t)
	msg := refuses(t, "def zzTraceBB(a):\n    return 1\n\nresult = 1\n")
	if !strings.Contains(msg, "already a built-in or a registered function") {
		t.Errorf("refusal %q does not say the name is taken", msg)
	}
}

// TestScriptFuncRefusesRecursion pins guard two, and that the message names the cycle.
//
// The self-call here has work left after it, which is what makes it an expansion that has no end. A
// self-call in TAIL position is a back edge to the function's own entry and compiles; see
// scripttail_test.go, which pins every other position against this refusal one at a time.
func TestScriptFuncRefusesRecursion(t *testing.T) {
	msg := refuses(t, "def f(v):\n    return f(v) + 1\n\nresult = f(1)\n")
	for _, want := range []string{"recursive", "f -> f"} {
		if !strings.Contains(msg, want) {
			t.Errorf("direct recursion refusal %q lacks %q", msg, want)
		}
	}

	msg = refuses(t, `def a(v):
    return b(v)

def b(v):
    return a(v)

result = a(1)
`)
	if !strings.Contains(msg, "a -> b -> a") {
		t.Errorf("mutual recursion refusal %q does not name the cycle", msg)
	}
}

// TestScriptFuncRefusesWrongArgumentCount pins guard three, including both counts.
func TestScriptFuncRefusesWrongArgumentCount(t *testing.T) {
	msg := refuses(t, "def f(a, b):\n    return a\n\nresult = f(1)\n")
	for _, want := range []string{"f", "takes 2", "passes 1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q lacks %q", msg, want)
		}
	}
}

// TestScriptFuncRefusesACallInADelegatedSubtree pins guard four.
//
// A ternary, a list literal, an ad literal, a select and a lazy built-in are all evaluated by the
// tree-walker against the ad, which resolves a function name against the built-ins and the UDF
// registry. A script's own function is in neither, so the call would evaluate to ERROR -- quietly,
// as a value, with nothing to say why.
//
// A subscript is not among them: it compiles to an element load whose operands are ordinary
// compiled expressions, so a call inside one is expanded like any other. See
// TestSubscriptCarriesAScriptFunctionCall.
func TestScriptFuncRefusesACallInADelegatedSubtree(t *testing.T) {
	cases := []struct{ name, src string }{
		{"conditional", "result = f(1) ? 2 : 3\n"},
		{"list literal", "result = {f(1), 2}\n"},
		{"lazy built-in", "result = eval(f(1))\n"},
		{"ad literal", "result = [a = f(1)]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := refuses(t, "def f(v):\n    return v\n\n"+tc.src)
			if !strings.Contains(msg, "calls f") || !strings.Contains(msg, "evaluated against the ad") {
				t.Errorf("refusal %q does not explain the delegated call", msg)
			}
		})
	}
}

// TestScriptFuncRefusesMalformedDefinitions pins guard five: the ways a definition can be written
// that have no meaning, each with a position.
func TestScriptFuncRefusesMalformedDefinitions(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"duplicate parameter", "def f(a, a):\n    return a\n\nresult = f(1, 2)\n", "two parameters named"},
		{"duplicate definition", "def f(a):\n    return a\n\ndef f(b):\n    return b\n\nresult = f(1)\n", "defined twice"},
		{"return outside a function", "return 1\nresult = 1\n", "outside a function body: the script's value is 'result'"},
		{"def inside an if", "if MY.x:\n    def f(a):\n        return a\nresult = 1\n", "must be a top-level statement"},
		{"def inside a def", "def f(a):\n    def g(b):\n        return b\n    return a\n\nresult = f(1)\n", "must be a top-level statement"},
		{"no parameter list", "def f:\n    return 1\n\nresult = f()\n", "expected a function name and a parameter list"},
		{"parameter is not a name", "def f(1):\n    return 1\n\nresult = f(1)\n", "expected a parameter name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := refuses(t, tc.src)
			if !strings.Contains(msg, tc.want) {
				t.Errorf("refusal %q lacks %q", msg, tc.want)
			}
			if !strings.HasPrefix(msg, "line ") {
				t.Errorf("refusal %q has no position", msg)
			}
		})
	}
}

// TestScriptFuncRefusesRunawayExpansion pins guard six, with the shape that makes a bound necessary:
// each function calls the one below it twice, so nine readable definitions expand into hundreds of
// copies of the innermost body.
func TestScriptFuncRefusesRunawayExpansion(t *testing.T) {
	var b strings.Builder
	b.WriteString("def f0(v):\n    return v + 1\n\n")
	for i := 1; i <= 8; i++ {
		fmt.Fprintf(&b, "def f%d(v):\n    return f%d(v) + f%d(v)\n\n", i, i-1, i-1)
	}
	b.WriteString("result = f8(1)\n")
	msg := refuses(t, b.String())
	if !strings.Contains(msg, "function calls expanded") {
		t.Errorf("refusal %q does not name the expansion bound", msg)
	}

	// One step smaller still compiles, so the bound is a bound and not a blanket refusal.
	var ok strings.Builder
	ok.WriteString("def f0(v):\n    return v + 1\n\n")
	for i := 1; i <= 6; i++ {
		fmt.Fprintf(&ok, "def f%d(v):\n    return f%d(v) + f%d(v)\n\n", i, i-1, i-1)
	}
	ok.WriteString("result = f6(1)\n")
	wants(t, runScript(t, ok.String(), `[]`), "128")
}

// --------------------------------------------------------------------------
// The builder API, which the parser lowers onto and which a caller may use directly.
// --------------------------------------------------------------------------

func TestScriptFuncBuilder(t *testing.T) {
	must := func(src string) ast.Expr {
		t.Helper()
		e, err := parser.ParseExpr(src)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	// The call is written BEFORE the definition, which the builder allows because nothing is
	// compiled until Compile.
	sp, err := NewScript().
		Result(&ast.FunctionCall{Name: "half", Args: []ast.Expr{must("Cpus")}}).
		Func("half", []string{"v"}, func(s *Script) { s.Return(must("v / 2")) }).
		Compile()
	if err != nil {
		t.Fatal(err)
	}
	ad, err := classad.Parse(`[Cpus = 8]`)
	if err != nil {
		t.Fatal(err)
	}
	wants(t, sp.Run(ad), "4")
}

func TestScriptFuncBuilderRefusals(t *testing.T) {
	must := func(src string) ast.Expr {
		e, _ := parser.ParseExpr(src)
		return e
	}
	t.Run("Return outside a body", func(t *testing.T) {
		_, err := NewScript().Return(must("1")).Result(must("1")).Compile()
		if err == nil || !strings.Contains(err.Error(), "outside a function body") {
			t.Errorf("got %v, want a refusal naming the misplaced return", err)
		}
	})
	t.Run("nil body", func(t *testing.T) {
		_, err := NewScript().Func("f", nil, nil).Result(must("1")).Compile()
		if err == nil || !strings.Contains(err.Error(), "has no body") {
			t.Errorf("got %v, want a refusal naming the missing body", err)
		}
	})
	t.Run("name no call can spell", func(t *testing.T) {
		_, err := NewScript().Func("not a name", nil, func(*Script) {}).Result(must("1")).Compile()
		if err == nil || !strings.Contains(err.Error(), "not a name a call can spell") {
			t.Errorf("got %v, want a refusal about the name", err)
		}
	})
}

// --------------------------------------------------------------------------
// The reduction law, for functions.
// --------------------------------------------------------------------------

// funcReduction checks one instance of the law for a straight-line function:
//
//	def f(zzLocalA, zzLocalB):
//	    return <tmpl with the parameters>
//	result = f(<x>, <y>)      ==      Eval(<tmpl with (x) and (y) substituted>)
//
// Which is the SAME law the bindings obey, asked of the other mechanism: inlining a call must equal
// substituting the body. The parameters reuse the binding law's names so the templates and the
// precondition -- that the operands do not themselves mention them -- carry over unchanged.
func funcReduction(t *testing.T, scope *classad.ClassAd, x, y, tmpl string) {
	t.Helper()
	if capturesLocal(x, y) {
		return
	}
	xe, err1 := parser.ParseExpr(x)
	ye, err2 := parser.ParseExpr(y)
	be, err3 := parser.ParseExpr(tmpl)
	resSrc := strings.NewReplacer(localA, "("+x+")", localB, "("+y+")").Replace(tmpl)
	inlined, err4 := parser.ParseExpr(resSrc)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || xe == nil || ye == nil || be == nil || inlined == nil {
		return
	}
	sp, err := NewScript().
		Func("zzFnBB", []string{localA, localB}, func(s *Script) { s.Return(be) }).
		Result(&ast.FunctionCall{Name: "zzFnBB", Args: []ast.Expr{xe, ye}}).
		Compile()
	if err != nil {
		var capture *LocalCaptureError
		if errors.As(err, &capture) {
			// The body delegates a subtree that reads a parameter, which is refused rather than
			// computed; the law has nothing to say about a script that does not compile.
			return
		}
		t.Fatalf("compile {def f(%s,%s): return %s; f(%s,%s)}: %v", localA, localB, tmpl, x, y, err)
	}
	got := sp.Run(scope)
	want := refEval(scope, inlined)
	if !valuesEqual(want, got) {
		t.Errorf("def f(%s,%s): return %s  called f(%s, %s) = %s, inlined %q = %s",
			localA, localB, tmpl, x, y, describe(got), resSrc, describe(want))
	}
}

// TestScriptFuncReductionCorpus is the function reduction law over the binding law's corpus.
func TestScriptFuncReductionCorpus(t *testing.T) {
	for _, scopeSrc := range reductionScopes {
		scope, err := classad.Parse(scopeSrc)
		if err != nil {
			t.Fatalf("parse scope: %v", err)
		}
		for _, x := range reductionOperands {
			for _, y := range reductionOperands {
				for _, tmpl := range reductionTemplates {
					funcReduction(t, scope, x, y, tmpl)
				}
			}
		}
	}
}

// FuzzScriptFuncReduction fuzzes the arguments of the same law.
func FuzzScriptFuncReduction(f *testing.F) {
	for _, x := range reductionOperands {
		for _, y := range reductionOperands {
			f.Add(x, y)
		}
	}
	scope, err := classad.Parse(reductionScopes[0])
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, x, y string) {
		for _, tmpl := range reductionTemplates {
			funcReduction(t, scope, x, y, tmpl)
		}
	})
}

// --------------------------------------------------------------------------
// Vectorization and the planner's read set.
// --------------------------------------------------------------------------

// TestVecScriptFuncStraightLineStillVectorizes pins that a function with no control flow adds none:
// a call becomes argument stores, the body's instructions and a load, all of which the vector
// executor already serves. A script that was servable before the function was factored out of it
// must still be servable after.
func TestVecScriptFuncStraightLineStillVectorizes(t *testing.T) {
	src := vecCorpus()
	n := len(src.recs)
	var scratch VecScratch
	for _, s := range []string{
		"def f(v):\n    return v + 1\n\nt = f(A)\nresult = t > 4\n",
		"def f(v, w):\n    return v * w\n\nresult = f(A, B) + f(B, A)\n",
		"def f(v):\n    m = v + B\n    return m\n\nresult = f(A)\n",
		"def f():\n    return A\n\nresult = f() > B\n",
	} {
		sp, err := ParseScript(s)
		if err != nil {
			t.Fatalf("compile %q: %v", s, err)
		}
		got, ok := sp.VecEval(src, n, &scratch)
		if !ok {
			t.Errorf("declined: %q", s)
			continue
		}
		for i := 0; i < n; i++ {
			if want := sp.Run(src.ad(i)); !sameValue(got.value(i), want) {
				t.Errorf("%q record %d: vector %s != scalar %s", s, i, got.value(i), want)
			}
		}
	}
}

// TestScriptFuncReadSetExcludesParameters pins that a parameter and a body local are not attributes.
// A planner told otherwise looks for a column that does not exist.
func TestScriptFuncReadSetExcludesParameters(t *testing.T) {
	sp, err := ParseScript(`def f(requested):
    scratch = requested + Cpus
    return scratch

result = f(Memory)
`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, n := range sp.Program().ReadAttrs() {
		got[strings.ToLower(n)] = true
	}
	for _, want := range []string{"cpus", "memory"} {
		if !got[want] {
			t.Errorf("read set %v is missing the attribute %q", sp.Program().ReadAttrs(), want)
		}
	}
	for _, unwanted := range []string{"requested", "scratch"} {
		if got[unwanted] {
			t.Errorf("read set %v contains %q, which is not an attribute", sp.Program().ReadAttrs(), unwanted)
		}
	}
}

// --------------------------------------------------------------------------
// A UDF that records the arguments it was handed, for the tests that have to tell
// "evaluated" from "not evaluated".
// --------------------------------------------------------------------------

var (
	traceMu   sync.Mutex
	traceLog  []string
	traceOnce sync.Once
)

func registerTraceUDF(t *testing.T) {
	t.Helper()
	traceOnce.Do(func() {
		err := classad.RegisterFunction("zzTraceBB", 1, 1, func(args []classad.Value) classad.Value {
			traceMu.Lock()
			traceLog = append(traceLog, args[0].String())
			traceMu.Unlock()
			return args[0]
		})
		if err != nil {
			t.Fatalf("register zzTraceBB: %v", err)
		}
	})
}

func traceReset() {
	traceMu.Lock()
	traceLog = nil
	traceMu.Unlock()
}

func traceSeen() string {
	traceMu.Lock()
	defer traceMu.Unlock()
	return strings.Join(traceLog, ",")
}

// TestScriptFuncInEveryExpressionPosition pins the assumption the whole mechanism rests on: an
// expansion sits in the middle of an enclosing expression's operands, so the statements it emits
// run with the operand stack NOT empty. A branch's arms have to reach their merge with the same
// stack, and the lowering has to end with exactly one value on it.
//
// The positions that make that non-trivial are collected here: a loop inside a body, a call in a
// condition, a call in each arm of an if, and a call as the right operand of a short-circuiting
// operator, where the expansion lives inside the region the short circuit jumps over.
func TestScriptFuncInEveryExpressionPosition(t *testing.T) {
	t.Run("return out of a while in the body", func(t *testing.T) {
		wants(t, runScript(t, `def firstOver(limit):
    i = 1
    while i < 1000:
        if i > limit:
            return i
        i = i * 2
    return 0

result = firstOver(10)
`, `[]`), "16")
	})
	t.Run("call in a while condition", func(t *testing.T) {
		wants(t, runScript(t, `def small(v):
    return v < 4

n = 1
while small(n):
    n = n + 1
result = n
`, `[]`), "4")
	})
	t.Run("call in an if condition and both arms", func(t *testing.T) {
		wants(t, runScript(t, `def d(v):
    return v * 2

x = 0
if d(2) > 3:
    x = d(5)
else:
    x = d(1)
result = x
`, `[]`), "10")
	})
	t.Run("call as a short-circuit right operand", func(t *testing.T) {
		src := `def t(v):
    if v > 0:
        return true
    return false

result = t(FirstOne) && t(-1)
`
		wants(t, runScript(t, src, `[FirstOne = 1]`), "false")
		// The left operand settles the answer, so the right -- and the whole expansion inside it --
		// is jumped over.
		wants(t, runScript(t, src, `[FirstOne = -1]`), "false")
	})
	t.Run("body rebinds a parameter", func(t *testing.T) {
		wants(t, runScript(t, `def clamp(v):
    if v < 0:
        v = 0
    return v

result = clamp(-5) + clamp(3)
`, `[]`), "3")
	})
}
