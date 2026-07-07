package classad

import "github.com/PelicanPlatform/classad/ast"

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
	return e.evaluateAttributeReference(&ast.AttributeReference{Name: name, Scope: scope})
}

// SetScope rebinds the evaluator to a new scope ClassAd and resets its recursion
// depth, so one Evaluator can be reused across many evaluations (e.g. a bytecode
// Matcher scanning a collection) instead of allocating a fresh one per ad.
//
// Cyclic-reference state is tracked per ClassAd (in ClassAd.evaluating), not on
// the Evaluator, and each attribute evaluation removes its own marker on the way
// out, so nothing carries over between scopes. Depth is balanced by Evaluate's
// deferred decrement even when a cyclic panic unwinds, so it is 0 here after any
// completed or recovered evaluation; the reset is a defensive guarantee. The
// Evaluator is not safe for concurrent use.
func (e *Evaluator) SetScope(ad *ClassAd) {
	e.classad = ad
	e.depth = 0
}

// SetResolver installs (or clears, with nil) a custom attribute resolver. While
// set, every attribute reference the evaluator resolves is delegated to fn(name,
// scope) instead of being looked up in the ClassAd scope, so a bytecode program
// can be evaluated against an alternate backing (e.g. an encoded ad) without
// materializing a ClassAd. fn should return the attribute's value, or undefined
// if absent. The Evaluator is not safe for concurrent use.
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
// mutated; attributes are sorted by normalized name.
func (c *ClassAd) AST() *ast.ClassAd {
	if c == nil {
		return nil
	}
	c.ensureSorted()
	return c.ad
}

// FromAST wraps an ast.ClassAd in a ClassAd. The ast.ClassAd is adopted (not
// copied); callers should not mutate it afterward. It is the inverse of AST for
// serialization layers that decode to an ast.ClassAd.
func FromAST(a *ast.ClassAd) *ClassAd {
	if a == nil {
		a = &ast.ClassAd{}
	}
	obj := &ClassAd{ad: a, attrsDirty: true}
	obj.rebuildIndex()
	return obj
}

// FoldConstants returns e with its constant sub-expressions pre-computed
// (e.g. Memory > 2048*1024 becomes Memory > 2147483648), evaluating against an
// empty scope so attribute references are left intact. It is a thin bridge over
// the flattener (see ClassAd.Flatten) for query planners that pattern-match
// normalized expressions. Returns nil for a nil input.
func FoldConstants(e ast.Expr) ast.Expr {
	if e == nil {
		return nil
	}
	return New().flattenExpr(e)
}

// RecoverCyclic converts a cyclic-reference panic raised during evaluation into
// an error value, matching the tree-walker's top-level entry points. Use it as
// `defer classad.RecoverCyclic(&result)` around a bytecode run so a cyclic
// reference resolves to error rather than crashing.
func RecoverCyclic(result *Value) {
	recoverCyclic(result)
}
