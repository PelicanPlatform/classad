package scriptudf_test

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/scriptudf"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// fakeRegistry records registrations instead of making them.
//
// The real registry is process-wide and append-only, so a test that used it could not assert what a
// REFUSED registration left behind: there is no way to clear a name and try again. A test registry
// can be inspected and thrown away.
type fakeRegistry struct {
	mu   sync.Mutex
	seen []string
	min  []int
	max  []int
	err  error // what RegisterFunction returns, for the failure paths
}

func (f *fakeRegistry) RegisterFunction(name string, minArgs, maxArgs int, fn func(args []classad.Value) classad.Value) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.seen = append(f.seen, name)
	f.min = append(f.min, minArgs)
	f.max = append(f.max, maxArgs)
	return nil
}

const pctSource = `
def pctUsed(used, total):
    if used is undefined:
        return undefined
    if total is undefined:
        return undefined
    if total <= 0:
        return error
    return used * 100.0 / total
`

func mustParse(t *testing.T, src string) *vm.ScriptModule {
	t.Helper()
	m, err := vm.ParseScriptModule(src)
	if err != nil {
		t.Fatalf("ParseScriptModule: %v", err)
	}
	return m
}

// TestRegisterTakesArityFromTheSource is the difference from a wasm or Go registration: the `def`
// says how many arguments it takes, so the minimum and the maximum are not the caller's to supply.
func TestRegisterTakesArityFromTheSource(t *testing.T) {
	reg := &fakeRegistry{}
	f, err := scriptudf.Register(mustParse(t, pctSource), reg, "pctUsed")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if f.Arity() != 2 {
		t.Fatalf("Arity() = %d, want 2", f.Arity())
	}
	if len(reg.seen) != 1 || reg.seen[0] != "pctUsed" {
		t.Fatalf("registered %v, want [pctUsed]", reg.seen)
	}
	if reg.min[0] != 2 || reg.max[0] != 2 {
		t.Fatalf("registered arity %d..%d, want 2..2", reg.min[0], reg.max[0])
	}
}

// TestRegisterRefusesAFunctionThatReadsTheAd pins that the purity gate runs BEFORE the registry is
// touched. Nothing may be registered against a function that could not keep the promise a
// registration makes, because a registration cannot be withdrawn.
func TestRegisterRefusesAFunctionThatReadsTheAd(t *testing.T) {
	reg := &fakeRegistry{}
	src := "def fits(requested, cap):\n    if requested is undefined:\n        return Optimistic\n    return requested <= cap\n"
	_, err := scriptudf.Register(mustParse(t, src), reg, "fits")
	if !errors.Is(err, vm.ErrReadsAd) {
		t.Fatalf("Register error = %v, want vm.ErrReadsAd", err)
	}
	if len(reg.seen) != 0 {
		t.Fatalf("registered %v after a refusal", reg.seen)
	}
}

// TestRegisterRefusesABuiltinName: the refusal comes from the module parser rather than the
// registry, because a module whose own `def` shadowed `strcat` would mean something different
// inside the module than everywhere else -- which is true whether or not it is ever registered.
func TestRegisterRefusesABuiltinName(t *testing.T) {
	_, err := vm.ParseScriptModule("def strcat(a, b):\n    return a\n")
	if err == nil || !strings.Contains(err.Error(), "built-in function") {
		t.Fatalf("ParseScriptModule error = %v, want a built-in refusal", err)
	}
}

// TestRegisterPropagatesRegistrySentinels: the failures a loader acts on are classad's own, not new
// ones invented here, and wrapping must keep errors.Is working on them.
func TestRegisterPropagatesRegistrySentinels(t *testing.T) {
	for _, want := range []error{classad.ErrBuiltinName, classad.ErrFunctionExists, classad.ErrBadFunction} {
		reg := &fakeRegistry{err: want}
		_, err := scriptudf.Register(mustParse(t, pctSource), reg, "pctUsed")
		if !errors.Is(err, want) {
			t.Fatalf("Register error = %v, want %v", err, want)
		}
	}
}

// TestRegisterTwiceIsRefused goes through the real registry, which is the only thing that can
// answer "is this name taken" truthfully.
func TestRegisterTwiceIsRefused(t *testing.T) {
	src := "def scriptDoubleReg(x):\n    return x + 1\n"
	m := mustParse(t, src)
	if _, err := scriptudf.Register(m, scriptudf.ClassAdRegistry(), "scriptDoubleReg"); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	_, err := scriptudf.Register(m, scriptudf.ClassAdRegistry(), "scriptDoubleReg")
	if !errors.Is(err, classad.ErrFunctionExists) {
		t.Fatalf("second Register error = %v, want classad.ErrFunctionExists", err)
	}
}

