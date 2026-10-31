package classad

import (
	"fmt"
	"strings"
	"testing"
)

// TestNonStrictFunctionsAreKnown guards the cheapest way this split can break
// silently: a misspelled or wrongly-cased key in nonStrictFunctions.
//
// The map is consulted with an already-folded name, so a key like "ifThenElse"
// would never match. Nothing would fail to compile and no existing test would
// notice -- StrictFunction would simply start accepting a LAZY built-in, and a
// compiled executor would evaluate both branches of every ifThenElse, which is
// observable only for an argument that must not be evaluated at all.
func TestNonStrictFunctionsAreKnown(t *testing.T) {
	for name := range nonStrictFunctions {
		if name != strings.ToLower(name) {
			t.Errorf("nonStrictFunctions key %q is not lower-cased, so it can never match", name)
		}
		if _, ok := functionArity[name]; !ok {
			t.Errorf("nonStrictFunctions key %q is not a known function; a typo here silently makes the real one strict", name)
		}
	}
}

// strictArgSources are literal argument expressions, as source text. They are
// evaluated rather than constructed, so a LIST literal is a real lazy list --
// which is the one argument shape that makes the ten list-consuming built-ins
// reach for an evaluator at all (see applyBuiltin).
var strictArgSources = []string{
	`1`, `2`, `0`, `3.5`, `"abc"`, `""`, `true`, `undefined`, `error`,
	`{1, 2, 3}`, `{"abc", "x"}`, `{}`, // lazy lists: member/sum/avg/min/max/join/quantize
}

// TestStrictFunctionsMatchASTEvaluation is the differential test for the strict
// set: for every built-in StrictFunction accepts, evaluating the arguments and
// then calling ApplyFunction must produce exactly what evaluating the equivalent
// FunctionCall AST produces. That is precisely what OpCallFunc does, so this is
// the compiled path checked against the tree-walking one.
//
// It is the test that catches a built-in wrongly classified as strict. A function
// that reaches for the calling scope or the raw AST cannot agree with itself
// across the two paths, because the compiled path hands it values and nothing
// else. A nondeterministic one cannot agree with itself either.
//
// The list arguments matter more than they look. A lazy list value holds its
// element expressions plus the ad they were written in, so materializing it needs
// an evaluator; passing a DIFFERENT evaluator bound to the same ad -- which is
// what happens here, and what happens in the interpreter -- must give the same
// answer. That is the property that makes the ten list-consuming built-ins safe
// to compile.
//
// Laziness is NOT caught here: a lazy built-in agrees with itself on plain
// literals, differing only on an argument that must not be evaluated at all.
// That is why the lazy entries are pinned by name in nonStrictFunctions and by
// the cyclic-argument tests.
func TestStrictFunctionsMatchASTEvaluation(t *testing.T) {
	// A real scope, so a lazy list argument captures something non-nil and a
	// divergence in how it is materialized has somewhere to show up.
	scope, err := Parse(`[ Cpus = 4; Owner = "alice" ]`)
	if err != nil {
		t.Fatalf("parse scope: %v", err)
	}
	checked, withLists := 0, 0
	for name, arity := range functionArity {
		// The argument counts worth trying: the minimum, and one more for an
		// optional or variadic argument, so both arities of substr/regexp/join
		// are exercised rather than just the shorter one.
		argcs := []int{arity.min}
		if arity.max < 0 || arity.max > arity.min {
			argcs = append(argcs, arity.min+1)
		}
		for _, argc := range argcs {
			if !StrictFunction(name, argc) {
				continue
			}
			for _, argSrc := range strictArgSources {
				srcs := make([]string, argc)
				args := make([]Value, argc)
				for i := range srcs {
					srcs[i] = argSrc
					ax, err := ParseExpr(argSrc)
					if err != nil {
						t.Fatalf("parse argument %q: %v", argSrc, err)
					}
					args[i] = ax.Eval(scope)
				}
				call := fmt.Sprintf("%s(%s)", name, strings.Join(srcs, ", "))
				expr, err := ParseExpr(call)
				if err != nil {
					t.Fatalf("parse %q: %v", call, err)
				}
				want := expr.Eval(scope)
				got := ApplyFunction(NewEvaluator(scope), name, args)
				if want.Type() != got.Type() || want.String() != got.String() {
					t.Errorf("%s: AST eval = %s (%v), ApplyFunction = %s (%v); a strict built-in must agree between the two paths",
						call, want.String(), want.Type(), got.String(), got.Type())
				}
				checked++
				if argc > 0 && strings.HasPrefix(argSrc, "{") {
					withLists++
				}
			}
		}
	}
	if checked < 200 {
		t.Fatalf("only %d strict calls checked; the scanner is broken or the strict set collapsed", checked)
	}
	if withLists < 50 {
		t.Fatalf("only %d calls had a list argument; the lazy-list path is the point of this test", withLists)
	}
	t.Logf("%d strict built-in calls agree between AST evaluation and ApplyFunction (%d with a lazy-list argument)",
		checked, withLists)
}

// TestStrictFunctionRejects pins the four reasons StrictFunction says no, so a
// change that starts accepting one of these has to update this test and say why.
// Each entry would be a real behaviour change if compiled to an eager call:
// see StrictFunction's comment.
func TestStrictFunctionRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		argc int
		why  string
	}{
		{"ifThenElse", 3, "lazy: evaluates only the selected branch"},
		{"strcat", 2, "lazy: stops at the first undefined/error argument"},
		{"eval", 1, "parses a runtime string and resolves it in the calling scope"},
		{"unparse", 1, "needs the argument's raw AST"},
		{"countMatches", 2, "evaluates its first argument in each nested ad's scope"},
		{"evalInEachContext", 2, "likewise"},
		{"random", 1, "nondeterministic"},
		{"time", 0, "nondeterministic"},
		{"formatTime", 0, "nondeterministic with no argument"},
		{"pow", 1, "wrong arity: ERROR without evaluating the arguments"},
		{"pow", 3, "likewise"},
		{"noSuchFunction", 1, "unknown name: ERROR without evaluating the arguments"},
	} {
		if StrictFunction(tc.name, tc.argc) {
			t.Errorf("StrictFunction(%q, %d) = true, want false (%s)", tc.name, tc.argc, tc.why)
		}
	}
	// Case-insensitivity, matching how the reference engine matches names: the
	// folded form is what both the map and functionArity are keyed by.
	for _, spelling := range []string{"toUpper", "TOUPPER", "toupper", "ToUpPeR"} {
		if !StrictFunction(spelling, 1) {
			t.Errorf("StrictFunction(%q, 1) = false, want true", spelling)
		}
	}
	for _, spelling := range []string{"ifThenElse", "IFTHENELSE", "ifthenelse"} {
		if StrictFunction(spelling, 3) {
			t.Errorf("StrictFunction(%q, 3) = true, want false", spelling)
		}
	}
}
