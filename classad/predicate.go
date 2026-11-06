package classad

import "github.com/PelicanPlatform/classad/ast"

// The predicate fast path: deciding `attribute OP literal` without building a Value.
//
// WHAT IT IS FOR. An ad stores Owner as *ast.StringLiteral{Value: "alice"} -- a Go
// string that already exists. Evaluating `Owner == "alice"` the ordinary way walks
// lookupNorm -> evalAttrExpr -> Evaluate -> NewStringValue, which wraps that string
// into a Value, and then ApplyBinaryOp immediately calls StringValue() to unwrap the
// same string back out. The same happens on the right: a literal node is boxed so the
// comparison can unbox it. Two Values are constructed and destroyed per comparison,
// and the operator itself is dispatched by switching on a Go string.
//
// Why that matters is not mainly cycles. It ties the number of Values built to the
// number of comparisons decided, with each Value built for exactly one comparison,
// and that is what defeats every scheme for making the COMPARISON cheaper by making
// the VALUE richer: an eager prefix in the Value relocates a cache miss to a
// construction that happens just as often; a lazy one defers work to a comparison
// that happens about once per construction; interning pays a cost per read and then
// throws the result away. None of those can win while nothing is ever read twice.
// The way out is not a better Value but no Value: read the operand in the form the
// AST already holds it and decide the comparison there.
//
// Measured over the committed pool corpus, before this path existed (see
// TestConstructionRatio, which is how to re-measure rather than re-argue): a job's
// Requirements against every machine ad built 3.93 Values per comparison it decided,
// a condor_status-style constraint 4.34, and a machine's own START policy 6.52. Not
// one of them below 1, and the surplus is the boolean and undefined results threaded
// between them, which are also built once and read once.
//
// THE DISCIPLINE, borrowed from the columnar executor one layer over
// (collections/vm/vecexec.go). That code specializes on the representations it can
// prove -- a dictionary-encoded column compared against a literal becomes an integer
// range test that never touches a character -- and returns ok=false for everything
// else, letting the caller take the ordinary path. It handles only the subset it can
// prove and declines the rest. This file does the same thing for the row-wise
// evaluator, and the decline is the important half: a wrong answer here decides
// matchmaking wrongly in production, while a decline costs nothing -- an operand this
// path cannot read as a literal is EVALUATED on the spot, from the resolution already
// done, so the shapes it does not handle do exactly the work they did before and not
// that work twice. The first version of this file did not do that, and paid 12% on an
// expression whose every comparison declines; see operandLiteral.
//
// WHAT IT HANDLES. Exactly `X OP Y` where OP is one of < <= > >= == != =?= =!= and
// each of X and Y is either a literal node or an attribute reference whose STORED
// expression is a literal node (the overwhelmingly common ad shape: Cpus = 8,
// Arch = "X86_64", Owner = "alice"). An attribute that is absent, in any scope, is
// undefined, which is also a literal operand and so also takes this path.
//
// WHAT IT DECLINES -- meaning it falls back to comparing two Values, which is what the
// evaluator always did -- and why each decline is necessary rather than merely
// conservative:
//
//   - A custom resolver (SetResolver) is installed. The resolver's contract is stated
//     in Values; it has no expression-level form, and for CurrentTime the evaluator
//     reads a resolver's undefined as "the ad does not define it". Nothing here can
//     reproduce that.
//   - The reference names CurrentTime. Whether the ad defines it changes the answer,
//     not just its value, and an undefined-but-defined CurrentTime is distinguishable
//     from an absent one. evaluateCurrentTime owns that; this file does not duplicate it.
//   - The attribute's stored expression is not a literal -- a reference, an arithmetic
//     expression, a function call, a parenthesized literal. This is the common decline.
//     It is also what keeps cyclic-reference detection intact: a cycle must run through
//     a non-literal expression, so a comparison this path answers cannot be part of one
//     and never needs evalAttrExpr's in-progress bookkeeping. That is the same argument
//     evalAttrExpr's own literal fast path already makes.
//   - Evaluation is close to maxEvalDepth. The ordinary path charges depth per AST node
//     and this one does not, so near the limit the two could disagree about whether an
//     expression is too deep to evaluate. Declining within a margin of the limit makes
//     that unreachable: the ordinary path then decides, as it always did.
//
// HOW && AND || COMPOSE. They are not handled here, and deliberately so. There is
// exactly one hook, in evaluateBinaryOp, and a conjunction reaches it once per
// conjunct through the short-circuit evaluation that already exists -- so
// `Cpus >= 2 && Arch == "X86_64" && State == "Unclaimed"` takes this path three
// times with no new recursion, no second copy of the short-circuit truth table, and
// no depth accounting of its own to keep in step with Evaluate's. A truth-only layer
// over && that consumed a logicalState instead of a Value would additionally save the
// one (cheap, pointer-free, non-allocating) boolean Value per conjunct, but it would
// have to re-attempt and re-decline every comparison the hook already declined --
// paying a second attribute lookup on exactly the shapes that are slowest already --
// and it would need its own depth guard. The boolean round trip is not worth either.
//
// All six comparison operators and both identity operators are reproduced here against
// the same rules the Value-level code uses -- error before undefined, int/real promotion
// with integers compared as int64 so 2^53+1 does not collapse, exact IEEE real equality,
// CASE-INSENSITIVE == and friends via compareStringsFold but CASE-SENSITIVE =?= / =!=.
// Those are stated once in the comparison functions below and are checked against the
// ordinary evaluator by the differential test in predicate_diff_test.go, which asserts
// that the two agree on every generated input rather than only on the ones a human
// thought to write down.

