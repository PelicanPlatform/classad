package classad

import (
	"strings"

	"github.com/PelicanPlatform/classad/ast"
)

// This file exposes a small, thin surface over the evaluator so an external
// bytecode interpreter (collections/vm) can reuse the exact value-operation
// semantics of the tree-walking evaluator instead of reimplementing ClassAd's
// three-valued logic. Every function here delegates to existing evaluator
// internals; it adds no new semantics. Keeping the interpreter's value ops
// routed through this surface is what guarantees vm/evaluator parity.

// ApplyBinaryOp applies a binary operator to two already-evaluated operand
// values, returning the same result the tree-walking evaluator would for the
// corresponding *ast.BinaryOp. It covers arithmetic, comparison, bitwise, the
// meta-equality operators ("is"/"isnt"), and the logical operators ("&&"/"||").
//
// For "&&"/"||" the caller is responsible for short-circuiting (see
// ShortCircuit): when both operands are supplied here, the full three-valued
// truth table is applied, which is correct only if the caller already decided
// the right operand had to be evaluated.
func (e *Evaluator) ApplyBinaryOp(op string, left, right Value) Value {
	return e.applyBinaryValues(op, left, right)
}

// ApplyUnaryOp applies a unary operator to an already-evaluated operand value.
func (e *Evaluator) ApplyUnaryOp(op string, operand Value) Value {
	return e.applyUnaryValue(op, operand)
}

// ResolveRef resolves an attribute reference (with the given scope) in the
// evaluator's ClassAd, exactly as the evaluator resolves an *ast.AttributeReference:
// scope-chain search for an unscoped name, MY/TARGET/PARENT handling, evaluation
// of the referenced attribute's expression, and cyclic-reference detection.
func (e *Evaluator) ResolveRef(name string, scope ast.AttributeScope) Value {
	return e.evaluateAttributeReference(ast.NewAttributeReference(name, scope))
}

// ResolveRefNode is ResolveRef for a reference node the caller already has, so a compiled program
// can hold one per reference instead of building a fresh node per evaluation.
//
// NewAttributeReference heap-allocates and calls strings.ToLower, and ResolveRef does both on EVERY
// resolution -- once per reference per record during a scan. That defeats the precomputed-fold
// optimization resolveAttributeReference documents (the folded name is meant to be computed at parse
// time), and for a custom resolver the fold is pure waste, since the resolver is handed the
// original-cased name and never sees the folded one.
//
// ref must not be mutated by the caller after this returns; evaluation treats it as read-only, which
// is what lets one node be shared by every evaluation of a program, including concurrent ones.
func (e *Evaluator) ResolveRefNode(ref *ast.AttributeReference) Value {
	return e.evaluateAttributeReference(ref)
}

// SetScope rebinds the evaluator to a new scope ClassAd and resets its recursion
// depth, so one Evaluator can be reused across many evaluations (e.g. a bytecode
// Matcher scanning a collection) instead of allocating a fresh one per ad.
//
// Cyclic-reference state is tracked on the Evaluator, and each attribute
// evaluation pops its own marker on the way out (even when a cyclic panic
// unwinds), so nothing carries over between scopes; the reset of it and of depth
// here is a defensive guarantee. The Evaluator is not safe for concurrent use, but
// the ads it evaluates are only read, so many Evaluators may share them.
func (e *Evaluator) SetScope(ad *ClassAd) {
	e.classad = ad
	e.depth = 0
	for e.nActive > 0 {
		e.popActive()
	}
}

// SetResolver installs (or clears, with nil) a custom attribute resolver. While
// set, every attribute reference the evaluator resolves is delegated to fn(name,
// scope) instead of being looked up in the ClassAd scope, so a bytecode program
// can be evaluated against an alternate backing (e.g. an encoded ad) without
// materializing a ClassAd. fn should return the attribute's value, or undefined
// if absent. The Evaluator is not safe for concurrent use.
//
// CurrentTime (IsCurrentTimeName) needs care, because whether the ad defines it
// matters, not only its value. The evaluator reads undefined from fn for an unscoped
// or MY CurrentTime as "the ad does not define it" and substitutes the time, so fn
// must not return undefined for a CurrentTime the ad defines as undefined. For TARGET
// and PARENT the evaluator returns fn's value as is, so fn must not return undefined
// for a CurrentTime a target or parent that exists lacks; that one is the time. A
// resolver that cannot give the value must make its caller evaluate the ad the
// ordinary way instead.
func (e *Evaluator) SetResolver(fn func(name string, scope ast.AttributeScope) Value) {
	e.resolver = fn
}

