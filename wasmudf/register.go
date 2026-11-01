package wasmudf

import (
	"context"
	"errors"
	"fmt"

	"github.com/PelicanPlatform/classad/classad"
)

// Registry is the part of classad's user-defined function registry this package
// needs: register a name, an arity range, and a pure function of argument values.
//
// It is an interface for two reasons. The registry lands in package classad
// independently of this one, so the wiring has to compile and be testable before
// it exists; and a test can hand Register a registry it can inspect, which is the
// only way to assert that loading registers the right name with the right arity
// without depending on a process-wide table it cannot reset.
//
// The method takes an UNNAMED func type so that it is satisfied by a method
// declared over classad.Func too -- a conversion between the two is allowed in
// either direction, and nothing here needs to name the registry's own type.
type Registry interface {
	RegisterFunction(name string, minArgs, maxArgs int, fn func(args []classad.Value) classad.Value) error
}

// ErrRegistryUnavailable is returned by the classad registry adapter until
// classad.RegisterFunction exists. See ClassAdRegistry.
var ErrRegistryUnavailable = errors.New("wasmudf: classad.RegisterFunction is not available in this classad version")

// ClassAdRegistry returns the process-wide registry in package classad.
//
// THE SWITCH IS THE ONE LINE MARKED BELOW. Until classad.RegisterFunction exists
// this adapter refuses, loudly, rather than pretending to register and leaving a
// UDF that no expression can call; everything else in this package -- loading,
// the ABI, the bounds, Register itself -- is exercised against a Registry
// implementation in the tests and does not wait on it.
func ClassAdRegistry() Registry { return classAdRegistry{} }

type classAdRegistry struct{}

func (classAdRegistry) RegisterFunction(_ string, _, _ int, _ func(args []classad.Value) classad.Value) error {
	// Replace this line with:
	//	return classad.RegisterFunction(name, minArgs, maxArgs, fn)
	// and name the parameters above.
	return ErrRegistryUnavailable
}

// Register loads wasm as name and registers it, so an expression calling name
// gets the guest.
//
// Loading comes first and registration second, which is the order that matters:
// every static problem with the module is a returned error here, and a name is
// only ever registered against a module that compiled, exports what it must,
// speaks this ABI, and has been instantiated and called once. A registration is
// not withdrawable -- compiled programs capture the strict/delegated decision for
// a name, so a name that has been seen cannot stop existing -- so registering
// something unproven is not a mistake that can be taken back.
//
// On a registration error the UDF stays loaded in the Loader, reachable through
// Lookup. It is not unloaded, because the usual reason for the error is that the
// name is taken, and silently discarding a compiled module on the way out of a
// failed call makes a retry under another name recompile for no reason.
func Register(ctx context.Context, l *Loader, reg Registry, name string, minArgs, maxArgs int, wasm []byte) (*UDF, error) {
	u, err := l.Load(ctx, name, minArgs, maxArgs, wasm)
	if err != nil {
		return nil, err
	}
	if err := reg.RegisterFunction(name, minArgs, maxArgs, u.Func()); err != nil {
		return u, fmt.Errorf("wasmudf: registering %s: %w", name, err)
	}
	return u, nil
}