// cmpKind is a comparison operator decoded out of its source spelling once, so the
// per-evaluation path compares a byte instead of a Go string. The ordinary evaluator
// switches on op.Op (a string) twice per operator application; this path does it once.
type cmpKind uint8

const (
	cmpNone cmpKind = iota
	cmpLT
	cmpLE
	cmpGT
	cmpGE
	cmpEQ
	cmpNE
	cmpIs   // =?= , which the parser normalizes to "is"
	cmpIsnt // =!= , normalized to "isnt"
)

// comparisonKind maps an operator spelling to its cmpKind, or cmpNone for an
// operator this path does not handle (arithmetic, bitwise, && and ||).
func comparisonKind(op string) cmpKind {
	switch op {
	case "<":
		return cmpLT
	case "<=":
		return cmpLE
	case ">":
		return cmpGT
	case ">=":
		return cmpGE
	case "==":
		return cmpEQ
	case "!=":
		return cmpNE
	case "is":
		return cmpIs
	case "isnt":
		return cmpIsnt
	}
	return cmpNone
}

// litKind is the type of a literal operand. It maps one-to-one onto ValueType for
// the six types a literal can have, which is what lets identity comparison (=?=)
// test kinds where the Value path tests Type().
type litKind uint8

const (
	lkUndef litKind = iota
	lkError
	lkBool
	lkInt
	lkReal
	lkString
)

// litOperand is a comparison operand read directly out of the AST, in the form the
// AST already holds it: an int64, a float64, a Go string. It is the thing a Value
// would have been built to carry. It never escapes -- it is created, compared and
// dropped inside one call -- so it costs a few registers and no allocation, and it
// keeps the real as a float64 rather than as bits, because nothing here copies it
// onto a stack machine.
type litOperand struct {
	kind litKind
	num  int64   // lkInt's value; lkBool's 0 or 1
	flt  float64 // lkReal's value
	str  string  // lkString's value
}

// undefLit is the operand an absent attribute reads as, in any scope.
var undefLit = litOperand{kind: lkUndef}

// valueType is the ValueType the equivalent Value would report. Equality uses it
// where valuesEqual uses Value.Type(), so the two agree on what "different types"
// means -- notably that a boolean and an integer ARE different types and therefore
// take the cross-type numeric-coercion branch.
func (l litOperand) valueType() ValueType {
	switch l.kind {
	case lkUndef:
		return UndefinedValue
	case lkError:
		return ErrorValue
	case lkBool:
		return BooleanValue
	case lkInt:
		return IntegerValue
	case lkReal:
		return RealValue
	default:
		return StringValue
	}
}

