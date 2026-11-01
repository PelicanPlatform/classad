package wasmudf

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// fakeRegistry stands in for classad's registry so the wiring is testable before
// that registry exists, and testable afterwards without a process-wide table a
// test cannot reset.
type fakeRegistry struct {
	names map[string]int // name -> times registered
	fns   map[string]func([]classad.Value) classad.Value
	arity map[string][2]int
	err   error
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{
		names: map[string]int{},
		fns:   map[string]func([]classad.Value) classad.Value{},
		arity: map[string][2]int{},
	}
}

func (f *fakeRegistry) RegisterFunction(name string, minArgs, maxArgs int, fn func(args []classad.Value) classad.Value) error {
	if f.err != nil {
		return f.err
	}
	f.names[name]++
	f.fns[name] = fn
	f.arity[name] = [2]int{minArgs, maxArgs}
	return nil
}

// TestRegisterWiresTheGuest -- the function handed to the registry must be the
// one that runs the module. Asserting the name and arity alone would pass for a
// registration of the wrong function, so this CALLS what was registered.
func TestRegisterWiresTheGuest(t *testing.T) {
	l := newLoader(t, Options{})
	reg := newFakeRegistry()

	u, err := Register(context.Background(), l, reg, "double", 1, 1, fixture(t, "tagecho"))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if reg.names["double"] != 1 {
		t.Fatalf("registered %d times", reg.names["double"])
	}
	if got := reg.arity["double"]; got != [2]int{1, 1} {
		t.Errorf("registered arity %v, want 1..1", got)
	}

	fn := reg.fns["double"]
	if fn == nil {
		t.Fatal("no function registered")
	}
	if n := mustInt(t, fn([]classad.Value{classad.NewIntValue(20)})); n != 40 {
		t.Errorf("the registered function returned %d for 20, want 40", n)
	}
	if s := u.Stats(); s.Calls != 1 {
		t.Errorf("the registered function did not reach this UDF (%d calls)", s.Calls)
	}
}

// TestRegisterLoadsBeforeRegistering -- a name is only ever registered against a
// module that compiled, exports what it must and has been called once. The
// registry is append-only, so a registration made on a module that does not work
// cannot be taken back.
func TestRegisterLoadsBeforeRegistering(t *testing.T) {
	l := newLoader(t, Options{})
	reg := newFakeRegistry()
	if _, err := Register(context.Background(), l, reg, "f", 1, 1, fixture(t, "badversion")); err == nil {
		t.Fatal("Register accepted a module with the wrong ABI version")
	}
	if len(reg.names) != 0 {
		t.Errorf("a refused module was registered: %v", reg.names)
	}
}

// TestRegisterReportsCollision -- the registry owns the name space (a UDF may not
// shadow a built-in), so its refusal has to reach the caller with the name in it.
func TestRegisterReportsCollision(t *testing.T) {
	l := newLoader(t, Options{})
	reg := newFakeRegistry()
	reg.err = errors.New("pow is a built-in")

	u, err := Register(context.Background(), l, reg, "pow", 2, 2, fixture(t, "identity"))
	if err == nil {
		t.Fatal("Register ignored the registry's refusal")
	}
	if !strings.Contains(err.Error(), "pow") || !strings.Contains(err.Error(), "built-in") {
		t.Errorf("error loses the registry's reason: %v", err)
	}
	// The module stays loaded, so retrying under another name does not recompile.
	if u == nil {
		t.Fatal("Register discarded the compiled module on a name collision")
	}
	if _, ok := l.Lookup("pow"); !ok {
		t.Error("the loaded module is not in the loader")
	}
}

// TestClassAdRegistryIsExplicitlyUnavailable pins the one-line switch. When
// classad.RegisterFunction lands, this test is what fails, and its failure is the
// reminder to delete it.
func TestClassAdRegistryIsExplicitlyUnavailable(t *testing.T) {
	err := ClassAdRegistry().RegisterFunction("f", 1, 1, func([]classad.Value) classad.Value {
		return classad.NewUndefinedValue()
	})
	if !errors.Is(err, ErrRegistryUnavailable) {
		t.Errorf("got %v, want ErrRegistryUnavailable", err)
	}
}

// TestFuncIsTheRegistrysType -- Func must satisfy the registry's signature
// exactly: a pure function of argument values, no evaluator, no error. The
// assignment below is the test; it stops compiling if either side moves.
func TestFuncIsTheRegistrysType(t *testing.T) {
	u := loadFixture(t, Options{}, "identity", 1, 1)
	var reg Registry = newFakeRegistry()
	if err := reg.RegisterFunction(u.Name(), 1, 1, u.Func()); err != nil {
		t.Fatal(err)
	}
}
