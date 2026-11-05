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

// ErrRegistryUnavailable was returned by the classad registry adapter while
// classad.RegisterFunction did not yet exist.
//
// Deprecated: it exists, and ClassAdRegistry calls it. Nothing returns this any
// more. It is kept because it is exported and a caller may still be testing for
// it -- a caller that does will now simply never see it, where deleting it would
// stop that caller compiling.
var ErrRegistryUnavailable = errors.New("wasmudf: classad.RegisterFunction is not available in this classad version")

// ClassAdRegistry returns the process-wide registry in package classad, which is
// what makes a loaded module reachable from an expression: after registering
// through it, an expression calling the module's name gets the guest.
//
// Use it for anything other than a test. The interface exists so a test can pass
// a registry it can inspect, since classad's registry is process-wide and
// append-only and a test cannot undo a registration in it.
func ClassAdRegistry() Registry { return classAdRegistry{} }

type classAdRegistry struct{}

func (classAdRegistry) RegisterFunction(name string, minArgs, maxArgs int, fn func(args []classad.Value) classad.Value) error {
	return classad.RegisterFunction(name, minArgs, maxArgs, fn)
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
	return RegisterWith(ctx, l, reg, name, minArgs, maxArgs, wasm, LoadOptions{})
}

// RegisterWith is Register with the per-module options an admin sets.
//
// It is a separate function rather than a changed signature because the registry
// is append-only and so is this API: a caller already registering UDFs keeps
// compiling. The trust label arrives HERE, from configuration, and reaches the
// Loader as a Go value -- there is no path by which the module being registered
// could influence it.
func RegisterWith(ctx context.Context, l *Loader, reg Registry, name string, minArgs, maxArgs int, wasm []byte, lo LoadOptions) (*UDF, error) {
	u, err := l.LoadWith(ctx, name, minArgs, maxArgs, wasm, lo)
	if err != nil {
		return nil, err
	}
	if err := reg.RegisterFunction(name, minArgs, maxArgs, u.Func()); err != nil {
		return u, fmt.Errorf("wasmudf: registering %s: %w", name, err)
	}
	return u, nil
}