// literalOperand decodes a literal AST node into an operand. ok is false for
// anything else, which is the signal to decline.
//
// The accepted set is exactly isLiteralExpr's: the nodes that reference nothing and
// so can neither depend on the scope nor take part in a cyclic reference. A literal
// wrapped in parentheses is deliberately not unwrapped, for the same reason
// isLiteralExpr does not: keeping the test a single type switch is worth more than
// the rare `(5)`, and the ordinary path handles it correctly.
func literalOperand(e ast.Expr) (litOperand, bool) {
	switch v := e.(type) {
	case *ast.IntegerLiteral:
		return litOperand{kind: lkInt, num: v.Value}, true
	case *ast.RealLiteral:
		return litOperand{kind: lkReal, flt: v.Value}, true
	case *ast.StringLiteral:
		return litOperand{kind: lkString, str: v.Value}, true
	case *ast.BooleanLiteral:
		if v.Value {
			return litOperand{kind: lkBool, num: 1}, true
		}
		return litOperand{kind: lkBool, num: 0}, true
	case *ast.UndefinedLiteral:
		return litOperand{kind: lkUndef}, true
	case *ast.ErrorLiteral:
		return litOperand{kind: lkError}, true
	}
	return litOperand{}, false
}

// numericLit is numericOperand for a litOperand, and must stay identical to it: a
// boolean coerces to 1/0 and counts as an INTEGER (so true < 2 compares as integers
// and keeps an integer result type), a real is never an integer, and nothing else is
// numeric at all.
func numericLit(l litOperand) (isNum, isInt bool, iv int64, rv float64) {
	switch l.kind {
	case lkInt, lkBool:
		return true, true, l.num, float64(l.num)
	case lkReal:
		return true, false, 0, l.flt
	}
	return false, false, 0, 0
}

// boolState is the logicalState for a decided boolean result.
func boolState(b bool) logicalState {
	if b {
		return lsTrue
	}
	return lsFalse
}

// cmpOrderedInt applies an ordering operator to two int64s. Integers are compared as
// int64 rather than promoted to float64 for the reason doComparison does it: both
// 9007199254740992 and 9007199254740993 round to 2^53 as doubles, so the float
// comparison would call them equal.
func cmpOrderedInt(c cmpKind, a, b int64) bool {
	switch c {
	case cmpLT:
		return a < b
	case cmpLE:
		return a <= b
	case cmpGT:
		return a > b
	default: // cmpGE
		return a >= b
	}
}

// cmpOrderedFloat applies an ordering operator to two float64s. Go's own operators
// give the IEEE answers the reference engine's C++ ones give for NaN (every ordering
// comparison false), for the infinities, and for -0.0 == 0.0.
func cmpOrderedFloat(c cmpKind, a, b float64) bool {
	switch c {
	case cmpLT:
		return a < b
	case cmpLE:
		return a <= b
	case cmpGT:
		return a > b
	default: // cmpGE
		return a >= b
	}
}

// cmpOrderedSign applies an ordering operator to a three-way comparison result, for
// strings, whose comparison is already reduced to -1/0/1 by compareStringsFold.
func cmpOrderedSign(c cmpKind, sign int) bool {
	switch c {
	case cmpLT:
		return sign < 0
	case cmpLE:
		return sign <= 0
	case cmpGT:
		return sign > 0
	default: // cmpGE
		return sign >= 0
	}
}

// orderedLiterals is evaluateLessThan and its three siblings, whose bodies are
// identical apart from the Go operator: undefined propagates, two numbers promote
// (integers staying integers), two strings compare case-insensitively, and every
// other pairing -- including a number against a string -- is an error rather than
// false. Errors are handled by the caller, before this, exactly as applyBinaryValues
// handles them before dispatching to those four.
func orderedLiterals(c cmpKind, l, r litOperand) logicalState {
	if l.kind == lkUndef || r.kind == lkUndef {
		return lsUndef
	}
	if ln, li, liv, lrv := numericLit(l); ln {
		if rn, ri, riv, rrv := numericLit(r); rn {
			if li && ri {
				return boolState(cmpOrderedInt(c, liv, riv))
			}
			return boolState(cmpOrderedFloat(c, lrv, rrv))
		}
	}
	if l.kind == lkString && r.kind == lkString {
		return boolState(cmpOrderedSign(c, compareStringsFold(l.str, r.str)))
	}
	return lsErr
}