// ShortCircuit reports whether the left operand of a logical operator already
// determines the result without evaluating the right operand, mirroring the
// evaluator's short-circuit rules:
//
//	"&&": left false  -> false ; left error -> error
//	"||": left true   -> true  ; left error -> error
//
// If done is true, result is the operator's value and the right operand must not
// be evaluated. If done is false, the caller must evaluate the right operand and
// combine with ApplyBinaryOp. This lets the interpreter reproduce short-circuit
// behaviour (including its interaction with cyclic references) exactly.
func (e *Evaluator) ShortCircuit(op string, left Value) (result Value, done bool) {
	switch logicalView(left) {
	case lsErr:
		return NewErrorValue(), true
	case lsFalse:
		if op == "&&" {
			return NewBoolValue(false), true
		}
	case lsTrue:
		if op == "||" {
			return NewBoolValue(true), true
		}
	}
	return Value{}, false
}

// AST returns the underlying ast.ClassAd. It is intended for serialization
// layers (e.g. collections/wire) that need the attribute expressions directly.
// The returned value aliases the ClassAd's internal storage and must not be
// mutated; attributes are sorted by normalized name. When the ad's own attribute
// list is not in that order, the result is a sorted copy of it (built once, and
// shared by later calls until the ad is written).
func (c *ClassAd) AST() *ast.ClassAd {
	if c == nil || c.ad == nil {
		return nil
	}
	return c.canonical()
}

// FromAST wraps an ast.ClassAd in a ClassAd. The ast.ClassAd is adopted (not
// copied): FromAST may reorder its attributes into sorted order and collapse duplicate
// names, and callers should not mutate it afterward. It is the inverse of AST for
// serialization layers that decode to an ast.ClassAd.
func FromAST(a *ast.ClassAd) *ClassAd {
	return adopt(a)
}

// FoldConstants returns e with its constant sub-expressions pre-computed
// (e.g. Memory > 2048*1024 becomes Memory > 2147483648), evaluating against an
// empty scope so attribute references are left intact. It is a thin bridge over
// the flattener (see ClassAd.Flatten) for query planners that pattern-match
// normalized expressions. Returns nil for a nil input.
//
// The result evaluates exactly as e does in any scope. That is why CurrentTime is
// left as a reference: its value is the scope's own CurrentTime when the scope
// defines one, so it is not a constant (see FoldConstantsAt).
func FoldConstants(e ast.Expr) ast.Expr {
	if e == nil {
		return nil
	}
	return (&flattener{c: New(), time: timeKeep, keepLists: true}).flatten(e)
}

// FoldConstantsAt is FoldConstants that also folds the magic CurrentTime attribute
// (an unscoped or MY reference) to now. The result evaluates as e does in a scope
// that does not define CurrentTime, at time now; in a scope that defines it, it may
// not. A query planner uses it to turn `CompletionDate > CurrentTime - 86400` into a
// range probe.
func FoldConstantsAt(e ast.Expr, now int64) ast.Expr {
	if e == nil {
		return nil
	}
	return (&flattener{c: New(), time: timeFixed, now: now, keepLists: true}).flatten(e)
}

// IsCurrentTimeRef reports whether ref names the magic CurrentTime attribute: an
// unscoped or MY reference, which evaluates to the current time when the scope does
// not define CurrentTime.
func IsCurrentTimeRef(ref *ast.AttributeReference) bool { return isCurrentTimeRef(ref) }

// PinCurrentTime makes a CurrentTime reference the scope does not define evaluate
// to now rather than to the clock's current reading, so every evaluation of a
// compiled query sees one time. now == 0 restores reading the clock. An ad that
// defines CurrentTime still supplies its own value.
func (e *Evaluator) PinCurrentTime(now int64) {
	e.now = now
}

