package vm

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// pctModule is the worked example: a module exporting one function, used by several tests below.
const pctModule = `
def pctUsed(used, total):
    if used is undefined:
        return undefined
    if total is undefined:
        return undefined
    if total <= 0:
        return error
    return used * 100.0 / total
`

func compileOne(t *testing.T, src, name string) *ScriptFunc {
	t.Helper()
	m, err := ParseScriptModule(src)
	if err != nil {
		t.Fatalf("ParseScriptModule: %v", err)
	}
	f, err := m.Compile(name)
	if err != nil {
		t.Fatalf("Compile(%q): %v", name, err)
	}
	return f
}

func TestScriptModuleFunctions(t *testing.T) {
	m, err := ParseScriptModule("def a():\n    return 1\n\ndef b(x):\n    return x\n")
	if err != nil {
		t.Fatalf("ParseScriptModule: %v", err)
	}
	if got := m.Functions(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("Functions() = %v, want [a b]", got)
	}
	// A call folds case, so looking a function up does too.
	if f, err := m.Compile("B"); err != nil || f.Name() != "b" {
		t.Fatalf("Compile(%q) = %v, %v; want the function named b", "B", f, err)
	}
	if _, err := m.Compile("missing"); err == nil {
		t.Fatal("Compile of an unexported name succeeded")
	}
}

// TestScriptModuleRefusals covers what a module may not contain. Each case names the rule it is
// here to pin, because each is a separate guard.
func TestScriptModuleRefusals(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"empty", "", "empty"},
		{"comments_only", "# nothing here\n", "empty"},
		{"no_def", "x = 1\n", "only 'def' statements"},
		{"top_level_let", "def f():\n    return shared\n\nshared = 1\n", "only 'def' statements"},
		{"top_level_result", "def f():\n    return 1\n\nresult = f()\n", "no 'result'"},
		{"duplicate_def", "def f():\n    return 1\n\ndef F():\n    return 2\n", "defined twice"},
		{"builtin_name", "def strcat(a, b):\n    return a\n", "built-in function"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseScriptModule(tc.src)
			if err == nil {
				t.Fatalf("ParseScriptModule(%q) succeeded, want a refusal", tc.src)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
	if _, err := ParseScriptModule(""); !errors.Is(err, ErrNoFunctions) {
		t.Fatalf("empty module error = %v, want ErrNoFunctions", err)
	}
}

func TestScriptFuncCall(t *testing.T) {
	f := compileOne(t, pctModule, "pctUsed")
	if f.Arity() != 2 {
		t.Fatalf("Arity() = %d, want 2", f.Arity())
	}
	cases := []struct {
		name string
		args []classad.Value
		want string
	}{
		{"numbers", []classad.Value{classad.NewIntValue(3), classad.NewIntValue(8)}, "37.5"},
		{"undefined_arg", []classad.Value{classad.NewIntValue(3), classad.NewUndefinedValue()}, "undefined"},
		{"error_arg", []classad.Value{classad.NewErrorValue(), classad.NewIntValue(8)}, "error"},
		{"division_refused", []classad.Value{classad.NewIntValue(3), classad.NewIntValue(0)}, "error"},
		{"string_arg", []classad.Value{classad.NewStringValue("three"), classad.NewIntValue(8)}, "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := f.Call(tc.args).String(); got != tc.want {
				t.Fatalf("Call(%v) = %s, want %s", tc.args, got, tc.want)
			}
		})
	}
}

// TestScriptFuncCallArity pins the refusal Call makes for itself. The registered path never gets
// here -- the registry enforces the arity before the call -- but Call is exported.
func TestScriptFuncCallArity(t *testing.T) {
	f := compileOne(t, pctModule, "pctUsed")
	for _, args := range [][]classad.Value{
		nil,
		{classad.NewIntValue(1)},
		{classad.NewIntValue(1), classad.NewIntValue(2), classad.NewIntValue(3)},
	} {
		if got := f.Call(args).String(); got != "error" {
			t.Fatalf("Call with %d args = %s, want error", len(args), got)
		}
	}
}

// TestScriptFuncFallsOffEnd pins that a body that returns nothing on a path yields UNDEFINED, which
// is what the same body does inside a script.
func TestScriptFuncFallsOffEnd(t *testing.T) {
	f := compileOne(t, "def positive(x):\n    if x > 0:\n        return x\n", "positive")
	if got := f.Call([]classad.Value{classad.NewIntValue(7)}).String(); got != "7" {
		t.Fatalf("Call(7) = %s, want 7", got)
	}
	if got := f.Call([]classad.Value{classad.NewIntValue(-1)}).String(); got != "undefined" {
		t.Fatalf("Call(-1) = %s, want undefined", got)
	}
	// A bare `return` is the same answer written down.
	g := compileOne(t, "def nothing(x):\n    return\n", "nothing")
	if got := g.Call([]classad.Value{classad.NewIntValue(7)}).String(); got != "undefined" {
		t.Fatalf("Call = %s, want undefined", got)
	}
}