// equalLiterals is valuesEqual: undefined propagates even when both sides are
// undefined (that case belongs to =?=, not ==); different types coerce only when both
// are numeric, and are otherwise an ERROR rather than false -- so `Owner == 3` is
// error, which surprises everyone who has not read coerceToNumber; and same-typed
// values compare by type, with strings folded and reals compared with exact IEEE
// equality (no tolerance, so 0.1 + 0.2 == 0.3 is false).
func equalLiterals(l, r litOperand) logicalState {
	if l.kind == lkUndef || r.kind == lkUndef {
		return lsUndef
	}
	if l.valueType() != r.valueType() {
		ln, li, liv, lrv := numericLit(l)
		rn, ri, riv, rrv := numericLit(r)
		if ln && rn {
			if li && ri {
				return boolState(liv == riv)
			}
			return boolState(lrv == rrv)
		}
		return lsErr
	}
	switch l.kind {
	case lkBool, lkInt:
		return boolState(l.num == r.num)
	case lkReal:
		return boolState(l.flt == r.flt)
	case lkString:
		return boolState(compareStringsFold(l.str, r.str) == 0)
	}
	// Two errors, which applyBinaryValues' error gate catches before == is ever
	// reached; valuesEqual's own default arm says the same thing.
	return lsErr
}

// identicalLiterals is evaluateIs: strict identity with no coercion at all.
// Different types are FALSE (not error, and not a numeric promotion -- true =?= 1 is
// false where true == 1 is true), undefined is undefined and error is error, and
// strings compare CASE-SENSITIVELY. That last one is the opposite of ==, and getting
// the two backwards is the single easiest way to break matchmaking silently, so the
// difference is spelled out at both call sites rather than inferred from one.
func identicalLiterals(l, r litOperand) bool {
	if l.kind != r.kind {
		return false
	}
	switch l.kind {
	case lkUndef, lkError:
		return true
	case lkBool, lkInt:
		return l.num == r.num
	case lkReal:
		return l.flt == r.flt
	default:
		return l.str == r.str
	}
}

// compareLiterals applies a comparison operator to two literal operands and returns
// the truth of the result, in the three-valued-plus-error form && and || consume.
//
// The operator groups are ordered as applyBinaryValues orders them: the identity
// operators do NOT propagate errors generically (error =?= error is true), so they
// are answered before the error gate; everything else is an error if either side is.
func compareLiterals(c cmpKind, l, r litOperand) logicalState {
	switch c {
	case cmpIs:
		return boolState(identicalLiterals(l, r))
	case cmpIsnt:
		return boolState(!identicalLiterals(l, r))
	}
	if l.kind == lkError || r.kind == lkError {
		return lsErr
	}
	switch c {
	case cmpEQ:
		return equalLiterals(l, r)
	case cmpNE:
		// evaluateNotEqual negates == only when == decided; undefined and error
		// pass through unnegated.
		switch st := equalLiterals(l, r); st {
		case lsTrue:
			return lsFalse
		case lsFalse:
			return lsTrue
		default:
			return st
		}
	default:
		return orderedLiterals(c, l, r)
	}
}

// predicateDepthMargin is how far from maxEvalDepth this path stops answering.
//
// The ordinary path charges one depth unit per AST node it visits, and for the shape
// handled here it visits at most two below the comparison node (the reference, then
// the literal the reference is bound to). This path visits none of them, so within a
// couple of levels of the limit the two could disagree about whether an expression is
// too deep -- one returning a comparison result where the other raises the cyclic
// sentinel. Declining with a margin wider than the deepest node count makes that
// disagreement unreachable without any arithmetic that has to be re-derived whenever
// the ordinary path's node visits change.
const predicateDepthMargin = 8

