package vm

import (
	"errors"
	"fmt"
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// A SCRIPT MODULE: a script that defines functions and produces nothing.
//
// A module and a WebAssembly module are the same kind of thing -- something that EXPORTS functions
// by name -- and that is the whole reason this exists. `wasmudf` compiles a guest, picks an export
// by name and registers it into classad's registry; a module does the same with a `def`. A caller
// wiring extensions into a process hands either one to the same adapter.
//
// TWO THINGS A SCRIPT GETS RIGHT THAT A GUEST CANNOT, and they are why a module is not simply a
// second spelling of the same thing:
//
//   - ARITY COMES FROM THE SOURCE. `def pctUsed(used, total)` states its own arity, so there is no
//     min/max for a caller to get wrong. A wasm export's signature says only how many i32s it
//     takes, which is why wasmudf.Register has to be told.
//
//   - PURITY IS CHECKED RATHER THAN PROMISED. A guest is pure because the ABI hands it values and
//     nothing else; there is no way for it to reach the ad, so nothing needs checking. A script
//     function CAN reach the ad -- a bare name the body does not bind is an attribute, which is
//     exactly what makes a script function useful inside a script -- so whether a particular one may
//     be registered is a question the compiler answers. See checkPure.
//
// WHAT A MODULE MAY CONTAIN: `def` and nothing else at the top level. Not a `result`, because a
// module is not run; and not a `let` either, though that one is worth saying why. A top-level
// binding would have to be computed at some moment, and a module has no such moment: there is no
// run of the module, only runs of the functions it exports, each with its own registers. Accepting
// `shared = expensive()` and then either recomputing it inside every function or never computing it
// at all are both answers the author did not ask for, so the binding is refused and the author
// writes a `def`.
//
// A FUNCTION IS COMPILED ON ITS OWN, not as part of a run of something larger. The body is expanded
// exactly as a call inside a script expands it -- same inliner, same scope rules, same rule on
// recursion -- but the parameters are filled from argument VALUES handed in by the caller instead of
// from argument expressions emitted by a caller's code. That difference is three lines in the
// inliner and nothing else: everything a script function means is already settled in scriptfunc.go
// and a module does not get to restate any of it.

// ErrNoFunctions is returned by ParseScriptModule for source that defines nothing. An empty module
// is almost always a mistake about which entry point was meant to be built, and registering nothing
// silently is a start-up that looks successful and leaves every expression calling a name that does
// not exist.
var ErrNoFunctions = errors.New("vm: script module defines no functions")

// ErrReadsAd is returned when a script function cannot be registered because it reaches the subject
// ad. See checkPure for what that covers and why each part of it is refused.
var ErrReadsAd = errors.New("vm: script function reaches the ad and cannot be registered")

// ScriptModule is parsed module source: a set of named functions, each compilable on its own.
//
// Parsing does not compile. A module whose functions include one that cannot be registered is still
// a usable module, because the refusal belongs to the function being registered and not to its
// siblings -- and because which functions a caller wants is the caller's business.
type ScriptModule struct {
	pos   *posIndex
	defs  []*defStmt
	byFn  map[string]*defStmt // folded name
	names []string            // as written, in source order
}

// ParseScriptModule parses ClassAd Script source that defines functions and produces no value.
//
// The language is the one ParseScript parses, restricted to `def` at the top level. Inside a body
// nothing changes at all: the same statements, the same expressions, the same parser.
func ParseScriptModule(src string) (*ScriptModule, error) {
	p := &scriptParser{pos: newPosIndex(src)}
	lines, clean, err := scanLines(src, p.pos)
	if err != nil {
		return nil, err
	}
	p.clean = clean
	p.lines = lines
	stmts, err := p.parseModuleTop()
	if err != nil {
		return nil, err
	}
	m := &ScriptModule{pos: p.pos, byFn: make(map[string]*defStmt, len(stmts))}
	for _, st := range stmts {
		d, isDef := st.(*defStmt)
		if !isDef {
			// parseModuleTop accepts nothing else at the top level, so this cannot happen. It is
			// checked rather than asserted because the alternative is a panic in a library.
			return nil, fmt.Errorf("vm: script module: %T at the top level", st)
		}
		folded := strings.ToLower(d.name)
		if prev, dup := m.byFn[folded]; dup {
			return nil, p.pos.errorAt(d.at(), fmt.Sprintf("%q is defined twice in this module; "+
				"it was already defined as %q", d.name, prev.name))
		}
		if classad.BuiltinFunctionName(d.name) {
			// Refused here rather than at registration, and refused for the whole module rather
			// than for the one function: a sibling calling `strcat` must not reach a `def` instead
			// of the built-in, and whether a name is a built-in does not change while the process
			// runs. Whether the name is already REGISTERED is a different question with a different
			// answer over time, and it is asked at registration; see Script.defineFunc.
			return nil, p.pos.errorAt(d.at(), fmt.Sprintf("%q is a built-in function and a module "+
				"may not define one: a call inside the module would mean something other than the "+
				"same call outside it", d.name))
		}
		m.byFn[folded] = d
		m.defs = append(m.defs, d)
		m.names = append(m.names, d.name)
	}
	return m, nil
}

// Functions returns the module's exported function names, as written, in source order.
func (m *ScriptModule) Functions() []string {
	return append([]string(nil), m.names...)
}

// Compile compiles one of the module's functions into a form that can be called with argument
// values. The name folds case, as a call does.
//
// The function may call the module's other functions; the inliner expands those exactly as it does
// inside a script, so recursion is treated here exactly as it is treated there: a call to itself in
// tail position is a jump back to the entry, and every other cycle is refused by naming it.
//
// It also applies the purity gate. A function that reaches the ad is refused HERE rather than at
// registration because the compiled form has no ad to reach: there is no subject, so an attribute
// read has no answer that could be right. Registration is where that refusal matters, and it is not
// where it is discovered.
func (m *ScriptModule) Compile(name string) (*ScriptFunc, error) {
	d := m.byFn[strings.ToLower(name)]
	if d == nil {
		return nil, fmt.Errorf("vm: script module: no function named %q; it exports %s",
			name, strings.Join(m.names, ", "))
	}

	s := NewScript()
	e := &emitter{pos: m.pos}
	for _, def := range m.defs {
		before := s.err
		s.defineFunc(def.name, def.params, func(sc *Script) { e.block(sc, def.body) }, false)
		if before == nil && s.err != nil {
			return nil, m.pos.errorAt(def.at(), detailOf(s.err))
		}
	}

	params, err := s.expandEntry(s.funcs[strings.ToLower(name)])
	if err != nil {
		return nil, err
	}
	if e.err != nil {
		return nil, e.err
	}
	sp, err := s.compile(params)
	if err != nil {
		return nil, err
	}
	f := &ScriptFunc{
		name:      d.name,
		arity:     len(d.params),
		sp:        sp,
		paramRegs: make([]int32, len(params)),
		budget:    DefaultStepBudget,
	}
	for i, local := range params {
		f.paramRegs[i] = sp.rp.localRegs[local]
	}
	if err := f.checkPure(); err != nil {
		return nil, err
	}
	return f, nil
}

// expandEntry expands fd as the whole of this script: its parameters are locals nothing stores to,
// and its return value is the script's result.
//
// The emitting flag is set for the same reason the inliner relies on it inside a call: the body's
// statements must be compiled NOW, into this expansion's scope, rather than staged for a flush that
// would run them after the scope has been swapped back.
func (s *Script) expandEntry(fd *funcDef) ([]int32, error) {
	s.flush()
	if s.err != nil {
		return nil, s.err
	}
	s.emitting = true
	params := s.inline(fd, nil)
	s.emitting = false
	if s.err != nil {
		return nil, s.err
	}
	s.hasRes = true
	return params, nil
}

// ScriptFunc is one of a module's functions, compiled on its own and callable with argument values.
//
// It is READ-ONLY once compiled and safe to call from any number of goroutines at once, which is
// not a convenience: a UDF is called from the parallel scan and from the vector executor, and
// `*classad.ClassAd` is explicitly not safe to share between goroutines. Nothing shared is written
// here -- each call allocates its own register file and its own evaluator, and the program, its
// constant pools and the parameter register list are written once at compile time.
type ScriptFunc struct {
	name      string
	arity     int
	sp        *ScriptProgram
	paramRegs []int32 // the register each parameter is filled into, in declaration order
	budget    int
}

// Name returns the function's name as the module spelled it.
func (f *ScriptFunc) Name() string { return f.name }

// Arity returns the number of parameters the `def` declares.
//
// It is the minimum AND the maximum: a `def` states its own arity, so unlike a Go or wasm
// registration there is no range for a caller to supply and get wrong.
func (f *ScriptFunc) Arity() int { return f.arity }

// Program returns the compiled program, for tests and diagnostics.
func (f *ScriptFunc) Program() *Program { return f.sp.Program() }

// Call applies the function to argument values.
//
// It returns a Value and never an error, because that is the shape a ClassAd function has: UNDEFINED
// and ERROR are values in this language and choosing between them is the function's job. The cases
// this function decides rather than the script are the two it can see and the script cannot:
//
//   - WRONG ARGUMENT COUNT is ERROR. A registration states its arity, so the engine rejects a call
//     of the wrong shape before reaching here; this covers a caller invoking Call directly.
//   - A STEP BUDGET THAT RUNS OUT is ERROR. A script may loop, so a registered function may loop,
//     and a registered function that hung would hang a scan over every record in a store rather
//     than fail one expression. There is no way to report "did not finish" through a Value, and
//     ERROR is the answer that propagates like any other failure.
func (f *ScriptFunc) Call(args []classad.Value) classad.Value {
	if len(args) != len(f.paramRegs) {
		return classad.NewErrorValue()
	}
	regs := make([]classad.Value, f.sp.rp.nregs)
	for i, r := range f.paramRegs {
		regs[r] = args[i]
	}
	// A scope-less evaluator, which is sound only because checkPure already established that this
	// program has no reference load and no delegated subtree: the evaluator is reached for operators
	// and for strict built-in calls over values, neither of which consults a scope. It is allocated
	// per call rather than shared, because an Evaluator carries per-evaluation state.
	result, _, _, _ := execReg(f.sp.rp, classad.NewEvaluator(nil), regs, f.budget)
	return result
}

// Func returns the function as a classad.Func, ready to register. It mirrors wasmudf's UDF.Func, so
// the two kinds of module reach the registry through the same shape.
func (f *ScriptFunc) Func() classad.Func { return f.Call }

// checkPure reports whether this function may be registered, and refuses it if it reaches the ad.
//
// WHY THERE IS A GATE AT ALL. classad.StrictFunction promises the compiler that a call to a
// registered name cannot make a compiled program disagree with the tree-walking evaluator about what
// an expression reads. A Go or wasm function keeps that promise structurally: it receives values, so
// there is nothing else it could read. A script function has a scope -- a bare name its body does
// not bind is an attribute -- so `def fits(requested, cap): return Optimistic` is a perfectly legal
// script function and an impossible registration. The compiled form has no subject ad at all, so the
// read has no answer; worse, an expression calling it would be planned as reading only what it names.
//
// THE RULES, each of which refuses a different way of reaching out, and all of which are properties
// of the COMPILED PROGRAM rather than of the source text. Reading them off the program is what makes
// them exhaustive: a name that turned out to be a parameter or a body local never appears, and a
// read the author did not write -- one the inliner produced -- does.
func (f *ScriptFunc) checkPure() error {
	p := f.sp.Program()

	// 1. THE READ SET, which is the same set the query planner is given: the unscoped attribute
	// names the program may read. Non-empty means the function reads the subject ad.
	if names := p.ReadAttrs(); len(names) != 0 {
		return fmt.Errorf("vm: %s reads the attribute %s: a registered function is given its "+
			"arguments and nothing else, so pass what it needs as a parameter: %w",
			f.name, strings.Join(names, ", "), ErrReadsAd)
	}

	// 2. A SCOPED REFERENCE, which rule 1 cannot see. `MY.RequestMemory` compiles to a native
	// reference load exactly as a bare name does, but the read set holds UNSCOPED names only -- a
	// scoped one names an ad the planner is not planning a scan over -- so a function whose only
	// contact with the ad is `MY.X` passes rule 1 and delegates nothing, and without this rule it
	// would register and then read an ad that is not there.
	for i := range p.refs {
		if p.refs[i].scope != ast.NoScope {
			return fmt.Errorf("vm: %s reads %s: a registered function has no subject ad to "+
				"resolve that against, so pass the value as a parameter: %w",
				f.name, p.refs[i].node.String(), ErrReadsAd)
		}
	}

	// 3. AN ALIAS LOAD, the joined-row counterpart of rule 2, refused for the same reason and
	// listed separately because it is a separate pool. A module is not compiled against an alias
	// table today, so this cannot fire; it is here because "cannot fire" is a property of the
	// current compile path and not of this function's contract.
	if len(p.aliasRefs) != 0 {
		return fmt.Errorf("vm: %s reads an aliased attribute, which a registered function has no "+
			"row to resolve against: %w", f.name, ErrReadsAd)
	}

	// 4. A DELEGATED SUBTREE: any expression the compiler handed to the tree-walking evaluator
	// instead of lowering natively. That evaluator resolves names against the ad and may be
	// nondeterministic, so a delegated node is a hole in every other rule here -- `random(10)` and
	// `self[0]` leave the read set empty and the reference pools empty and still do not belong in a
	// registration.
	//
	// This is the CONSERVATIVE rule, and deliberately so. It excludes nondeterminism and scope
	// access structurally rather than by enumerating which built-ins are nondeterministic and which
	// constructs can see a scope -- an enumeration that is wrong the first time either list grows.
	// The cost is that a few harmless expressions are refused too, which the message says how to
	// work around.
	if len(p.nodes) != 0 {
		// The refusal names what it FOUND rather than listing every construct that could have
		// caused one, so the author reads about the expression they wrote.
		return fmt.Errorf("vm: %s contains %s, which this compiler evaluates against the ad rather "+
			"than compiling: a registered function has no ad to evaluate it against, and that "+
			"evaluator need not be deterministic: %w",
			f.name, p.nodes[0].String(), ErrReadsAd)
	}
	return nil
}