// RecoverCyclic converts a cyclic-reference panic raised during evaluation into
// an error value, matching the tree-walker's top-level entry points. Use it as
// `defer classad.RecoverCyclic(&result)` around a bytecode run so a cyclic
// reference resolves to error rather than crashing.
//
// This repeats recoverCyclic's body rather than calling it. recover() only
// stops a panic when the deferred function calls it DIRECTLY, and recoverCyclic
// cannot be inlined (the compiler refuses to inline any function calling
// recover), so delegating left recover() one frame too deep and it returned
// nil. Go <=1.25 papered over that; 1.26 does not, which turned every cyclic or
// over-deep expression reaching a vm entry point into a process-killing panic
// instead of an error value.
func RecoverCyclic(result *Value) {
	if r := recover(); r != nil {
		if _, ok := r.(cyclicEvalError); ok {
			*result = NewErrorValue()
			return
		}
		panic(r)
	}
}

// CompareStringsFold compares two strings the way every ClassAd string comparison operator does:
// case-insensitively. All six of <, <=, >, >=, == and != route through it, while the identity
// operators =?= and =!= deliberately do not -- they are case SENSITIVE.
//
// Exported for the vectorized executor, which compares a whole column at a time and so cannot afford
// to box each element and re-dispatch on the operator, but must still produce exactly what the
// tree-walking evaluator produces. Sharing the function is what makes that a fact rather than a hope.
func CompareStringsFold(a, b string) int { return compareStringsFold(a, b) }

// nonStrictFunctions are the built-ins that CANNOT be compiled to a call on
// pre-evaluated argument values, each for one of three reasons.
//
// LAZY -- the function decides whether to evaluate an argument at all, so eager
// evaluation would change the result:
//
//	ifThenElse  evaluates only the selected branch, so ifThenElse(1, 0, B) is 0
//	            even when B is self-referential (evaluateIfThenElse).
//	strcat      stops at the first undefined/error argument, so strcat(undefined,
//	            A2) with a cyclic A2 is undefined, not error (evaluateStrcat).
//
// SCOPE OR AST -- the function needs something an argument VALUE cannot carry:
//
//	eval               parses a runtime string and evaluates it in the current
//	                   scope; its read set is not statically visible at all,
//	                   which is also why ReadPlan.PartialSafe goes false for it.
//	unparse            needs the argument's raw AST, not its value.
//	countMatches       evaluate their first argument once per nested ad in the
//	evalInEachContext  second, so they need the argument EXPRESSION and a scope.
//
// NONDETERMINISTIC -- the function returns a different value each call, so the
// scalar and vector executors could not be differentially tested against each
// other, and a vectorized call would differ per element in a way a per-record
// call does not:
//
//	random      draws from the global PRNG.
//	time        reads the wall clock.
//	formatTime  reads the wall clock when called with no argument, or with an
//	            undefined first argument (builtinFormatTime).
//
// Everything else in functionArity is strict, pure, and a function of its
// argument values alone. Membership here is the single source of truth for that
// split: evaluateFunctionCall special-cases exactly the lazy/scope/AST entries
// before reaching applyBuiltin, and StrictFunction refuses exactly this set.
var nonStrictFunctions = map[string]bool{
	// lazy
	"ifthenelse": true, "strcat": true,
	// scope or AST
	"eval": true, "unparse": true, "countmatches": true, "evalineachcontext": true,
	// nondeterministic
	"random": true, "time": true, "formattime": true,
}

// StrictFunction reports whether a call to name with argc arguments can be
// compiled to an ApplyFunction call on pre-evaluated argument values -- that is,
// whether the function is known, accepts that argument count, and is strict,
// pure and independent of the calling scope.
//
// A compiler must treat false as "delegate this call to the tree-walking
// evaluator", never as "this is an error". A false answer covers three different
// situations that happen to share a handling: a lazy or scope-dependent built-in
// (see nonStrictFunctions), an unknown function name, and a known function
// called with the wrong number of arguments. The last two produce ERROR *without
// evaluating the arguments*, which a compiled call cannot reproduce, because
// evaluateFunctionCall checks arity before it evaluates anything -- so pow(A0)
// with a self-referential A0 is error rather than a cyclic-reference failure.
// Delegating gets all three right by construction.
//
// Names are matched case-insensitively, as the reference engine matches them.
//
// A registered USER-DEFINED function is accepted on the same terms as a built-in, which is what
// makes a UDF compile natively and stay eligible for the columnar accelerators. Built-ins are
// consulted first; RegisterFunction refuses a name that collides with one, so the order only
// matters as a statement of precedence.
func StrictFunction(name string, argc int) bool {
	folded := strings.ToLower(name)
	if nonStrictFunctions[folded] {
		return false
	}
	if arity, known := functionArity[folded]; known {
		return arity.accepts(argc)
	}
	if u, ok := lookupUDF(folded); ok {
		return u.arity.accepts(argc)
	}
	return false
}