// toValue boxes a literal operand into the Value the ordinary path would have built
// for it. It is needed when ONE side of a comparison is literal-backed and the other
// is not: the comparison then has to be decided at the Value level after all, and the
// literal side has to arrive there looking exactly as Evaluate would have left it.
func (l litOperand) toValue() Value {
	switch l.kind {
	case lkInt:
		return NewIntValue(l.num)
	case lkReal:
		return NewRealValue(l.flt)
	case lkString:
		return NewStringValue(l.str)
	case lkBool:
		return NewBoolValue(l.num != 0)
	case lkError:
		return NewErrorValue()
	default:
		return NewUndefinedValue()
	}
}

// predicateEligible reports whether the fast path may answer at the evaluator's
// current recursion depth. See predicateDepthMargin.
func (e *Evaluator) predicateEligible() bool {
	return e.depth+predicateDepthMargin < maxEvalDepth
}

// operandLiteral reads one side of a comparison as a literal operand: a literal node
// directly, or an attribute reference whose bound expression is one.
//
// When it cannot -- the common case being a reference bound to a computed expression
// -- it writes the operand's VALUE through val and returns ok=false. That is the
// difference between this and a predicate that merely declines, and it is not a
// convenience: an earlier version returned ok=false and left the caller to evaluate
// the operand from scratch, which repeated the attribute lookup this function had
// already done. It cost 12% on an expression whose every comparison declines
// (BenchmarkReadPathEvaluateChain, a chain of computed attributes) -- a fast path
// that made the shapes it does not handle slower. Resolving once and handing back
// whichever form came out removes that entirely: a decline now does the same work the
// ordinary path did, not that work twice.
//
// It therefore EVALUATES on the decline path, which means it can panic the cyclic
// sentinel exactly where Evaluate would have, and must be called in the operand order
// the ordinary path uses -- left, then right -- so a cyclic left operand still
// unwinds before the right is touched.
//
// Both results are out-params rather than return values, which is not the house style
// and is here for a measured reason. Returning (litOperand, bool) and writing the
// Value through a pointer moves a litOperand back up the call chain on EVERY operand,
// including the ones that decline and will never look at it -- more bytes per operand
// than the single Value the ordinary path moves. Writing both in place costs a
// one-byte return and leaves the decline path moving exactly what it moved before.
// Escape analysis keeps both on the caller's stack (`val does not escape`), so this
// buys nothing on the heap and everything on the copy.
func (e *Evaluator) operandLiteral(expr ast.Expr, lit *litOperand, val *Value) bool {
	if ref, ok := expr.(*ast.AttributeReference); ok {
		return e.refOperand(ref, lit, val)
	}
	if l, ok := literalOperand(expr); ok {
		*lit = l
		return true
	}
	*val = e.Evaluate(expr)
	return false
}

// refOperand resolves an attribute reference to the literal bound to it, or, failing
// that, to its value.
//
// The scope resolution below mirrors resolveAttributeReference arm for arm, because
// any divergence is a wrong matchmaking answer rather than a slow one: MY never falls
// through to an enclosing scope, TARGET goes through targetOf so an evaluation-scoped
// MY/TARGET binding is honoured without either ad being written, an absolute (".attr")
// reference names the root ad and a miss there is undefined rather than a walk back
// down, and an UNQUALIFIED reference searches this ad and its enclosing scope chain
// and only then the target ad's chain -- the old-ClassAd matchmaking fallthrough that
// lets a startd's START expression name job attributes unqualified.
//
// A reference that resolves to nothing is undefined, which is a literal operand and
// not a decline: an absent attribute is a large share of real constraints
// (`TARGET.HasDocker` on a machine that has none) and answering it here is the point.
//
// A reference bound to a NON-literal expression yields its value instead, which is
// also where cyclic-reference detection is preserved: that evaluation runs through
// evalAttrExpr with its in-progress bookkeeping intact, while every reference answered
// as a literal is provably outside any cycle, since a cycle must run through a
// non-literal expression.
func (e *Evaluator) refOperand(ref *ast.AttributeReference, lit *litOperand, val *Value) bool {
	norm := ref.NormalizedName()
	if e.resolver != nil || norm == currentTimeAttr {
		// A custom resolver replaces scope resolution entirely and speaks only in
		// Values (see SetResolver, whose CurrentTime contract in particular has no
		// expression-level form). CurrentTime has the separate problem that whether
		// the scope DEFINES it changes the answer and not merely the value, which
		// evaluateCurrentTime owns. Neither has had any work done here yet, so both
		// hand the whole reference back to the ordinary path.
		*val = e.Evaluate(ref)
		return false
	}
	switch ref.Scope {
	case ast.MyScope:
		return e.boundOperand(e.classad, norm, lit, val)
	case ast.TargetScope:
		if e.classad == nil {
			*lit = undefLit
			return true
		}
		return e.boundOperand(e.targetOf(e.classad), norm, lit, val)
	case ast.ParentScope:
		if e.classad == nil {
			*lit = undefLit
			return true
		}
		return e.boundOperand(e.classad.parent, norm, lit, val)
	case ast.RootScope:
		return e.boundOperand(rootOf(e.classad), norm, lit, val)
	default:
		for ad := e.classad; ad != nil; ad = ad.parent {
			if bound := ad.lookupNorm(norm); bound != nil {
				return e.operandOfBound(ad, norm, bound, lit, val)
			}
		}
		if e.classad != nil {
			for ad := e.targetOf(e.classad); ad != nil; ad = ad.parent {
				if bound := ad.lookupNorm(norm); bound != nil {
					return e.operandOfBound(ad, norm, bound, lit, val)
				}
			}
		}
		*lit = undefLit
		return true
	}
}