// TestScriptFuncParametersAreNotCleared is the test for the one thing compiling a `def` on its own
// had to change in the lowering: a parameter is filled from outside the program, so it must not be
// initialized to UNDEFINED the way a local nothing stores to is.
func TestScriptFuncParametersAreNotCleared(t *testing.T) {
	f := compileOne(t, "def ident(x):\n    return x\n", "ident")
	if got := f.Call([]classad.Value{classad.NewIntValue(42)}).String(); got != "42" {
		t.Fatalf("Call(42) = %s, want 42", got)
	}
}

// TestScriptFuncCallsSibling pins that a module's functions can call each other, through the same
// inliner a call inside a script uses.
func TestScriptFuncCallsSibling(t *testing.T) {
	src := "def double(x):\n    return x * 2\n\ndef quad(x):\n    return double(double(x))\n"
	f := compileOne(t, src, "quad")
	if got := f.Call([]classad.Value{classad.NewIntValue(3)}).String(); got != "12" {
		t.Fatalf("quad(3) = %s, want 12", got)
	}
}

func TestScriptFuncRecursionRefused(t *testing.T) {
	m, err := ParseScriptModule("def loop(x):\n    return loop(x)\n")
	if err != nil {
		t.Fatalf("ParseScriptModule: %v", err)
	}
	if _, err := m.Compile("loop"); err == nil || !strings.Contains(err.Error(), "recursive") {
		t.Fatalf("Compile = %v, want a recursion refusal", err)
	}
}

