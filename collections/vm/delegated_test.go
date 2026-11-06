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

// TestDelegatedReadsLocal is the binding model inside a delegated subtree, one case per rule. Each
// of these is a case a resolver gets wrong, so each is written out rather than folded into a
// generator.
func TestDelegatedReadsLocal(t *testing.T) {
	cases := []struct {
		name string
		src  string
		ad   string
		want string
	}{
		// A BARE BOUND NAME IS THE LOCAL. The ternary is delegated, and x is the register.
		{
			"ternary reads the local",
			"x = Cpus + 1\nresult = x ? 2 : 3\n",
			`[ Cpus = 0; x = false ]`,
			"2", // the local is 1 (true); the ATTRIBUTE x is false, which would give 3
		},
		// MY.x IS ALWAYS THE ATTRIBUTE, inside a delegated subtree as everywhere else.
		{
			"MY.x is the attribute",
			"x = Cpus + 1\nresult = MY.x ? 2 : 3\n",
			`[ Cpus = 0; x = false ]`,
			"3",
		},
		// AND BESIDE THE LOCAL IN ONE SUBTREE, which is the case the overlay is actually
		// installed for: the bare x answers from the register, MY.x from the ad.
		{
			"MY.x beside the local",
			"x = Cpus + 1\nresult = x ? MY.x : 0\n",
			`[ Cpus = 0; x = 77 ]`,
			"77",
		},
		// TARGET and PARENT are scoped too, so they name an ad and not a register.
		{
			"TARGET.x beside the local",
			"x = 1\nresult = x ? TARGET.x : 0\n",
			`[ Cpus = 0 ]`,
			"undefined", // no target, so TARGET.x is undefined -- it is not the local, which is 1
		},
		{
			"TARGET.x is the attribute",
			"x = 1\nresult = TARGET.x ? 2 : 3\n",
			`[ Cpus = 0 ]`,
			// No target, so the condition is undefined and the ternary is undefined -- which is
			// still not the local, which is 1.
			"undefined",
		},
		// `x = x + 1` READS THE ATTRIBUTE AND BINDS THE LOCAL, and the delegated read afterwards is
		// the local.
		{
			"rebinding reads the attribute then the local",
			"x = x + 1\nresult = x ? x : 0\n",
			`[ x = 10 ]`,
			"11",
		},
		// A local the delegated subtree does not mention leaves the attribute alone.
		{
			"an unmentioned local does not shadow",
			"y = 5\nresult = x ? 2 : 3\n",
			`[ x = true ]`,
			"2",
		},
		// Case folding: a local is matched case-insensitively, as every ClassAd name is.
		{
			"case-insensitive",
			"XY = 7\nresult = xy ? 2 : 3\n",
			`[ xy = false ]`,
			"2",
		},
		// A SELECT and a LAZY BUILT-IN are delegated too, and read the local the same way.
		{
			"lazy builtin reads the local",
			"x = \"ab\"\nresult = strcat(x, \"c\")\n",
			`[ x = "zz" ]`,
			`"abc"`,
		},
		{
			"select over a local",
			"x = Nested\nresult = x.a\n",
			`[ Nested = [ a = 42 ] ]`,
			"42",
		},
		// Nested delegation: a ternary inside a lazy built-in's argument.
		{
			"nested delegated shapes",
			"x = 3\nresult = strcat(\"v\", x > 2 ? \"hi\" : \"lo\")\n",
			`[ x = 0 ]`,
			`"vhi"`,
		},
		// An ELVIS over a local: the local is the left operand and settles it.
		{
			"ifThenElse over a local",
			"x = Cpus\nresult = ifThenElse(x > 2, \"big\", \"small\")\n",
			`[ Cpus = 8 ]`,
			`"big"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp := mustScript(t, tc.src)
			if got := sp.Run(mustAd(t, tc.ad)).String(); got != tc.want {
				t.Errorf("%q over %s = %s, want %s", tc.src, tc.ad, got, tc.want)
			}
		})
	}
}

// TestDelegatedLocalIsFunctionScoped pins the one rule a function changes: a delegated subtree in a
// body sees the BODY's locals, never the caller's.
func TestDelegatedLocalIsFunctionScoped(t *testing.T) {
	// The body's `x` is its parameter, not the caller's local -- and the caller's local is 100, so
	// a leak would be visible in the value rather than only in the compiled shape.
	src := "def pick(x):\n" +
		"    return x ? \"param\" : \"no\"\n" +
		"x = 100\n" +
		"result = pick(0)\n"
	sp := mustScript(t, src)
	if got := sp.Run(mustAd(t, `[ x = 1 ]`)).String(); got != `"no"` {
		t.Errorf("result = %s, want \"no\": the body's x must be its parameter", got)
	}

	// A body that binds nothing of the name reads the ATTRIBUTE, not the caller's local. The
	// caller's local is 100 and the attribute is 0, so the two answers differ.
	src2 := "def peek():\n" +
		"    return x ? \"local\" : \"attr\"\n" +
		"x = 100\n" +
		"result = peek()\n"
	sp2 := mustScript(t, src2)
	if got := sp2.Run(mustAd(t, `[ x = 0 ]`)).String(); got != `"attr"` {
		t.Errorf("result = %s, want \"attr\": a body's free name is an attribute", got)
	}
}

// TestDelegatedResolverIsRemoved checks that the overlay belongs to the subtree that asked for it.
// A delegated node with no locals of its own must see the ad, whatever ran before it.
func TestDelegatedResolverIsRemoved(t *testing.T) {
	// peek() is expanded at the end, after the ternary that binds x has already run. Its `x` is an
	// attribute, so it must read 0 and not the caller's 100.
	src := "def peek():\n" +
		"    return x ? 7 : 8\n" +
		"x = 100\n" +
		"a = x ? 1 : 2\n" +
		"result = a + peek()\n"
	sp := mustScript(t, src)
	if got := sp.Run(mustAd(t, `[ x = 0 ]`)).String(); got != "9" {
		t.Errorf("result = %s, want 9 (1 + 8): the overlay must not outlive its own subtree", got)
	}
}

// TestDelegatedLocalReadSet checks the planner's view: a name the overlay answers is not a column.
func TestDelegatedLocalReadSet(t *testing.T) {
	// The ternary mentions the local x and the genuine attribute Memory.
	sp := mustScript(t, "x = Cpus + 1\nresult = x ? Memory : 0\n")
	got := sp.Program().ReadAttrs()
	want := []string{"Cpus", "Memory"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ReadAttrs() = %v, want %v", got, want)
	}

	// And the value is what the read set implies: the local decides the branch, the attribute
	// supplies the answer.
	if v := sp.Run(mustAd(t, `[ Cpus = 1; Memory = 512; x = false ]`)).String(); v != "512" {
		t.Errorf("result = %s, want 512", v)
	}
}

// TestDelegatedLocalBlockedPositions is the refusal that remains: three positions evaluate in an
// evaluator of their own, which no overlay installed on the caller's reaches.
func TestDelegatedLocalBlockedPositions(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"list literal", "x = Cpus\nresult = {x, 1}\n"},
		{"list literal nested in a ternary", "x = Cpus\nresult = Cpus ? {x} : {}\n"},
		{"ad literal", "x = Cpus\nresult = [a = x].a\n"},
		{"countMatches context", "x = Cpus\nresult = countMatches(x, MY.GPUs)\n"},
		{"evalInEachContext context", "x = Cpus\nresult = evalInEachContext(x, MY.GPUs)\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Through the parser the refusal arrives inside a *ScriptSyntaxError, which carries
			// the position; the sentence is the compiler's.
			_, err := ParseScript(tc.src)
			var se *ScriptSyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("ParseScript(%q) = %v, want a *ScriptSyntaxError", tc.src, err)
			}
			if !strings.Contains(se.Msg, `reads the local "x"`) {
				t.Errorf("message %q does not refuse the local read", se.Msg)
			}
		})
	}

	// The SECOND argument of countMatches is the list, evaluated in the calling scope, so a local
	// there is read like any other.
	sp := mustScript(t, "gpus = MY.GPUs\nresult = countMatches(Want, gpus)\n")
	ad := mustAd(t, `[ Want = (Cap > 4); GPUs = { [Cap = 8], [Cap = 1] } ]`)
	if got := sp.Run(ad).String(); got != "1" {
		t.Errorf("countMatches over a local list = %s, want 1", got)
	}
}

// TestDelegatedLazyListEscapes is the reason a list literal over a local is refused rather than
// computed, demonstrated at the level it happens: a lazy list carries its element EXPRESSIONS and
// the ad they were written in, and materializes them in an evaluator of its own. Nothing the
// interpreter installs is in force at that moment, and the moment may come after the script has
// finished and its registers are gone.
func TestDelegatedLazyListEscapes(t *testing.T) {
	ad := mustAd(t, `[ x = "attribute" ]`)
	ev := classad.NewEvaluator(ad)
	ev.SetPartialResolver(func(name string, scope ast.AttributeScope) (classad.Value, bool) {
		if scope == ast.NoScope && strings.EqualFold(name, "x") {
			return classad.NewStringValue("local"), true
		}
		return classad.Value{}, false
	})
	expr, err := parser.ParseExpr("{x}")
	if err != nil {
		t.Fatal(err)
	}
	list := ev.Evaluate(expr)
	// The overlay is gone, exactly as it is once the delegated evaluation returns.
	ev.SetPartialResolver(nil)
	elems, lerr := list.ListValue()
	if lerr != nil || len(elems) != 1 {
		t.Fatalf("ListValue() = %v, %v", elems, lerr)
	}
	if got := elems[0].String(); got != `"attribute"` {
		t.Fatalf("a list element materialized later = %s; if this ever reads the overlay, a list "+
			"literal over a local can be allowed instead of refused", got)
	}

	// Which is why the script form is refused rather than silently reading the attribute.
	if _, cerr := ParseScript("x = \"local\"\nresult = {x}\n"); cerr == nil {
		t.Error("a list literal over a local must be refused")
	}

	// A list literal over no local still works and still escapes: the refusal is about the local,
	// not about the list.
	sp := mustScript(t, "result = {Cpus, 2}\n")
	v := sp.Run(mustAd(t, `[ Cpus = 9 ]`))
	out, verr := v.ListValue()
	if verr != nil || len(out) != 2 || out[0].String() != "9" {
		t.Errorf("{Cpus, 2} = %v (%v)", out, verr)
	}
}

// TestDelegatedLocalBothInterpreters runs one straight-line script through both interpreters.
//
// They share evalDelegated, so this is less about catching an inconsistency than about making the
// sharing a fact a test holds down: the stack form indexes its locals by local and the register
// form by register, and the two tables must come out at the same values.
func TestDelegatedLocalBothInterpreters(t *testing.T) {
	sp := mustScript(t, "x = Cpus + 1\ny = x ? Memory : 0\nresult = y + x\n")
	ad := mustAd(t, `[ Cpus = 3; Memory = 100; x = false ]`)

	reg := sp.Run(ad)
	stackVal, _, _, ok := exec(sp.Program(), classad.NewEvaluator(ad), nil, DefaultStepBudget)
	if !ok {
		t.Fatal("the stack interpreter ran out of budget")
	}
	if !valuesEqual(reg, stackVal) {
		t.Errorf("register = %s, stack = %s", describe(reg), describe(stackVal))
	}
	if reg.String() != "104" {
		t.Errorf("result = %s, want 104", reg)
	}
}

// TestDelegatedLocalSurvivesOptimizer checks the optimizer's use table. A delegated subtree names no
// register in its instruction and still reads one, so dead-code elimination has to be told.
func TestDelegatedLocalSurvivesOptimizer(t *testing.T) {
	sp := mustScript(t, "x = Cpus + 1\nresult = x ? 2 : 3\n")
	ad := mustAd(t, `[ Cpus = 0; x = false ]`)
	want := sp.Run(ad)
	opt := Optimize(sp.RegProgram(), AllPasses())
	got := RunRegisters(opt, mustAd(t, `[ Cpus = 0; x = false ]`))
	if !valuesEqual(want, got) {
		t.Errorf("optimized = %s, unoptimized = %s", describe(got), describe(want))
	}
	if want.String() != "2" {
		t.Errorf("result = %s, want 2", want)
	}
}

// TestDelegatedLocalConcurrent runs one compiled program from many goroutines.
//
// A resolver is state on an Evaluator, and an Evaluator is not safe for concurrent use. What this
// says is that nothing installed here is shared: each run allocates its own evaluator and its own
// register file, and the closure the resolver is made of closes over those rather than over
// anything the program holds. Each goroutine parses its own ad, because a *classad.ClassAd is not
// safe to share either. Run under -race.
func TestDelegatedLocalConcurrent(t *testing.T) {
	sp := mustScript(t, "x = Cpus + 1\ny = x ? Memory : 0\nresult = strcat(\"n\", y > 50 ? \"hi\" : \"lo\")\n")
	const goroutines = 16
	const iterations = 200
	var wg sync.WaitGroup
	errs := make([]string, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				ad, err := classad.Parse(`[ Cpus = 3; Memory = 100; x = false ]`)
				if err != nil {
					errs[g] = err.Error()
					return
				}
				if got := sp.Run(ad).String(); got != `"nhi"` {
					errs[g] = fmt.Sprintf("goroutine %d iteration %d: %s", g, i, got)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	for _, e := range errs {
		if e != "" {
			t.Error(e)
		}
	}
}

// delegatedShapes are delegated subtrees that mention NO local. Every one of them must evaluate
// through a compiled script exactly as it does through the plain evaluator -- the overlay is a
// thing that happens to subtrees reading a local, and must be invisible to the rest.
var delegatedShapes = []string{
	// ternaries
	"Cpus ? 1 : 2",
	"Missing ? 1 : 2",
	"\"s\" ? 1 : 2",
	"Cpus > 2 ? Memory : Missing",
	// list literals
	"{}",
	"{1, 2, Cpus}",
	"{Cpus, Missing}",
	"{{1}, {2, Cpus}}",
	"{Cpus, 2}[1]",
	"sum({Cpus, Memory})",
	// ad literals
	"[a = 1].a",
	"[a = Cpus].a",
	"[a = 1; b = a + 1].b",
	// selects
	"Nested.a",
	"Missing.a",
	"Nested.missing",
	"{Nested, Nested}.a",
	// lazy built-ins
	"ifThenElse(Cpus > 2, \"big\", \"small\")",
	"ifThenElse(Missing, 1, 2)",
	"strcat(\"a\", Cpus)",
	"strcat(Missing, Cpus)",
	"eval(\"Cpus + 1\")",
	"unparse(Cpus)",
	"countMatches(Want, GPUs)",
	"evalInEachContext(Want, GPUs)",
	// unknown names and bad arities, which are delegated for their own reasons
	"nosuchfunction(Cpus)",
	"pow(Cpus)",
	// nested combinations
	"Cpus ? {1, Memory} : [a = 2].a",
	"strcat(\"v\", Cpus ? \"hi\" : \"lo\")",
	"ifThenElse(Cpus, {1, 2}, Nested.a)",
	"{Cpus ? 1 : 2, [a = 3].a}",
	// a cyclic reference, which the plain evaluator panics on and refEval recovers
	"Cyclic ? 1 : 2",
	"{Cyclic}",
}

// TestDelegatedParity is the differential: a delegated subtree that mentions no local evaluates
// exactly as the plain evaluator evaluates it, both as an expression program and as a script's
// result -- the two differ in whether nodeLocals exists at all.
func TestDelegatedParity(t *testing.T) {
	const adSrc = `[ Cpus = 4; Memory = 100; Nested = [ a = 7 ]; Want = (Cap > 4);
	                 GPUs = { [Cap = 8], [Cap = 1] }; Cyclic = Cyclic + 1 ]`
	for _, src := range delegatedShapes {
		t.Run(src, func(t *testing.T) {
			expr, err := parser.ParseExpr(src)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			want := refEval(mustAd(t, adSrc), expr)

			if got := Run(CompileProgram(expr), mustAd(t, adSrc)); !valuesEqual(want, got) {
				t.Errorf("expression program = %s, evaluator = %s", describe(got), describe(want))
			}

			sp, serr := ParseScript("result = (" + src + ")\n")
			if serr != nil {
				t.Fatalf("ParseScript: %v", serr)
			}
			if got := sp.Run(mustAd(t, adSrc)); !valuesEqual(want, got) {
				t.Errorf("script = %s, evaluator = %s", describe(got), describe(want))
			}

			// And again with a local bound but unmentioned, so the script carries a register file
			// the subtree does not read.
			sp2, s2err := ParseScript("unused = Cpus * 2\nresult = (" + src + ")\n")
			if s2err != nil {
				t.Fatalf("ParseScript with a local: %v", s2err)
			}
			if got := sp2.Run(mustAd(t, adSrc)); !valuesEqual(want, got) {
				t.Errorf("script with an unused local = %s, evaluator = %s", describe(got), describe(want))
			}
		})
	}
}

// TestDelegatedOverlayStopsAtAnAttributesValue pins the boundary of the overlay: it is the scope the
// script evaluates in, not a rewrite of the ad. `A = x + 1` means the ad's x wherever A is read.
func TestDelegatedOverlayStopsAtAnAttributesValue(t *testing.T) {
	sp := mustScript(t, "x = 1000\nresult = A ? A : -1\n")
	// A is x + 1 with the AD's x, which is 5, so A is 6 and the result is 6 -- not 1001.
	if got := sp.Run(mustAd(t, `[ x = 5; A = x + 1 ]`)).String(); got != "6" {
		t.Errorf("result = %s, want 6: an attribute's value resolves against the ad", got)
	}
}

// TestDelegatedEvalStringIgnoresTheOverlay pins that `eval` of a string means the same thing
// wherever it is written.
//
// The overlay answers names the compiler saw in the delegated subtree. It cannot see into a string
// parsed at run time, so without suppressing it `eval(s)` would resolve a name through the overlay
// only when the enclosing expression happened to mention that name somewhere else — the same call
// meaning the local in one expression and the attribute in another, for a reason the author of the
// eval cannot see.
func TestDelegatedEvalStringIgnoresTheOverlay(t *testing.T) {
	const ad = `[ x = 7 ]`
	// No overlay is installed here at all: nothing in the subtree names x.
	hidden := mustScript(t, "x = 42\ns = \"x\"\nresult = eval(s)\n").Run(mustAd(t, ad)).String()
	// An overlay IS installed here, because the ternary's condition names the local.
	beside := mustScript(t, "x = 42\ns = \"x\"\nresult = (x > 0 ? eval(s) : -1)\n").Run(mustAd(t, ad)).String()

	if hidden != "7" {
		t.Errorf("eval of a name the subtree does not mention = %s, want 7 (the attribute)", hidden)
	}
	if beside != "7" {
		t.Errorf("eval beside the local = %s, want 7: the overlay reached into a run-time string", beside)
	}
}