// boundOperand is evalAttrIn's resolution: a nil ad and a missing attribute are both
// undefined, and a bound expression is decoded or evaluated.
func (e *Evaluator) boundOperand(ad *ClassAd, norm string, lit *litOperand, val *Value) bool {
	if ad == nil {
		*lit = undefLit
		return true
	}
	bound := ad.lookupNorm(norm)
	if bound == nil {
		*lit = undefLit
		return true
	}
	return e.operandOfBound(ad, norm, bound, lit, val)
}

// operandOfBound turns the expression bound to an attribute into an operand: decoded
// in place if it is a literal, evaluated otherwise.
func (e *Evaluator) operandOfBound(ad *ClassAd, norm string, bound ast.Expr, lit *litOperand, val *Value) bool {
	if l, ok := literalOperand(bound); ok {
		*lit = l
		return true
	}
	*val = e.evalBoundRef(ad, norm, bound)
	return false
}

// evalBoundRef evaluates the expression bound to an attribute, charging the recursion
// depth Evaluate would have charged for the reference node that named it.
//
// This repeats Evaluate's depth bookkeeping rather than calling Evaluate, because
// Evaluate would start again from the reference node and redo the lookup that found
// `bound` -- the whole cost this path exists to avoid. The bookkeeping is Evaluate's
// exactly, including leaving depth inflated when the cyclic sentinel unwinds, which
// Evaluate documents as harmless: every terminal entry point recovers into a fresh or
// SetScope-reset evaluator.
func (e *Evaluator) evalBoundRef(ad *ClassAd, norm string, bound ast.Expr) Value {
	if e.depth >= maxEvalDepth {
		panic(cyclicEvalError{})
	}
	e.depth++
	v := e.evalAttrExpr(ad, norm, bound)
	e.depth--
	return v
}

// evaluateComparison decides a comparison operator, taking the unboxed path when both
// operands turn out to be literal-backed and the ordinary Value path otherwise -- with
// each operand resolved exactly once either way.
//
// The caller has established that op is a comparison and that the evaluator is not
// within predicateDepthMargin of the recursion limit.
func (e *Evaluator) evaluateComparison(c cmpKind, op *ast.BinaryOp) Value {
	var l, r litOperand
	var leftVal, rightVal Value
	lok := e.operandLiteral(op.Left, &l, &leftVal)
	rok := e.operandLiteral(op.Right, &r, &rightVal)
	if lok && rok {
		countFastCompare()
		return mapState(compareLiterals(c, l, r))
	}
	// One side only was literal-backed; box it and decide at the Value level, which
	// is what the ordinary path would have done with both.
	if lok {
		leftVal = l.toValue()
	}
	if rok {
		rightVal = r.toValue()
	}
	return e.applyBinaryValues(op.Op, leftVal, rightVal)
}