// TestScriptFuncPurityGate is the gate, one case per rule.
//
// Which rule catches which is the point of the table: the rules are not interchangeable, and a
// change that made one of them stop firing would be caught here by the wrong message rather than by
// a case that started passing.
func TestScriptFuncPurityGate(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string // a phrase only the rule that should fire produces
	}{
		// The read set: a bare name the body does not bind is an attribute.
		{"unscoped_read", "def f():\n    return Optimistic\n", "reads the attribute Optimistic"},
		// A scoped reference is a native load that the read set, which holds unscoped names only,
		// never sees. Rule 1 and rule 4 both pass it.
		{"scoped_read", "def f():\n    return MY.RequestMemory\n", "reads MY.RequestMemory"},
		{"target_read", "def f():\n    return TARGET.Memory\n", "reads TARGET.Memory"},
		// A nondeterministic built-in is not compiled natively, so it arrives as a delegated node.
		{"nondeterministic_builtin", "def f():\n    return random(10)\n", "random(10)"},
		// A ternary over no local at all is delegated too, which is rule 4 again.
		{"constant_ternary", "def f():\n    return (1 ? 2 : 3)\n", "(1 ? 2 : 3)"},
		// A delegated construct over a parameter compiles -- the ternary reads the parameter's
		// register -- and is refused HERE, by rule 4, because a registered function has no ad for
		// the tree-walker to evaluate the rest of that subtree against and that evaluator need not
		// be deterministic. Pinned because it is the one case where the overlay makes a function
		// compile that used to be refused earlier, and the gate has to catch it either way.
		{"ternary_over_param", "def f(x):\n    return (x ? 1 : 0)\n", "(x ? 1 : 0)"},
		// A list literal over a parameter is still refused by the COMPILER, before the gate: the
		// elements are evaluated later, from a captured scope, where the register does not exist.
		{"list_over_param", "def f(x):\n    return {x, 1}\n", `reads the local "x"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseScriptModule(tc.src)
			if err != nil {
				t.Fatalf("ParseScriptModule: %v", err)
			}
			f, err := m.Compile("f")
			if err == nil {
				t.Fatalf("Compile succeeded for %q (arity %d), want a refusal", tc.src, f.Arity())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestScriptFuncPurityGateSentinel separates the refusals that are the gate's from the one that is
// the compiler's, so a caller can tell "this function reaches the ad" from "this function does not
// compile".
func TestScriptFuncPurityGateSentinel(t *testing.T) {
	for _, src := range []string{
		"def f():\n    return Optimistic\n",
		"def f():\n    return MY.RequestMemory\n",
		"def f():\n    return random(10)\n",
	} {
		m, err := ParseScriptModule(src)
		if err != nil {
			t.Fatalf("ParseScriptModule: %v", err)
		}
		if _, err := m.Compile("f"); !errors.Is(err, ErrReadsAd) {
			t.Fatalf("Compile(%q) error = %v, want ErrReadsAd", src, err)
		}
	}
}

// TestScriptFuncPureFunctionsCompile is the other half of the gate: a function that only computes
// from its arguments is accepted, including one that calls strict built-ins and one that loops.
func TestScriptFuncPureFunctionsCompile(t *testing.T) {
	cases := []struct {
		name string
		src  string
		args []classad.Value
		want string
	}{
		{"builtin_call", "def up(s):\n    return toUpper(s)\n",
			[]classad.Value{classad.NewStringValue("ab")}, `"AB"`},
		{"loop", "def halvings(n):\n    k = 0\n    while n > 1:\n        n = n / 2\n        k = k + 1\n    return k\n",
			[]classad.Value{classad.NewIntValue(64)}, "6"},
		{"no_parameters", "def answer():\n    return 42\n", nil, "42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := compileOne(t, tc.src, strings.SplitN(strings.TrimPrefix(tc.src, "def "), "(", 2)[0])
			if got := f.Call(tc.args).String(); got != tc.want {
				t.Fatalf("Call = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestScriptFuncStepBudget pins that a registered function cannot hang a scan. A script may loop,
// so one that does not terminate must end as ERROR rather than as a goroutine nothing can stop.
func TestScriptFuncStepBudget(t *testing.T) {
	f := compileOne(t, "def forever(n):\n    while n >= 0:\n        n = n + 1\n    return n\n", "forever")
	t.Run("default_budget", func(t *testing.T) {
		if got := f.Call([]classad.Value{classad.NewIntValue(0)}).String(); got != "error" {
			t.Fatalf("Call = %s, want error", got)
		}
	})
	t.Run("budget_is_per_call", func(t *testing.T) {
		// A call that ran out must not leave the next one short: each call gets the whole budget,
		// which is what makes a scan's millionth record behave like its first.
		g := compileOne(t, "def ident(x):\n    return x\n", "ident")
		g.budget = 4
		for i := range 3 {
			if got := g.Call([]classad.Value{classad.NewIntValue(int64(i))}).String(); got != strconv.Itoa(i) {
				t.Fatalf("call %d = %s", i, got)
			}
		}
	})
}

// TestScriptFuncConcurrent calls one compiled function from many goroutines at once.
//
// This is the property that makes a script function registrable at all: a UDF is reached from the
// parallel scan and from the vector executor, and *classad.ClassAd is explicitly not safe to share
// between goroutines. Each call must own its register file and its evaluator. Run under -race.
func TestScriptFuncConcurrent(t *testing.T) {
	f := compileOne(t, pctModule, "pctUsed")
	const goroutines, iterations = 32, 200
	var wg sync.WaitGroup
	errs := make([]string, goroutines)
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			// Each goroutine uses its own arguments, so a shared register file would not merely
			// race: it would produce another goroutine's answer, which the assertion below catches.
			total := int64(g + 1)
			want := classad.NewRealValue(float64(1) * 100.0 / float64(total)).String()
			for range iterations {
				got := f.Call([]classad.Value{classad.NewIntValue(1), classad.NewIntValue(total)}).String()
				if got != want {
					errs[g] = got + " != " + want
					return
				}
			}
		}(g)
	}
	wg.Wait()
	for g, e := range errs {
		if e != "" {
			t.Fatalf("goroutine %d: %s", g, e)
		}
	}
}

// TestScriptFuncDelegatedHintFitsWhatWasFound pins that the refusal's advice is chosen from the
// construct that caused it. One fixed hint listed every workaround at once, so the author of a list
// literal was told to rewrite a ternary they had not written.
func TestScriptFuncDelegatedHintFitsWhatWasFound(t *testing.T) {
	refusal := func(t *testing.T, src string) string {
		t.Helper()
		m, err := ParseScriptModule(src)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if _, cerr := m.Compile("f"); cerr != nil {
			return cerr.Error()
		}
		t.Fatal("expected a refusal")
		return ""
	}

	ternary := refusal(t, "def f(x):\n    return (1 ? 2 : 3)\n")
	if !strings.Contains(ternary, "`if` statement") {
		t.Errorf("a ternary should be told about `if`; got %q", ternary)
	}

	list := refusal(t, "def f(x):\n    return size({1, 2, 3})\n")
	if strings.Contains(list, "ternary") {
		t.Errorf("a list literal should not be told to rewrite a ternary; got %q", list)
	}
	if !strings.Contains(list, "{1, 2, 3}") {
		t.Errorf("the refusal should name what it found; got %q", list)
	}
}
