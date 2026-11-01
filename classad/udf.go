package classad

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
)

// USER-DEFINED FUNCTIONS.
//
// A UDF is registered by name and then behaves exactly as a built-in does: the compiler sees it
// through StrictFunction and compiles a call to it natively, the executors reach it through
// ApplyFunction, and a query using one stays eligible for every columnar accelerator. Nothing about
// the call path is special-cased for it, which is the point -- the built-in call protocol was built
// so that a UDF would be a registration rather than a second mechanism.
//
// PURITY IS STRUCTURAL, NOT DOCUMENTED. Func receives values and nothing else: no evaluator, no
// scope, no AST. A UDF therefore cannot read an attribute, cannot resolve a reference, and cannot
// materialize a lazy list -- and so cannot make a compiled program disagree with the tree-walking
// evaluator about what an expression reads. That is the property StrictFunction promises the
// compiler, and a signature that could not keep it would be a promise made in a comment.
//
// The cost of that is real and worth stating: a UDF handed a list-valued argument cannot look inside
// it, because realizing a list literal's elements requires evaluating them in the scope they were
// written in. Such a UDF should return ERROR.
//
// APPEND-ONLY, AND WHY THERE IS NO UNREGISTER. A compiled program captures the decision StrictFunction
// made: a name that was a UDF became an OpCallFunc, and a name that was not became a delegated node.
// Programs are cached and shared. Withdrawing a registration would leave compiled programs calling
// something that is gone, and re-deciding it would mean invalidating every cached program in the
// process. Registration is expected during start-up, before anything compiles.

// Registration failures a caller can act on differently, as sentinels rather than only as messages.
//
// A loader registering a batch of functions wants to tell these apart: colliding with a built-in is a
// mistake in what it was asked to load, already-registered is usually a double-load it can ignore,
// and a malformed name or arity is a bug in the thing that produced them. Without sentinels a caller
// can only re-wrap the text, and text is not an interface.
var (
	// ErrBuiltinName means the name is already a built-in, which a UDF may not shadow.
	ErrBuiltinName = errors.New("classad: name is a built-in")
	// ErrFunctionExists means the name is already registered as a UDF.
	ErrFunctionExists = errors.New("classad: function already registered")
	// ErrBadFunction means the registration itself is malformed: a nil function, a name no
	// expression could call, or an impossible arity.
	ErrBadFunction = errors.New("classad: invalid function registration")
)

// Func is a user-defined function: a pure function of its argument values.
//
// It must not panic. One that does is contained -- ApplyFunction converts it to ERROR rather than
// letting it reach a daemon's goroutine -- but a panic is not a way to report failure. Returning
// ERROR is.
type Func func(args []Value) Value

// udfEntry is one registration. Read-only once published.
type udfEntry struct {
	name  string // as registered, for diagnostics; lookups go by the folded name
	arity funcArity
	fn    Func
}

// udfs is the registry, published as a whole map rather than mutated in place.
//
// Copy-on-write, so the READ path -- which runs once per call per record -- is a single atomic load
// with no lock. A mutex here would be correct and would show up in a scan: registration happens a
// handful of times in a process's life and lookup happens millions.
var udfs atomic.Pointer[map[string]udfEntry]

// validUDFName reports whether name is one an expression could actually call.
//
// A name the parser cannot produce is a registration nobody can use, and failing at registration is
// a better place to find that out than at every call site.
func validUDFName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// RegisterFunction registers name as a user-defined function accepting minArgs..maxArgs arguments,
// where a negative maxArgs means variadic.
//
// It fails rather than overwriting: a name that collides with a built-in or with an existing
// registration is an error. Shadowing a built-in would change what existing expressions mean, and
// silently replacing a registration would make the behaviour of a call depend on package
// initialization order.
func RegisterFunction(name string, minArgs, maxArgs int, fn Func) error {
	if fn == nil {
		return fmt.Errorf("RegisterFunction(%q): nil function: %w", name, ErrBadFunction)
	}
	if !validUDFName(name) {
		return fmt.Errorf("RegisterFunction(%q): not a name an expression can call: %w", name, ErrBadFunction)
	}
	if minArgs < 0 || (maxArgs >= 0 && maxArgs < minArgs) {
		return fmt.Errorf("RegisterFunction(%q): bad arity %d..%d: %w", name, minArgs, maxArgs, ErrBadFunction)
	}
	folded := strings.ToLower(name)
	if _, isBuiltin := functionArity[folded]; isBuiltin {
		return fmt.Errorf("RegisterFunction(%q): %w", name, ErrBuiltinName)
	}
	entry := udfEntry{name: name, arity: funcArity{min: minArgs, max: maxArgs}, fn: fn}
	for {
		cur := udfs.Load()
		next := make(map[string]udfEntry, 8)
		if cur != nil {
			for k, v := range *cur {
				next[k] = v
			}
		}
		if prev, dup := next[folded]; dup {
			return fmt.Errorf("RegisterFunction(%q): registered as %q: %w", name, prev.name, ErrFunctionExists)
		}
		next[folded] = entry
		if udfs.CompareAndSwap(cur, &next) {
			return nil
		}
	}
}

// lookupUDF finds a registration by its folded name. Callers on a per-record path should check
// udfs.Load() first and skip this entirely when nothing is registered.
func lookupUDF(folded string) (udfEntry, bool) {
	m := udfs.Load()
	if m == nil {
		return udfEntry{}, false
	}
	e, ok := (*m)[folded]
	return e, ok
}

// RegisteredFunctions returns the registered names, as registered, for diagnostics. The order is
// unspecified.
func RegisteredFunctions() []string {
	m := udfs.Load()
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(*m))
	for _, e := range *m {
		out = append(out, e.name)
	}
	return out
}

// callUDF applies a registration, containing a panic as ERROR.
//
// Containment is not an invitation to panic -- a UDF reports failure by returning ERROR -- it is
// because this library runs inside daemons that have no top-level recovery, where an escaping panic
// from one record's arithmetic would take down a process serving everything else.
//
// A cyclic-reference panic is deliberately NOT contained. A UDF cannot raise one, having no
// evaluator to resolve a reference with, so seeing one here means it came from somewhere this
// function does not understand, and swallowing it would turn a structural failure into a value.
func callUDF(e udfEntry, args []Value) (result Value) {
	defer func() {
		if r := recover(); r != nil {
			if _, cyclic := r.(cyclicEvalError); cyclic {
				panic(r)
			}
			result = NewErrorValue()
		}
	}()
	return e.fn(args)
}