// TestRegisterOneFunctionLeavesTheSiblingsCompilable is the reason a module's definitions are not
// refused for colliding with a registered name the way a script's are: registering one export must
// not stop the module's other exports compiling.
func TestRegisterOneFunctionLeavesTheSiblingsCompilable(t *testing.T) {
	src := "def scriptSibA(x):\n    return x + 1\n\ndef scriptSibB(x):\n    return scriptSibA(x) * 2\n"
	m := mustParse(t, src)
	if _, err := scriptudf.Register(m, scriptudf.ClassAdRegistry(), "scriptSibA"); err != nil {
		t.Fatalf("Register scriptSibA: %v", err)
	}
	f, err := scriptudf.Register(m, scriptudf.ClassAdRegistry(), "scriptSibB")
	if err != nil {
		t.Fatalf("Register scriptSibB after its sibling was registered: %v", err)
	}
	if got := f.Call([]classad.Value{classad.NewIntValue(3)}).String(); got != "8" {
		t.Fatalf("scriptSibB(3) = %s, want 8", got)
	}
}

// TestRegisteredFunctionFromAnExpression is the point of the whole thing: after registration the
// name is a function like any built-in, reached from an ordinary ClassAd expression.
func TestRegisteredFunctionFromAnExpression(t *testing.T) {
	src := `
def scriptPct(used, total):
    if used is undefined:
        return undefined
    if total is undefined:
        return undefined
    if total <= 0:
        return error
    return used * 100.0 / total
`
	if _, err := scriptudf.Register(mustParse(t, src), scriptudf.ClassAdRegistry(), "scriptPct"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ad, err := classad.Parse(`[ Used = 3; Total = 8; Label = "eight" ]`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cases := []struct {
		name string
		expr string
		want string
	}{
		{"numbers", `scriptPct(Used, Total)`, "37.5"},
		{"name_folds", `SCRIPTPCT(Used, Total)`, "37.5"},
		{"absent_attribute", `scriptPct(Used, Absent)`, "undefined"},
		{"string_argument", `scriptPct(Used, Label)`, "error"},
		{"too_few_arguments", `scriptPct(Used)`, "error"},
		{"too_many_arguments", `scriptPct(Used, Total, Used)`, "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := classad.ParseExpr(tc.expr)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.expr, err)
			}
			if got := e.Eval(ad).String(); got != tc.want {
				t.Fatalf("%s = %s, want %s", tc.expr, got, tc.want)
			}
		})
	}
	// A registered script function is compilable, which is what keeps an expression using it on the
	// native and columnar paths. That is the whole reason the purity gate exists.
	if !classad.StrictFunction("scriptPct", 2) {
		t.Fatal("StrictFunction(scriptPct, 2) = false, want true")
	}
	if classad.StrictFunction("scriptPct", 3) {
		t.Fatal("StrictFunction(scriptPct, 3) = true; the arity comes from the def")
	}
}

// TestRegisteredFunctionIsCalledConcurrently exercises the registered path from many goroutines at
// once, which is how a scan reaches it. Run under -race.
func TestRegisteredFunctionIsCalledConcurrently(t *testing.T) {
	src := "def scriptConcur(a, b):\n    return a * 100 + b\n"
	if _, err := scriptudf.Register(mustParse(t, src), scriptudf.ClassAdRegistry(), "scriptConcur"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	e, err := classad.ParseExpr(`scriptConcur(A, B)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	const goroutines, iterations = 16, 100
	var wg sync.WaitGroup
	bad := make([]string, goroutines)
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			// Each goroutine brings its own ad, because *classad.ClassAd is not safe to share.
			ad, err := classad.Parse(`[ B = 1 ]`)
			if err != nil {
				bad[g] = err.Error()
				return
			}
			if err := ad.Set("A", g); err != nil {
				bad[g] = err.Error()
				return
			}
			want := classad.NewIntValue(int64(g)*100 + 1).String()
			for range iterations {
				if got := e.Eval(ad).String(); got != want {
					bad[g] = got + " != " + want
					return
				}
			}
		}(g)
	}
	wg.Wait()
	for g, b := range bad {
		if b != "" {
			t.Fatalf("goroutine %d: %s", g, b)
		}
	}
}