// ApplyFunction calls a strict built-in on already-evaluated argument values,
// returning the same value the tree-walking evaluator would for the equivalent
// *ast.FunctionCall. It is the function-call counterpart of ApplyBinaryOp: the
// one place a compiled executor gets built-in semantics from, so it cannot
// diverge from classad.Eval by having its own copy.
//
// e is used only to materialize a lazy LIST argument and never to resolve a free
// attribute reference (see applyBuiltin). Pass the evaluator the arguments were
// evaluated with, which is what makes the result identical to evaluating the call
// as an AST node. A caller that cannot have one may pass a scope-less evaluator
// if and only if no argument can be a list -- then the ten list-consuming
// built-ins return ERROR without consulting it.
//
// The caller must have established StrictFunction(name, len(args)) first. Calling
// it for a name that fails that predicate does not panic -- a lazy built-in is
// simply applied eagerly and an unknown one returns ERROR -- but the result is
// then not necessarily what evaluating the expression would have produced, which
// is the whole property this surface exists to preserve.
func ApplyFunction(e *Evaluator, name string, args []Value) Value {
	folded := strings.ToLower(name)
	// Gated on the registry being non-empty, which it is in almost every process. This runs once per
	// call PER RECORD, so a process that registered nothing must not pay a map lookup for the
	// possibility: with no registrations the whole check is one atomic load of a nil pointer.
	if udfs.Load() != nil {
		if _, isBuiltin := functionArity[folded]; !isBuiltin {
			if u, ok := lookupUDF(folded); ok {
				return callUDF(u, args)
			}
		}
	}
	return applyBuiltin(e, folded, args)
}

// CondState is how a condition value selects a branch, as the conditional
// constructs classify it.
type CondState uint8

const (
	CondTrue  CondState = iota // a boolean true, or a nonzero integer/real
	CondFalse                  // a boolean false, or a zero integer/real
	CondUndef                  // UNDEFINED
	CondError                  // ERROR, or any non-coercible type (string, list, ad)
)

// ConditionState classifies a condition value exactly as evaluateIfThenElse and
// evaluateConditional classify it, by delegating to the same logicalView the
// evaluator uses. A compiled executor needs this to lower a conditional to a
// branch without reimplementing the coercion -- "1 ? a : b" selects a, and a
// string condition is an error, neither of which is obvious.
//
// What a caller does with each state is NOT the same for the two constructs, and
// the difference is not visible from here:
//
//	ifThenElse(c, t, f)  CondUndef yields UNDEFINED and evaluates NEITHER branch.
//	c ? t : f            CondUndef yields UNDEFINED but still evaluates BOTH
//	                     branches, because an error value in a branch is absorbed
//	                     while a cyclic self-reference must still propagate.
//
// Both yield ERROR for CondError without evaluating either branch.
func ConditionState(v Value) CondState {
	switch logicalView(v) {
	case lsTrue:
		return CondTrue
	case lsFalse:
		return CondFalse
	case lsUndef:
		return CondUndef
	default:
		return CondError
	}
}

// SplitStringList splits a delimited string list exactly as the stringList
// built-ins split it: on any rune in delimiter, with each element trimmed of
// surrounding space, and with ", " as the delimiter set when delimiter is empty.
//
// It exists so a compiler can split a CONSTANT list once instead of per record,
// without reimplementing the split. stringListMember re-splits its list argument
// on every call, which for a literal list is the same work repeated for every ad
// in a scan.
func SplitStringList(list, delimiter string) []string {
	return parseStringList(list, delimiter)
}

// StringListArg coerces a value to the string a stringList built-in treats it
// as: a string is itself, UNDEFINED is the empty string, and anything else is
// not usable (ok=false), which those built-ins report as ERROR.
//
// Note an undefined item does NOT match a list that looks like it has an empty
// element: the split uses strings.FieldsFunc, which drops empty fields, so
// "a,,b" is two elements and stringListMember(undefined, "a,,b") is FALSE. The
// empty string is only ever found in a list that is itself empty of elements,
// which no list is. That is worth stating because the opposite is the obvious
// guess.
func StringListArg(v Value) (string, bool) {
	return stringListStrArg(v)
}
