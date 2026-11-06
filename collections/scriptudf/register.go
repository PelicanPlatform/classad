// Package scriptudf registers a function written in ClassAd Script as a user-defined function, so
// an ordinary ClassAd expression can call it.
//
// It is the script counterpart of wasmudf, and deliberately the same shape: a module exports
// functions by name, registration picks one of those exports and puts it in classad's registry, and
// a caller wiring extensions into a process hands either kind of module to the same adapter.
//
// Two things differ, and both are things a script can do that a wasm guest cannot:
//
//   - A `def` STATES ITS OWN ARITY, so Register takes no min/max. There is no arity for a caller to
//     supply and get wrong.
//
//   - PURITY IS CHECKED. A wasm guest is pure because the ABI hands it values and nothing else. A
//     script function can read the subject ad -- inside a script that is the point of it -- so
//     whether a particular one may be registered is decided by the compiler and reported here as
//     vm.ErrReadsAd. A registered function may not read the ad.
package scriptudf

import (
	"fmt"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// Registry is the part of classad's user-defined function registry this package needs: a name, an
// arity range, and a pure function of argument values.
//
// It is the interface wasmudf declares, redeclared rather than imported. Go interfaces are
// structural, so an adapter written for one satisfies the other, and a caller can hand a script
// module and a wasm module to the same registry; importing wasmudf's copy would instead make every
// program that registers a script function link a WebAssembly runtime.
//
// The method takes an UNNAMED func type so that it is satisfied by a method declared over
// classad.Func too.
type Registry interface {
	RegisterFunction(name string, minArgs, maxArgs int, fn func(args []classad.Value) classad.Value) error
}

// ClassAdRegistry returns the process-wide registry in package classad, which is what makes a
// registered function reachable from an expression.
//
// Use it for anything other than a test. The interface exists so a test can pass a registry it can
// inspect, since classad's registry is process-wide and append-only and a test cannot undo a
// registration in it.
func ClassAdRegistry() Registry { return classAdRegistry{} }

type classAdRegistry struct{}

func (classAdRegistry) RegisterFunction(name string, minArgs, maxArgs int, fn func(args []classad.Value) classad.Value) error {
	return classad.RegisterFunction(name, minArgs, maxArgs, fn)
}

// Register compiles the module's function called name and registers it, so an expression calling
// name reaches it.
//
// COMPILATION COMES FIRST AND REGISTRATION SECOND, which is the order that matters and the same
// order wasmudf uses. Every static problem with the function -- a syntax error in its body,
// recursion, and above all reaching the ad -- is a returned error here, and a name is only ever
// registered against a function that compiled and passed the purity gate. A registration cannot be
// withdrawn: compiled programs capture the decision that a name was a UDF, and they are cached and
// shared, so registering something unproven is not a mistake that can be taken back.
//
// The errors a loader will want to tell apart are the registry's own sentinels, not new ones:
// classad.ErrBuiltinName for a name that is already a built-in, classad.ErrFunctionExists for a name
// already registered. vm.ErrReadsAd is the one failure that has no counterpart in a Go or wasm
// registration, because only a script can express it.
func Register(mod *vm.ScriptModule, reg Registry, name string) (*vm.ScriptFunc, error) {
	f, err := mod.Compile(name)
	if err != nil {
		return nil, err
	}
	// Arity twice: a `def` declares exactly as many parameters as it takes, so the minimum and the
	// maximum are the same number and neither is a choice.
	if err := reg.RegisterFunction(f.Name(), f.Arity(), f.Arity(), f.Func()); err != nil {
		return f, fmt.Errorf("scriptudf: registering %s: %w", f.Name(), err)
	}
	return f, nil
}
