package classad

import (
	"fmt"
	"math"
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/parser"
)

// ValueType represents the type of a ClassAd value.
type ValueType int

const (
	// UndefinedValue represents an undefined value
	UndefinedValue ValueType = iota
	// ErrorValue represents an error value
	ErrorValue
	// BooleanValue represents a boolean value
	BooleanValue
	// IntegerValue represents an integer value
	IntegerValue
	// RealValue represents a real (float) value
	RealValue
	// StringValue represents a string value
	StringValue
	// ListValue represents a list value
	ListValue
	// ClassAdValue represents a nested ClassAd value
	ClassAdValue
)

// Value represents the result of evaluating a ClassAd expression.
//
// A Value is copied by value constantly -- on every bytecode stack push/pop and
// every operator call -- so it is kept small: the list payload (which is four
// words and only used by list values) lives out-of-line behind the list pointer,
// leaving a Value that is a scalar/string/classad in one compact struct.
type Value struct {
	valueType ValueType
	// intVal holds an integer value, a boolean (0 or 1), and -- for a RealValue --
	// the IEEE-754 bits of the float64 (see real()/NewRealValue). Folding all three
	// scalar payloads here keeps Value one word smaller than a separate bool/real
	// field would, and the bits round-trip exactly (NaN/Inf/-0 included).
	intVal     int64
	strVal     string
	classAdVal *ClassAd
	// list holds a list value's payload out-of-line; it is non-nil exactly when
	// valueType == ListValue. See listData.
	list *listData
}

// listData is a list value's payload, stored out-of-line to keep Value small. It
// is immutable once constructed (materialization allocates a fresh []Value rather
// than mutating), so copies of a list Value may safely share one *listData.
//
// exprs / scope make a list value from a literal lazy: they hold the source
// element expressions and the scope to evaluate them in, rather than pre-evaluated
// values, mirroring the reference engine (a list value is its unevaluated
// ExprList). Elements are evaluated on demand by ListValue(). This lets
// string()/strcat()/etc. unparse the source expressions exactly (string({1, 1+1})
// is "{ 1,1 + 1 }"), and a self-referential list (A0 = {{A0}}) is a list value
// rather than a construction-time cycle error. Lists built programmatically by
// functions leave exprs nil and use the eager vals instead.
//
// depth records the evaluator recursion depth at which a lazy list value was
// created. Materializing the list's elements (ListValue/String/subscript) resumes
// depth accounting from here, so a value that is cyclic only through list
// materialization (A = {A}) -- where each materialization step would otherwise
// start a fresh evaluator at depth 0 -- still hits maxEvalDepth and resolves to
// error instead of overflowing the goroutine stack.
type listData struct {
	vals  []Value
	exprs []ast.Expr
	scope *ClassAd
	depth int
}

// NewUndefinedValue creates an undefined value.
func NewUndefinedValue() Value {
	return Value{valueType: UndefinedValue}
}

// NewErrorValue creates an error value.
func NewErrorValue() Value {
	return Value{valueType: ErrorValue}
}

// NewBoolValue creates a boolean value.
func NewBoolValue(b bool) Value {
	var i int64
	if b {
		i = 1
	}
	return Value{valueType: BooleanValue, intVal: i}
}

// NewIntValue creates an integer value.
func NewIntValue(i int64) Value {
	return Value{valueType: IntegerValue, intVal: i}
}

// NewRealValue creates a real value.
func NewRealValue(r float64) Value {
	return Value{valueType: RealValue, intVal: int64(math.Float64bits(r))}
}

// real reinterprets intVal as the float64 it encodes. Valid only when
// valueType == RealValue. Inlined to a bit reinterpret, so it costs nothing over
// a plain field read.
func (v Value) real() float64 {
	return math.Float64frombits(uint64(v.intVal))
}

// NewStringValue creates a string value.
func NewStringValue(s string) Value {
	return Value{valueType: StringValue, strVal: s}
}

// NewListValue creates a list value.
func NewListValue(list []Value) Value {
	return Value{valueType: ListValue, list: &listData{vals: list}}
}

// NewClassAdValue creates a ClassAd value.
func NewClassAdValue(ad *ClassAd) Value {
	return Value{valueType: ClassAdValue, classAdVal: ad}
}

// Type returns the type of the value.
func (v Value) Type() ValueType {
	return v.valueType
}

// IsUndefined returns true if the value is undefined.
func (v Value) IsUndefined() bool {
	return v.valueType == UndefinedValue
}

// IsError returns true if the value is an error.
func (v Value) IsError() bool {
	return v.valueType == ErrorValue
}

// IsBool returns true if the value is a boolean.
func (v Value) IsBool() bool {
	return v.valueType == BooleanValue
}

// IsInteger returns true if the value is an integer.
func (v Value) IsInteger() bool {
	return v.valueType == IntegerValue
}

// IsReal returns true if the value is a real number.
func (v Value) IsReal() bool {
	return v.valueType == RealValue
}

// IsNumber returns true if the value is an integer or real.
func (v Value) IsNumber() bool {
	return v.valueType == IntegerValue || v.valueType == RealValue
}

// IsString returns true if the value is a string.
func (v Value) IsString() bool {
	return v.valueType == StringValue
}

// IsList returns true if the value is a list.
func (v Value) IsList() bool {
	return v.valueType == ListValue
}

// IsClassAd returns true if the value is a ClassAd.
func (v Value) IsClassAd() bool {
	return v.valueType == ClassAdValue
}

// BoolValue returns the boolean value. Returns error if not a boolean.
func (v Value) BoolValue() (bool, error) {
	if v.valueType != BooleanValue {
		return false, fmt.Errorf("value is not a boolean")
	}
	return v.intVal != 0, nil
}

// IntValue returns the integer value. Returns error if not an integer.
func (v Value) IntValue() (int64, error) {
	if v.valueType != IntegerValue {
		return 0, fmt.Errorf("value is not an integer")
	}
	return v.intVal, nil
}

// RealValue returns the real value. Returns error if not a real.
func (v Value) RealValue() (float64, error) {
	if v.valueType != RealValue {
		return 0, fmt.Errorf("value is not a real")
	}
	return v.real(), nil
}

// NumberValue returns the numeric value as a float64, converting integers if needed.
func (v Value) NumberValue() (float64, error) {
	switch v.valueType {
	case IntegerValue:
		return float64(v.intVal), nil
	case RealValue:
		return v.real(), nil
	default:
		return 0, fmt.Errorf("value is not a number")
	}
}

// StringValue returns the string value. Returns error if not a string.
func (v Value) StringValue() (string, error) {
	if v.valueType != StringValue {
		return "", fmt.Errorf("value is not a string")
	}
	return v.strVal, nil
}

// ListValue returns the list value. Returns error if not a list.
func (v Value) ListValue() ([]Value, error) {
	if v.valueType != ListValue {
		return nil, fmt.Errorf("value is not a list")
	}
	if v.list.exprs != nil {
		// Lazy list: evaluate each element expression in its stored scope now.
		vals := make([]Value, len(v.list.exprs))
		ev := v.lazyEvaluator(nil)
		for i, e := range v.list.exprs {
			vals[i] = evalRecoveringCyclic(ev, e)
		}
		return vals, nil
	}
	return v.list.vals, nil
}

// evalRecoveringCyclic evaluates a lazy list element, turning a cyclic-reference
// panic into an error value rather than letting it escape. A lazy list can be
// materialized outside the recover-protected entry points (canonical encoding,
// String), so the sentinel must not propagate there; a cyclic element becomes
// that element's error value (the list survives), matching the reference engine.
func evalRecoveringCyclic(ev *Evaluator, e ast.Expr) (result Value) {
	// Save/restore depth around the recover: Evaluate skips its depth decrement
	// when a cyclic panic unwinds (it has no per-node defer), and this is the one
	// site that recovers and then keeps evaluating with the SAME evaluator (the
	// next lazy-list element), so depth must be rewound or later elements would
	// start artificially deep and spuriously trip maxEvalDepth.
	//
	// recoverCyclic must remain a DIRECT deferred call -- recover() only stops the
	// panic when the deferred function itself calls it -- so the depth rewind is a
	// separate defer (registered after, it runs first, before recoverCyclic).
	saved := ev.depth
	defer recoverCyclic(&result)
	defer func() { ev.depth = saved }()
	return ev.Evaluate(e)
}

// listLen returns the number of elements in a list value without evaluating
// them (for a lazy list). The caller must ensure v is a list.
func (v Value) listLen() int {
	if v.list.exprs != nil {
		return len(v.list.exprs)
	}
	return len(v.list.vals)
}

// listElementAt evaluates and returns the i-th element of a list value,
// evaluating only that element for a lazy list (so e.g. {selfRef, x}[1]
// evaluates x without touching the self-referential element 0). The caller must
// ensure v is a list and 0 <= i < listLen().
func (v Value) listElementAt(i int, parent *Evaluator) Value {
	if v.list.exprs != nil {
		return evalRecoveringCyclic(v.lazyEvaluator(parent), v.list.exprs[i])
	}
	return v.list.vals[i]
}

// lazyEvaluator builds the evaluator for materializing a lazy list's elements in
// its stored scope. Its depth is the greater of the caller's current depth
// (parent, which may be nil at a top-level entry such as ListValue/String) and
// the depth at which the list value was created, so recursion accounting never
// resets going into materialization and a materialization-only cycle terminates.
func (v Value) lazyEvaluator(parent *Evaluator) *Evaluator {
	depth := v.list.depth
	if parent != nil && parent.depth > depth {
		depth = parent.depth
	}
	return &Evaluator{classad: v.list.scope, depth: depth}
}

// ClassAdValue returns the ClassAd value. Returns error if not a ClassAd.
func (v Value) ClassAdValue() (*ClassAd, error) {
	if v.valueType != ClassAdValue {
		return nil, fmt.Errorf("value is not a ClassAd")
	}
	return v.classAdVal, nil
}

// String returns a string representation of the value.
func (v Value) String() string {
	switch v.valueType {
	case UndefinedValue:
		return "undefined"
	case ErrorValue:
		return "error"
	case BooleanValue:
		if v.intVal != 0 {
			return "true"
		}
		return "false"
	case IntegerValue:
		return fmt.Sprintf("%d", v.intVal)
	case RealValue:
		return fmt.Sprintf("%g", v.real())
	case StringValue:
		return fmt.Sprintf("%q", v.strVal)
	case ListValue:
		elems, _ := v.ListValue()
		return fmt.Sprintf("%v", elems)
	case ClassAdValue:
		if v.classAdVal != nil {
			return v.classAdVal.String()
		}
		return "[]"
	default:
		return "unknown"
	}
}

// cyclicEvalError is panicked when a cyclic attribute reference is detected
// during evaluation; recoverCyclic turns it into an error value at the
// top-level evaluation entry points.
type cyclicEvalError struct{}

// recoverCyclic recovers a cyclicEvalError panic and stores an error value in
// result; any other panic is re-raised. Use as `defer recoverCyclic(&result)`
// with a named return value.
func recoverCyclic(result *Value) {
	if r := recover(); r != nil {
		if _, ok := r.(cyclicEvalError); ok {
			*result = NewErrorValue()
			return
		}
		panic(r)
	}
}

// Evaluator handles evaluation of ClassAd expressions.
type Evaluator struct {
	classad *ClassAd
	// depth is the current evaluation recursion depth. It is inherited by
	// child evaluators created during evaluation (list-element and nested-ad
	// scopes) so a cyclic value that escapes the per-attribute cycle guard --
	// e.g. a lazy list element referencing its own attribute, A = {A[0]} --
	// fails at maxEvalDepth instead of overflowing the goroutine stack.
	depth int
	// resolver, when non-nil, supplies attribute values instead of the ClassAd
	// scope (see SetResolver). It lets a caller evaluate against an alternate
	// backing (e.g. an encoded ad) without materializing a ClassAd.
	resolver func(name string, scope ast.AttributeScope) Value
}

// maxEvalDepth bounds evaluation recursion. It is far below what overflows the
// goroutine stack, and deeper than any legitimate expression reaches.
const maxEvalDepth = 2000

// NewEvaluator creates a new evaluator for the given ClassAd.
func NewEvaluator(ad *ClassAd) *Evaluator {
	return &Evaluator{classad: ad}
}

// child creates a sub-evaluator for ad that continues this evaluator's
// recursion-depth accounting.
func (e *Evaluator) child(ad *ClassAd) *Evaluator {
	return &Evaluator{classad: ad, depth: e.depth}
}

// Evaluate evaluates an expression in the context of the ClassAd.
//
// Depth is tracked to bound recursion (a cyclic reference or a pathologically
// deep expression panics with cyclicEvalError once maxEvalDepth is reached).
// The increment/decrement is done inline rather than with `defer e.depth--`:
// this function recurses once per AST node, and because a recover() sits in an
// ancestor frame (the recoverCyclic at every evaluation entry point) the Go
// compiler cannot open-code those defers, so a per-node defer put the whole
// tree-walk on the slow stack-defer path -- ~25% of matchmaking CPU. On the
// cyclic-panic path the decrement is skipped (depth is left inflated), which is
// harmless: every terminal entry point recovers into a fresh or SetScope-reset
// evaluator, and the one recover-and-continue caller (evalRecoveringCyclic, for
// lazy-list elements) saves and restores depth around each element.
func (e *Evaluator) Evaluate(expr ast.Expr) Value {
	if expr == nil {
		return NewUndefinedValue()
	}
	if e.depth >= maxEvalDepth {
		// Too deep to be anything but a cycle; treat it as one.
		panic(cyclicEvalError{})
	}
	e.depth++
	v := e.evalNode(expr)
	e.depth--
	return v
}

// evalNode dispatches one expression node. Callers go through Evaluate, which
// manages the recursion-depth guard.
func (e *Evaluator) evalNode(expr ast.Expr) Value {
	switch v := expr.(type) {
	case *ast.ParenExpr:
		// Parentheses are transparent to evaluation (they only affect parsing
		// precedence and unparsing).
		return e.Evaluate(v.Inner)

	case *ast.IntegerLiteral:
		return NewIntValue(v.Value)

	case *ast.RealLiteral:
		return NewRealValue(v.Value)

	case *ast.StringLiteral:
		return NewStringValue(v.Value)

	case *ast.BooleanLiteral:
		return NewBoolValue(v.Value)

	case *ast.UndefinedLiteral:
		return NewUndefinedValue()

	case *ast.ErrorLiteral:
		return NewErrorValue()

	case *ast.AttributeReference:
		return e.evaluateAttributeReference(v)

	case *ast.BinaryOp:
		return e.evaluateBinaryOp(v)

	case *ast.UnaryOp:
		return e.evaluateUnaryOp(v)

	case *ast.ConditionalExpr:
		return e.evaluateConditional(v)

	case *ast.ElvisExpr:
		return e.evaluateElvis(v)

	case *ast.ListLiteral:
		return e.evaluateList(v)

	case *ast.RecordLiteral:
		return NewClassAdValue(&ClassAd{ad: v.ClassAd})

	case *ast.FunctionCall:
		return e.evaluateFunctionCall(v)

	case *ast.SelectExpr:
		return e.evaluateSelectExpr(v)

	case *ast.SubscriptExpr:
		return e.evaluateSubscriptExpr(v)

	default:
		return NewErrorValue()
	}
}

func (e *Evaluator) evaluateAttributeReference(ref *ast.AttributeReference) Value {
	if e.resolver != nil {
		// A custom resolver replaces ClassAd-scope resolution entirely (used for
		// evaluating a native program against an encoded ad). It receives the
		// original-cased name and the reference's scope.
		return e.resolver(ref.Name, ref.Scope)
	}
	// Resolve the reference name case-insensitively. The folded name is
	// precomputed on the AST node at parse time (NewAttributeReference), so this
	// hot path -- run once per candidate per reference during matchmaking -- does
	// not allocate a fresh strings.ToLower result on every lookup.
	norm := ref.NormalizedName()
	switch ref.Scope {
	case ast.MyScope:
		// MY.attr - always the current ClassAd (no scope-chain fallthrough).
		return e.evalAttrIn(e.classad, norm)
	case ast.TargetScope:
		if e.classad == nil {
			return NewUndefinedValue()
		}
		return e.evalAttrIn(e.classad.target, norm)
	case ast.ParentScope:
		if e.classad == nil {
			return NewUndefinedValue()
		}
		return e.evalAttrIn(e.classad.parent, norm)
	default:
		// Unscoped: search the current ad, then up the enclosing (parent) scope
		// chain, matching the reference engine. The chain is established by the
		// select operator (see evaluateSelectExpr); a top-level ad has no parent
		// so this is just a lookup in the current ad.
		for ad := e.classad; ad != nil; ad = ad.parent {
			if expr := ad.lookupNorm(norm); expr != nil {
				return e.evalAttrExpr(ad, norm, expr)
			}
		}
		// Old-ClassAd matchmaking fallthrough: an unqualified reference not found
		// in MY (this ad and its parent scope chain) resolves against the TARGET
		// ad when one is set. This is what HTCondor relies on during matchmaking --
		// a startd START expression names job attributes and a job's Requirements
		// names machine attributes, both unqualified. Mirrors the C++ classad
		// alternateScope mechanism (src/classad/attrrefs.cpp FindExpr):
		//
		//	rc = current->LookupInScope( attributeStr, tree, state );
		//	if ( !expr && !absolute && rc == EVAL_UNDEF && current->alternateScope )
		//		rc = current->alternateScope->LookupInScope( attributeStr, tree, state );
		//
		// where alternateScope is set to the peer ad by MatchClassAd under old
		// semantics. Only bare references reach here (MY./TARGET./PARENT./absolute
		// refs are handled in their own cases above), matching the C++ !expr &&
		// !absolute guard. The attribute found in the target evaluates in the
		// target's own context (evalAttrExpr rebinds the scope), so ITS unqualified
		// refs resolve target-ad-first and then fall back through the target's own
		// target -- the same ping-pong the C++ code allows, bounded by the shared
		// per-(ad,attr) cyclic-reference guard in evalAttrExpr.
		if e.classad != nil {
			for ad := e.classad.target; ad != nil; ad = ad.parent {
				if expr := ad.lookupNorm(norm); expr != nil {
					return e.evalAttrExpr(ad, norm, expr)
				}
			}
		}
		return NewUndefinedValue()
	}
}

// evalAttrIn looks up an already-normalized name in ad and evaluates its value.
func (e *Evaluator) evalAttrIn(ad *ClassAd, norm string) Value {
	if ad == nil {
		return NewUndefinedValue()
	}
	expr := ad.lookupNorm(norm)
	if expr == nil {
		return NewUndefinedValue()
	}
	return e.evalAttrExpr(ad, norm, expr)
}

// evalAttrExpr evaluates expr -- the value bound to the already-normalized name
// norm in ad -- with cyclic-reference detection. A cycle (the attribute is
// already being evaluated in ad) panics a cyclicEvalError sentinel, recovered as
// error at the top-level entry points (distinct from an error value, which
// =?= / =!= would compare as a type).
//
// The expression is evaluated in ad's context by temporarily rebinding the
// evaluator's ClassAd rather than allocating a child evaluator per reference.
// The deferred cleanup restores the ClassAd and clears the cyclic marker even
// when a cyclic panic unwinds; recursion depth is restored by Evaluate's own
// deferred decrement (which runs during the same unwind).
func (e *Evaluator) evalAttrExpr(ad *ClassAd, norm string, expr ast.Expr) Value {
	// Fast path: a literal attribute value references nothing, so it cannot form a
	// cyclic reference and its value does not depend on the scope. Skip the
	// per-reference cyclic-detection bookkeeping (a map insert + deferred delete)
	// and the scope rebinding entirely. Real ads are overwhelmingly literal-valued
	// (Cpus = 8, Arch = "X86_64", ...), so this is the common case. A literal
	// wrapped in parentheses falls to the slow path, which is correct and rare.
	if isLiteralExpr(expr) {
		return e.Evaluate(expr)
	}
	if ad.evaluating[norm] {
		panic(cyclicEvalError{})
	}
	if ad.evaluating == nil {
		ad.evaluating = make(map[string]bool)
	}
	ad.evaluating[norm] = true
	saved := e.classad
	e.classad = ad
	defer func() {
		e.classad = saved
		delete(ad.evaluating, norm)
	}()

	return e.Evaluate(expr)
}

// isLiteralExpr reports whether e is a literal -- a value that references no
// attributes and so can neither depend on the scope nor participate in a cyclic
// reference. Used by evalAttrExpr to skip cyclic-detection bookkeeping. A literal
// wrapped in a ParenExpr is deliberately not treated as a literal here, to keep
// the check O(1); the slow path handles it correctly.
func isLiteralExpr(e ast.Expr) bool {
	switch e.(type) {
	case *ast.IntegerLiteral, *ast.RealLiteral, *ast.StringLiteral,
		*ast.BooleanLiteral, *ast.UndefinedLiteral, *ast.ErrorLiteral:
		return true
	}
	return false
}

func (e *Evaluator) evaluateBinaryOp(op *ast.BinaryOp) Value {
	// && and || implement short-circuit three-valued logic: the right operand
	// is evaluated only when the left does not already determine the result, so
	// "false && q" is false even when q would cycle or error (and likewise
	// "true || q"). This must run before the generic error/undefined
	// propagation below ("false && error" is false, "true || undefined" true).
	if op.Op == "&&" || op.Op == "||" {
		return e.evaluateShortCircuit(op)
	}

	left := e.Evaluate(op.Left)
	right := e.Evaluate(op.Right)
	return e.applyBinaryValues(op.Op, left, right)
}

// applyBinaryValues applies a binary operator to two already-evaluated operand
// values. It is the value-level core shared by the AST evaluator (above) and the
// exported ApplyBinaryOp hook used by the bytecode interpreter, so both compute
// binary operations through identical logic.
//
// && and || are included for the interpreter's combine path (after it has decided
// the right operand must be evaluated); the AST path never reaches here for them
// because evaluateBinaryOp short-circuits first.
func (e *Evaluator) applyBinaryValues(op string, left, right Value) Value {
	// Logical and meta-equality operators do not propagate errors generically.
	switch op {
	case "&&":
		return e.evaluateAnd(left, right)
	case "||":
		return e.evaluateOr(left, right)
	case "is":
		return e.evaluateIs(left, right)
	case "isnt":
		return e.evaluateIsnt(left, right)
	}

	// Handle error propagation for the remaining operators.
	if left.IsError() || right.IsError() {
		return NewErrorValue()
	}

	switch op {
	// Arithmetic operators
	case "+":
		return e.evaluateAdd(left, right)
	case "-":
		return e.evaluateSubtract(left, right)
	case "*":
		return e.evaluateMultiply(left, right)
	case "/":
		return e.evaluateDivide(left, right)
	case "%":
		return e.evaluateModulo(left, right)

	// Comparison operators
	case "<":
		return e.evaluateLessThan(left, right)
	case ">":
		return e.evaluateGreaterThan(left, right)
	case "<=":
		return e.evaluateLessOrEqual(left, right)
	case ">=":
		return e.evaluateGreaterOrEqual(left, right)
	case "==":
		return e.evaluateEqual(left, right)
	case "!=":
		return e.evaluateNotEqual(left, right)

	// Bitwise operators (integer-only).
	case "&", "|", "^", "<<", ">>", ">>>":
		return e.evaluateBitwise(op, left, right)

	default:
		return NewErrorValue()
	}
}

func (e *Evaluator) evaluateUnaryOp(op *ast.UnaryOp) Value {
	return e.applyUnaryValue(op.Op, e.Evaluate(op.Expr))
}

// applyUnaryValue applies a unary operator to an already-evaluated operand value.
// Shared by the AST evaluator and the exported ApplyUnaryOp hook.
func (e *Evaluator) applyUnaryValue(op string, val Value) Value {
	if val.IsError() {
		return val
	}

	// Unary operators propagate undefined (e.g. -undefined, +undefined,
	// !undefined are all undefined in the reference engine).
	if val.IsUndefined() {
		return NewUndefinedValue()
	}

	switch op {
	case "-":
		if val.IsInteger() {
			intVal, _ := val.IntValue()
			return NewIntValue(-intVal)
		}
		if val.IsReal() {
			realVal, _ := val.RealValue()
			return NewRealValue(-realVal)
		}
		return NewErrorValue()

	case "+":
		if val.IsNumber() {
			return val
		}
		return NewErrorValue()

	case "!":
		// Logical not uses the same three-valued, number-coercing view as
		// && / ||: !undefined is undefined, !0 is true, !5 is false.
		switch logicalView(val) {
		case lsTrue:
			return NewBoolValue(false)
		case lsFalse:
			return NewBoolValue(true)
		case lsUndef:
			return NewUndefinedValue()
		default:
			return NewErrorValue()
		}

	case "~":
		// Bitwise not requires a genuine integer (undefined already handled
		// above); a real or boolean operand is an error.
		if val.IsInteger() {
			i, _ := val.IntValue()
			return NewIntValue(^i)
		}
		return NewErrorValue()

	default:
		return NewErrorValue()
	}
}

func (e *Evaluator) evaluateConditional(cond *ast.ConditionalExpr) Value {
	condVal := e.Evaluate(cond.Condition)

	// The condition coerces a number to its truthiness like && / || do, so
	// "1 ? a : b" selects a and "0 ? a : b" selects b. Undefined yields
	// undefined; a non-coercible condition (string/list/error) is an error.
	switch logicalView(condVal) {
	case lsTrue:
		return e.Evaluate(cond.TrueExpr)
	case lsFalse:
		return e.Evaluate(cond.FalseExpr)
	case lsUndef:
		// An undefined condition yields undefined, but -- unlike a true/false
		// condition, which evaluates only the taken branch -- the reference
		// engine still evaluates BOTH branches. An error *value* in a branch is
		// absorbed (the result stays undefined), but a cyclic self-reference is
		// a hard failure that propagates: undefined ? 1 : error is undefined,
		// while undefined ? {} : A0 (with A0 the attribute itself) is error.
		e.Evaluate(cond.TrueExpr)
		e.Evaluate(cond.FalseExpr)
		return NewUndefinedValue()
	default:
		return NewErrorValue()
	}
}

// evaluateElvis evaluates the Elvis operator (expr1 ?: expr2).
// If expr1 evaluates to undefined, returns expr2; otherwise returns expr1.
func (e *Evaluator) evaluateElvis(elvis *ast.ElvisExpr) Value {
	leftVal := e.Evaluate(elvis.Left)

	// If left side is undefined, evaluate and return the right side
	if leftVal.IsUndefined() {
		return e.Evaluate(elvis.Right)
	}

	// Otherwise, return the left side value (including error values)
	return leftVal
}

func (e *Evaluator) evaluateList(list *ast.ListLiteral) Value {
	// Lazy: do not evaluate the elements now. Carry the source expressions
	// (non-nil even when empty, to distinguish a literal from a
	// programmatically-built list) and the scope to evaluate them in. Elements
	// are evaluated on demand by ListValue(). This matches the reference engine
	// (a list value is its unevaluated ExprList): string coercion can unparse
	// the source expressions, and a self-referential list is a value rather
	// than a construction-time cycle error.
	exprs := list.Elements
	if exprs == nil {
		exprs = []ast.Expr{}
	}
	return Value{valueType: ListValue, list: &listData{exprs: exprs, scope: e.classad, depth: e.depth}}
}

func (e *Evaluator) evaluateSelectExpr(sel *ast.SelectExpr) Value {
	// Evaluate the record expression
	recordVal := e.Evaluate(sel.Record)

	// Handle undefined and error
	if recordVal.IsUndefined() {
		return NewUndefinedValue()
	}
	if recordVal.IsError() {
		return NewErrorValue()
	}

	// List projection: selecting an attribute from a list maps the select over
	// each element, matching the reference engine. So {[A=1],[A=2]}.A is
	// {1,2}, {[A=1],[B=2]}.A is {1,undefined} (the missing attribute chains to
	// the enclosing scope), {1,2,3}.A is {error,error,error} (a non-ad element
	// selects to error), and {}.A is the empty list.
	if recordVal.IsList() {
		elems := recordVal.listElementsPropagating(e)
		results := make([]Value, len(elems))
		for i, el := range elems {
			results[i] = e.selectAttr(el, sel.Attr)
		}
		return NewListValue(results)
	}

	return e.selectAttr(recordVal, sel.Attr)
}

// listElementsPropagating evaluates a list value's elements WITHOUT recovering
// a cyclic-reference panic (unlike ListValue, which localizes a cycle to an
// error element). Used by list projection so that a cyclic element aborts the
// whole select to error -- matching the reference engine, where (A.A) with
// A = {0 % A0} and A0 = A.A is error, not list[error]. The panic propagates to
// the nearest recover (the enclosing attribute evaluation, or the outermost
// ListValue if projection happens during a later materialization).
func (v Value) listElementsPropagating(parent *Evaluator) []Value {
	if v.list.exprs != nil {
		vals := make([]Value, len(v.list.exprs))
		ev := v.lazyEvaluator(parent)
		for i, e := range v.list.exprs {
			vals[i] = ev.Evaluate(e)
		}
		return vals
	}
	return v.list.vals
}

// selectAttr resolves record.attr for an already-evaluated record value (a
// single element of a `.` select). A non-ad record is an error.
func (e *Evaluator) selectAttr(recordVal Value, attr string) Value {
	// undefined/error elements propagate (so {undefined,[A=1]}.A is
	// {undefined,1}); any other non-ad element selects to error.
	if recordVal.IsUndefined() {
		return NewUndefinedValue()
	}
	if recordVal.IsError() {
		return NewErrorValue()
	}
	if !recordVal.IsClassAd() {
		return NewErrorValue()
	}
	ad, _ := recordVal.ClassAdValue()
	if ad == nil {
		return NewErrorValue()
	}

	// Connect the nested ad's scope to the selecting scope, so that an
	// attribute missing from the nested ad -- or an unscoped reference inside
	// the selected attribute's value -- resolves up the enclosing scope chain,
	// matching the reference engine ([x=1].A resolves A in the enclosing ad,
	// and [A=[].A] is a cycle). The nested ad value is freshly built per
	// evaluation, so setting its parent here is safe. Resolve the attribute as
	// an unscoped reference so it chains (and participates in cycle detection).
	ad.parent = e.classad
	nested := e.child(ad)
	return nested.evaluateAttributeReference(ast.NewAttributeReference(attr, ast.NoScope))
}

func (e *Evaluator) evaluateSubscriptExpr(sub *ast.SubscriptExpr) Value {
	// Evaluate the container
	containerVal := e.Evaluate(sub.Container)

	// Handle undefined and error
	if containerVal.IsUndefined() {
		return NewUndefinedValue()
	}
	if containerVal.IsError() {
		return NewErrorValue()
	}

	// Evaluate the index
	indexVal := e.Evaluate(sub.Index)

	// Handle undefined and error in index
	if indexVal.IsUndefined() {
		return NewUndefinedValue()
	}
	if indexVal.IsError() {
		return NewErrorValue()
	}

	// Handle list subscripting with integer index
	if containerVal.IsList() {
		if !indexVal.IsInteger() {
			return NewErrorValue()
		}

		index, _ := indexVal.IntValue()

		// An out-of-range (including negative) list index is an error in the
		// reference engine, not undefined.
		if index < 0 || index >= int64(containerVal.listLen()) {
			return NewErrorValue()
		}

		// Evaluate only the indexed element (a lazy list does not evaluate its
		// other elements), so {selfRef, x}[1] yields x without cycling on the
		// self-referential element.
		return containerVal.listElementAt(int(index), e)
	}

	// Handle ClassAd subscripting with string key
	if containerVal.IsClassAd() {
		if !indexVal.IsString() {
			return NewErrorValue()
		}

		ad, _ := containerVal.ClassAdValue()
		key, _ := indexVal.StringValue()
		return ad.EvaluateAttr(key)
	}

	return NewErrorValue()
}

// Arithmetic operations
// numericOperand reports whether v can act as a number in arithmetic and
// relational contexts. Booleans participate as integers (true=1, false=0),
// matching the C++ reference engine where BOOLEAN_VALUE is part of the numeric
// value mask. isInt is true for integers and booleans (so int/bool operands
// keep an integer result type); iv/rv carry the integer and real views.
//
// Note this is operator-level coercion only: the value's own type is unchanged,
// so isInteger()/isReal() still report false for a boolean.
func numericOperand(v Value) (isNum, isInt bool, iv int64, rv float64) {
	switch v.valueType {
	case IntegerValue:
		return true, true, v.intVal, float64(v.intVal)
	case RealValue:
		return true, false, 0, v.real()
	case BooleanValue:
		if v.intVal != 0 {
			return true, true, 1, 1
		}
		return true, true, 0, 0
	default:
		return false, false, 0, 0
	}
}

// realArithResult mirrors the reference engine's doRealArithmetic result check:
// a result of +Inf (HUGE_VAL) is an error, while -Inf and NaN are returned as
// ordinary real values. (1.0/0.0 is error, but -1.0/0.0 is -INF and 0.0/0.0 is
// NaN.)
func realArithResult(comp float64) Value {
	if math.IsInf(comp, 1) {
		return NewErrorValue()
	}
	return NewRealValue(comp)
}

func (e *Evaluator) evaluateAdd(left, right Value) Value {
	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}
	ln, li, liv, lrv := numericOperand(left)
	rn, ri, riv, rrv := numericOperand(right)
	if !ln || !rn {
		return NewErrorValue()
	}
	if li && ri {
		return NewIntValue(liv + riv)
	}
	return realArithResult(lrv + rrv)
}

func (e *Evaluator) evaluateSubtract(left, right Value) Value {
	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}
	ln, li, liv, lrv := numericOperand(left)
	rn, ri, riv, rrv := numericOperand(right)
	if !ln || !rn {
		return NewErrorValue()
	}
	if li && ri {
		return NewIntValue(liv - riv)
	}
	return realArithResult(lrv - rrv)
}

func (e *Evaluator) evaluateMultiply(left, right Value) Value {
	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}
	ln, li, liv, lrv := numericOperand(left)
	rn, ri, riv, rrv := numericOperand(right)
	if !ln || !rn {
		return NewErrorValue()
	}
	if li && ri {
		return NewIntValue(liv * riv)
	}
	return realArithResult(lrv * rrv)
}

func (e *Evaluator) evaluateDivide(left, right Value) Value {
	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}
	ln, li, liv, lrv := numericOperand(left)
	rn, ri, riv, rrv := numericOperand(right)
	if !ln || !rn {
		return NewErrorValue()
	}

	if li && ri {
		// Integer / integer yields an integer (truncated toward zero), matching
		// the C++ reference engine; integer division by zero is an error. Guard
		// the one signed-overflow case (MinInt64 / -1) that would panic in Go;
		// libclassad yields MaxInt64 there, so mirror that.
		if riv == 0 {
			return NewErrorValue()
		}
		if liv == math.MinInt64 && riv == -1 {
			return NewIntValue(math.MaxInt64)
		}
		return NewIntValue(liv / riv)
	}

	// Real division: division by zero produces +Inf/-Inf/NaN; only +Inf is an
	// error (handled by realArithResult), so -1.0/0.0 is -INF and 0.0/0.0 NaN.
	return realArithResult(lrv / rrv)
}

func (e *Evaluator) evaluateModulo(left, right Value) Value {
	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}
	ln, li, liv, _ := numericOperand(left)
	rn, ri, riv, _ := numericOperand(right)
	// Modulo requires integer-typed operands (booleans count as integers).
	if !ln || !rn || !li || !ri {
		return NewErrorValue()
	}

	if riv == 0 {
		return NewErrorValue()
	}

	// Any integer modulo ±1 is 0; special-casing -1 also avoids the
	// MinInt64 % -1 signed-overflow panic in Go.
	if riv == -1 {
		return NewIntValue(0)
	}

	return NewIntValue(liv % riv)
}

// evaluateBitwise implements the integer bitwise operators. Both operands must
// be genuine integers (a real or boolean operand is an error, unlike the
// arithmetic operators), and undefined propagates. Shift counts are masked with
// & 63 like the reference engine (and C on 64-bit), so the shift never panics
// and e.g. 1 << 64 == 1; >> is arithmetic (sign-extending) and >>> is logical.
func (e *Evaluator) evaluateBitwise(op string, left, right Value) Value {
	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}
	if !left.IsInteger() || !right.IsInteger() {
		return NewErrorValue()
	}
	l, _ := left.IntValue()
	r, _ := right.IntValue()
	switch op {
	case "&":
		return NewIntValue(l & r)
	case "|":
		return NewIntValue(l | r)
	case "^":
		return NewIntValue(l ^ r)
	case "<<":
		return NewIntValue(l << uint(r&63))
	case ">>":
		// Arithmetic right shift. For a negative value the reference engine
		// shifts one bit at a time, re-forcing the sign bit, for min(64, count)
		// steps -- so a count >= 64 saturates to -1 rather than wrapping.
		if l >= 0 {
			return NewIntValue(l >> uint(r&63))
		}
		steps := r
		if steps > 64 {
			steps = 64
		}
		val := l
		for i := int64(0); i < steps; i++ {
			val = (val >> 1) | math.MinInt64
		}
		return NewIntValue(val)
	case ">>>":
		// Logical right shift. The reference engine, for a negative value,
		// shifts right one, clears the sign bit, then shifts the remaining
		// count-1 (masked & 63) -- so e.g. (-29 >>> 0) is 0, not -29.
		if l >= 0 {
			return NewIntValue(l >> uint(r&63))
		}
		val := (l >> 1) & math.MaxInt64
		val >>= uint((r - 1) & 63)
		return NewIntValue(val)
	default:
		return NewErrorValue()
	}
}

// lowerASCII folds an ASCII upper-case byte to lower case, leaving every other
// byte unchanged. This matches the C ctype/strcasecmp behavior the reference
// engine uses (it is intentionally ASCII-only, not Unicode-aware).
func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// compareStringsFold compares two strings case-insensitively, byte for byte,
// like strcasecmp: it returns -1, 0, or 1. The relational and equality
// operators (< <= > >= == !=) use this because the C++ reference engine
// compares strings case-insensitively. (=?=/=!= remain case-sensitive; see
// evaluateIs.)
func compareStringsFold(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		ca, cb := lowerASCII(a[i]), lowerASCII(b[i])
		if ca != cb {
			if ca < cb {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	default:
		return 0
	}
}

// Comparison operations
func (e *Evaluator) evaluateLessThan(left, right Value) Value {
	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}

	if ln, _, _, lrv := numericOperand(left); ln {
		if rn, _, _, rrv := numericOperand(right); rn {
			return NewBoolValue(lrv < rrv)
		}
	}

	if left.IsString() && right.IsString() {
		leftStr, _ := left.StringValue()
		rightStr, _ := right.StringValue()
		return NewBoolValue(compareStringsFold(leftStr, rightStr) < 0)
	}

	return NewErrorValue()
}

func (e *Evaluator) evaluateGreaterThan(left, right Value) Value {
	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}

	if ln, _, _, lrv := numericOperand(left); ln {
		if rn, _, _, rrv := numericOperand(right); rn {
			return NewBoolValue(lrv > rrv)
		}
	}

	if left.IsString() && right.IsString() {
		leftStr, _ := left.StringValue()
		rightStr, _ := right.StringValue()
		return NewBoolValue(compareStringsFold(leftStr, rightStr) > 0)
	}

	return NewErrorValue()
}

func (e *Evaluator) evaluateLessOrEqual(left, right Value) Value {
	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}

	if ln, _, _, lrv := numericOperand(left); ln {
		if rn, _, _, rrv := numericOperand(right); rn {
			return NewBoolValue(lrv <= rrv)
		}
	}

	if left.IsString() && right.IsString() {
		leftStr, _ := left.StringValue()
		rightStr, _ := right.StringValue()
		return NewBoolValue(compareStringsFold(leftStr, rightStr) <= 0)
	}

	return NewErrorValue()
}

func (e *Evaluator) evaluateGreaterOrEqual(left, right Value) Value {
	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}

	if ln, _, _, lrv := numericOperand(left); ln {
		if rn, _, _, rrv := numericOperand(right); rn {
			return NewBoolValue(lrv >= rrv)
		}
	}

	if left.IsString() && right.IsString() {
		leftStr, _ := left.StringValue()
		rightStr, _ := right.StringValue()
		return NewBoolValue(compareStringsFold(leftStr, rightStr) >= 0)
	}

	return NewErrorValue()
}

func (e *Evaluator) evaluateEqual(left, right Value) Value {
	return valuesEqual(left, right)
}

// valuesEqual implements the == operator's value semantics (also used by
// member()): numeric types coerce across int/real/bool, strings compare
// case-insensitively, reals compare exactly, and any other cross-type or
// list/classad comparison is an error. Undefined propagates.
func valuesEqual(left, right Value) Value {
	// Regular == / != propagate undefined even when both sides are undefined
	// (undefined == undefined is undefined). Identity (=?=) handles the
	// "undefined is undefined -> true" case separately in evaluateIs.
	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}

	if left.Type() != right.Type() {
		// Numeric types (int/real/bool) coerce and compare across types; a
		// boolean compares as 1/0. Any other cross-type comparison -- e.g.
		// string vs int, or list/classad vs anything -- is an error in the
		// reference engine (coerceToNumber), not false.
		ln, li, liv, lrv := numericOperand(left)
		rn, ri, riv, rrv := numericOperand(right)
		if ln && rn {
			if li && ri {
				return NewBoolValue(liv == riv)
			}
			// The reference engine compares reals with exact IEEE equality
			// (no tolerance), so 0.1 + 0.2 == 0.3 is false.
			return NewBoolValue(lrv == rrv)
		}
		return NewErrorValue()
	}

	switch left.Type() {
	case BooleanValue:
		leftBool, _ := left.BoolValue()
		rightBool, _ := right.BoolValue()
		return NewBoolValue(leftBool == rightBool)
	case IntegerValue:
		leftInt, _ := left.IntValue()
		rightInt, _ := right.IntValue()
		return NewBoolValue(leftInt == rightInt)
	case RealValue:
		leftReal, _ := left.RealValue()
		rightReal, _ := right.RealValue()
		return NewBoolValue(leftReal == rightReal)
	case StringValue:
		leftStr, _ := left.StringValue()
		rightStr, _ := right.StringValue()
		return NewBoolValue(compareStringsFold(leftStr, rightStr) == 0)
	default:
		return NewErrorValue()
	}
}

func (e *Evaluator) evaluateNotEqual(left, right Value) Value {
	result := e.evaluateEqual(left, right)
	if result.IsError() || result.IsUndefined() {
		return result
	}
	boolVal, _ := result.BoolValue()
	return NewBoolValue(!boolVal)
}

// evaluateIs checks for strict identity (same type and same value).
// Unlike ==, this distinguishes between undefined and error, and does not perform type coercion.
func (e *Evaluator) evaluateIs(left, right Value) Value {
	// IS operator: strict identity check
	// - Different types -> false
	// - Same type -> compare values
	// - Undefined IS Undefined -> true
	// - Error IS Error -> true

	if left.Type() != right.Type() {
		return NewBoolValue(false)
	}

	switch left.Type() {
	case UndefinedValue:
		return NewBoolValue(true) // undefined is undefined
	case ErrorValue:
		return NewBoolValue(true) // error is error
	case BooleanValue:
		leftBool, _ := left.BoolValue()
		rightBool, _ := right.BoolValue()
		return NewBoolValue(leftBool == rightBool)
	case IntegerValue:
		leftInt, _ := left.IntValue()
		rightInt, _ := right.IntValue()
		return NewBoolValue(leftInt == rightInt)
	case RealValue:
		leftReal, _ := left.RealValue()
		rightReal, _ := right.RealValue()
		return NewBoolValue(leftReal == rightReal)
	case StringValue:
		leftStr, _ := left.StringValue()
		rightStr, _ := right.StringValue()
		return NewBoolValue(leftStr == rightStr)
	case ListValue, ClassAdValue:
		// The reference engine cannot compare lists or classads with =?= / =!=:
		// such a comparison is an error (only the type-mismatch case above
		// yields a boolean, e.g. {1} =?= 1 is false).
		return NewErrorValue()
	default:
		return NewErrorValue()
	}
}

// evaluateIsnt is the negation of evaluateIs.
func (e *Evaluator) evaluateIsnt(left, right Value) Value {
	result := e.evaluateIs(left, right)
	if result.IsError() {
		return result
	}
	boolVal, _ := result.BoolValue()
	return NewBoolValue(!boolVal)
}

// Logical operations.
//
// The reference engine uses three-valued logic with short-circuiting. Operands
// are coerced to booleans the same way the truthiness of a number is taken:
// a non-zero number is true, zero is false, undefined stays undefined, and any
// other type (string, list, classad, error) behaves like error. The
// short-circuit operand wins regardless of what the other side is, so
// "false && error" is false and "true || undefined" is true.

// logicalState is the boolean view of an operand for && / ||.
type logicalState int

const (
	lsFalse logicalState = iota
	lsTrue
	lsUndef
	lsErr // error, or a value not coercible to boolean
)

func logicalView(v Value) logicalState {
	switch v.valueType {
	case BooleanValue:
		if v.intVal != 0 {
			return lsTrue
		}
		return lsFalse
	case IntegerValue:
		if v.intVal != 0 {
			return lsTrue
		}
		return lsFalse
	case RealValue:
		if v.real() != 0 {
			return lsTrue
		}
		return lsFalse
	case UndefinedValue:
		return lsUndef
	default:
		return lsErr
	}
}

// mapState turns a logicalState back into a Value (used for the non-short-
// circuiting branch where the result is simply the other operand's truth).
func mapState(s logicalState) Value {
	switch s {
	case lsTrue:
		return NewBoolValue(true)
	case lsFalse:
		return NewBoolValue(false)
	case lsUndef:
		return NewUndefinedValue()
	default:
		return NewErrorValue()
	}
}

// evaluateShortCircuit evaluates a && / || expression, deferring evaluation of
// the right operand until the left operand's truth value requires it. A false
// (for &&) or true (for ||) left operand decides the result outright; an error
// left operand is an error; only a true/undefined (&&) or false/undefined (||)
// left operand evaluates the right and applies the full three-valued logic.
func (e *Evaluator) evaluateShortCircuit(op *ast.BinaryOp) Value {
	left := e.Evaluate(op.Left)
	switch logicalView(left) {
	case lsErr:
		return NewErrorValue()
	case lsFalse:
		if op.Op == "&&" {
			return NewBoolValue(false)
		}
	case lsTrue:
		if op.Op == "||" {
			return NewBoolValue(true)
		}
	}
	right := e.Evaluate(op.Right)
	if op.Op == "&&" {
		return e.evaluateAnd(left, right)
	}
	return e.evaluateOr(left, right)
}

func (e *Evaluator) evaluateAnd(left, right Value) Value {
	l, r := logicalView(left), logicalView(right)
	switch l {
	case lsFalse:
		return NewBoolValue(false) // false && anything == false
	case lsErr:
		return NewErrorValue()
	case lsTrue:
		return mapState(r) // true && x == x
	default: // lsUndef
		switch r {
		case lsFalse:
			return NewBoolValue(false)
		case lsErr:
			return NewErrorValue()
		default: // true or undefined
			return NewUndefinedValue()
		}
	}
}

func (e *Evaluator) evaluateOr(left, right Value) Value {
	l, r := logicalView(left), logicalView(right)
	switch l {
	case lsTrue:
		return NewBoolValue(true) // true || anything == true
	case lsErr:
		return NewErrorValue()
	case lsFalse:
		return mapState(r) // false || x == x
	default: // lsUndef
		switch r {
		case lsTrue:
			return NewBoolValue(true)
		case lsErr:
			return NewErrorValue()
		default: // false or undefined
			return NewUndefinedValue()
		}
	}
}

// evaluateUnparse handles the unparse() function which returns the string representation
// of an attribute's expression without evaluating it.
// unparse(attribute_name) - Returns the unparsed expression string for the given attribute
func (e *Evaluator) evaluateUnparse(args []ast.Expr) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}

	// The argument should be an attribute reference
	attrRef, ok := args[0].(*ast.AttributeReference)
	if !ok {
		return NewErrorValue()
	}

	// Determine which ClassAd to look up the attribute in based on scope
	var targetClassAd *ClassAd
	switch attrRef.Scope {
	case ast.MyScope:
		targetClassAd = e.classad
	case ast.TargetScope:
		if e.classad != nil {
			targetClassAd = e.classad.target
		}
	case ast.ParentScope:
		if e.classad != nil {
			targetClassAd = e.classad.parent
		}
	default:
		targetClassAd = e.classad
	}

	if targetClassAd == nil {
		return NewUndefinedValue()
	}

	// Look up the attribute's expression (not evaluated)
	expr := targetClassAd.lookupInternal(attrRef.Name)
	if expr == nil {
		return NewUndefinedValue()
	}

	// Return the string representation of the expression
	return NewStringValue(expr.String())
}

// evaluateEval handles the eval() function which parses and evaluates a string expression
// in the context of the current ClassAd.
// eval(string_expr) - Parses the string as a ClassAd expression and evaluates it
func (e *Evaluator) evaluateEval(args []ast.Expr) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}

	// Evaluate the argument to get the string to parse
	val := e.Evaluate(args[0])

	if val.IsError() {
		return NewErrorValue()
	}
	if val.IsUndefined() {
		return NewUndefinedValue()
	}
	if !val.IsString() {
		return NewErrorValue()
	}

	exprStr, _ := val.StringValue()

	// The parser expects a full ClassAd, so we wrap the expression in a temporary attribute
	// Parse it as "[__eval_tmp = <expression>]" and extract the expression
	wrappedStr := "[__eval_tmp = " + exprStr + "]"

	node, err := parser.Parse(wrappedStr)
	if err != nil {
		return NewErrorValue()
	}

	// The result should be a ClassAd
	classAd, ok := node.(*ast.ClassAd)
	if !ok || len(classAd.Attributes) != 1 {
		return NewErrorValue()
	}

	// Extract the expression from the temporary attribute
	expr := classAd.Attributes[0].Value

	// Evaluate the expression in the current context
	return e.Evaluate(expr)
}

// evaluateIfThenElse implements ifThenElse(cond, t, f) with the same lazy,
// number-coercing semantics as the ?: operator (see evaluateConditional): the
// condition's truthiness selects a branch and only that branch is evaluated, so
// a self-referential or erroring unselected branch is never touched
// (ifThenElse(1, 0, B) is 0). Undefined yields undefined; a non-coercible
// condition is an error.
func (e *Evaluator) evaluateIfThenElse(args []ast.Expr) Value {
	if len(args) != 3 {
		return NewErrorValue()
	}
	switch logicalView(e.Evaluate(args[0])) {
	case lsTrue:
		return e.Evaluate(args[1])
	case lsFalse:
		return e.Evaluate(args[2])
	case lsUndef:
		return NewUndefinedValue()
	default:
		return NewErrorValue()
	}
}

// funcArity is the accepted argument-count range of a built-in: [min, max],
// with max == -1 meaning unbounded (variadic).
type funcArity struct {
	min, max int
}

func (a funcArity) accepts(n int) bool {
	return n >= a.min && (a.max < 0 || n <= a.max)
}

// functionArity is the set of built-in function names (lower-cased) the engine
// recognizes, each mapped to its accepted argument-count range. It is used to
// reject an unknown function -- or a known function called with the wrong
// number of arguments -- BEFORE evaluating its arguments, matching the
// reference engine (which checks arity first, so a cyclic/erroring argument to
// a wrong-arity call is never evaluated). It must list every name handled by
// evaluateFunctionCall (the switch below plus the specially-cased
// unparse/eval/ifthenelse); TestKnownFunctionsCoversDispatch guards that they
// stay in sync.
var functionArity = map[string]funcArity{
	"unparse": {1, 1}, "eval": {1, 1}, "ifthenelse": {3, 3},
	"strcat": {0, -1}, "substr": {2, 3}, "size": {1, 1}, "tolower": {1, 1},
	"toupper": {1, 1}, "floor": {1, 1}, "ceiling": {1, 1}, "ceil": {1, 1},
	"round": {1, 1}, "random": {0, 1}, "int": {1, 1}, "real": {1, 1},
	"isundefined": {1, 1}, "iserror": {1, 1}, "isstring": {1, 1},
	"isinteger": {1, 1}, "isreal": {1, 1}, "isboolean": {1, 1},
	"islist": {1, 1}, "isclassad": {1, 1}, "time": {0, 0}, "member": {2, 2},
	"stringlistmember": {2, 3}, "stringlistimember": {2, 3}, "regexp": {2, 3},
	"string": {1, 1}, "bool": {1, 1}, "pow": {2, 2}, "quantize": {2, 2},
	"sum": {1, 1}, "avg": {1, 1}, "min": {1, 1}, "max": {1, 1}, "join": {1, -1},
	"split": {1, 2}, "splitusername": {1, 1}, "splitslotname": {1, 1},
	"strcmp": {2, 2}, "stricmp": {2, 2}, "versioncmp": {2, 2},
	"version_in_range": {3, 3},
	"formattime":       {0, 2}, "interval": {1, 1}, "identicalmember": {2, 2},
	"anycompare": {3, 3}, "allcompare": {3, 3}, "stringlistsize": {1, 2},
	"stringlistsum": {1, 2}, "stringlistavg": {1, 2}, "stringlistmin": {1, 2},
	"stringlistmax": {1, 2}, "stringlistsintersect": {2, 3},
	"stringlistsubsetmatch": {2, 3}, "stringlistregexpmember": {2, 4},
	"regexpmember": {2, 3}, "regexps": {3, 4}, "replace": {3, 4},
	"replaceall": {3, 4},
}

// evaluateStrcat implements strcat with the reference engine's short-circuit:
// arguments are evaluated left-to-right and evaluation stops at the first
// undefined or error one, so a later cyclic argument is never reached --
// strcat(undefined, A2) with a self-referential A2 is undefined, not error.
// (Lists and ads are concatenable: they stringify via unparse, so strcat does
// not stop on them.) The collected prefix (through the first undefined/error
// value) is handed to builtinStrcat, whose flag logic would have stopped at the
// same point. A cyclic argument reached before any stop still propagates (its
// Evaluate panics) and fails the call.
func (e *Evaluator) evaluateStrcat(argExprs []ast.Expr) Value {
	args := make([]Value, 0, len(argExprs))
	for _, ax := range argExprs {
		v := e.Evaluate(ax)
		args = append(args, v)
		if v.IsUndefined() || v.IsError() {
			break // strcat stops at the first undefined/error argument
		}
	}
	return builtinStrcat(args)
}

// Built-in function evaluation
func (e *Evaluator) evaluateFunctionCall(fc *ast.FunctionCall) Value {
	// Function names are matched case-insensitively, like the reference engine
	// (so SUBSTR, suBstr and substr are the same function).
	funcName := strings.ToLower(fc.Name)

	// Handle unparse() specially - it needs access to the raw AST and ClassAd context
	if funcName == "unparse" {
		return e.evaluateUnparse(fc.Args)
	}

	// Handle eval() specially - it needs to parse and evaluate in the current context
	if funcName == "eval" {
		return e.evaluateEval(fc.Args)
	}

	// Handle ifThenElse() specially - like the ?: operator it evaluates only
	// the selected branch (so ifThenElse(1, 0, B) is 0 and never evaluates a
	// self-referential B), rather than eagerly evaluating all arguments.
	if funcName == "ifthenelse" {
		return e.evaluateIfThenElse(fc.Args)
	}

	// An unknown function, or a known function called with the wrong number of
	// arguments, is an error and its arguments are NOT evaluated, matching the
	// reference engine (which checks arity before evaluating args). So
	// A((A0)) and pow(A0) -- with A0 the attribute itself -- are error without
	// the cyclic argument ever being evaluated.
	arity, known := functionArity[funcName]
	if !known || !arity.accepts(len(fc.Args)) {
		return NewErrorValue()
	}

	// strcat evaluates its arguments left-to-right and stops at the first one it
	// cannot concatenate (undefined/error/list/ad), so a later cyclic argument
	// is never reached: strcat(undefined, A2) with a cyclic A2 is undefined.
	if funcName == "strcat" {
		return e.evaluateStrcat(fc.Args)
	}

	// Evaluate all arguments. A cyclic argument propagates (the reference
	// engine's argument Evaluate returns false on a cycle, failing the call):
	// strcmp(undefined, A2) with a cyclic A2 is error, not undefined.
	args := make([]Value, len(fc.Args))
	for i, arg := range fc.Args {
		args[i] = e.Evaluate(arg)
	}

	// Dispatch to the appropriate function (funcName is already lower-cased)
	switch funcName {
	// String functions
	case "strcat":
		return builtinStrcat(args)
	case "substr":
		return builtinSubstr(args)
	case "size":
		return builtinSize(args)
	case "tolower":
		return builtinToLower(args)
	case "toupper":
		return builtinToUpper(args)

	// Math functions
	case "floor":
		return builtinFloor(args)
	case "ceiling", "ceil":
		return builtinCeiling(args)
	case "round":
		return builtinRound(args)
	case "random":
		return builtinRandom(args)
	case "int":
		return builtinInt(args)
	case "real":
		return builtinReal(args)

	// Type checking functions
	case "isundefined":
		return builtinIsUndefined(args)
	case "iserror":
		return builtinIsError(args)
	case "isstring":
		return builtinIsString(args)
	case "isinteger":
		return builtinIsInteger(args)
	case "isreal":
		return builtinIsReal(args)
	case "isboolean":
		return builtinIsBoolean(args)
	case "islist":
		return builtinIsList(args)
	case "isclassad":
		return builtinIsClassAd(args)

	// Time functions
	case "time":
		return builtinTime(args)

	// List functions
	case "member":
		return builtinMember(args)
	case "stringlistmember":
		return builtinStringListMember(args)
	case "stringlistimember":
		return builtinStringListIMember(args)

	// Pattern matching functions
	case "regexp":
		return builtinRegexp(args)

	// Control flow functions
	// ifThenElse is handled lazily before argument evaluation (above).

	// Type conversion functions
	case "string":
		return builtinString(args)
	case "bool":
		return builtinBool(args)

	// Math functions
	case "pow":
		return builtinPow(args)
	case "quantize":
		return builtinQuantize(args)

	// List aggregation functions
	case "sum":
		return builtinSum(args)
	case "avg":
		return builtinAvg(args)
	case "min":
		return builtinMin(args)
	case "max":
		return builtinMax(args)

	// String manipulation functions
	case "join":
		return builtinJoin(args)
	case "split":
		return builtinSplit(args)
	case "splitusername":
		return builtinSplitUserName(args)
	case "splitslotname":
		return builtinSplitSlotName(args)
	case "strcmp":
		return builtinStrcmp(args)
	case "stricmp":
		return builtinStricmp(args)

	// Version comparison functions. (The reference engine's per-operator helpers
	// are spelled versionGT/GE/LT/LE/EQ -- camelCase and case-sensitive, see
	// CPP_QUIRKS.md -- which Go's case-insensitive dispatch cannot represent, so
	// they are intentionally not provided here. versioncmp and version_in_range
	// are the lowercase-friendly ones.)
	case "versioncmp":
		return builtinVersioncmp(args)
	case "version_in_range":
		return builtinVersionInRange(args)

	// Time formatting functions
	case "formattime":
		return builtinFormatTime(args)
	case "interval":
		return builtinInterval(args)

	// List comparison functions
	case "identicalmember":
		return builtinIdenticalMember(args)
	case "anycompare":
		return builtinAnyCompare(args)
	case "allcompare":
		return builtinAllCompare(args)

	// StringList functions
	case "stringlistsize":
		return builtinStringListSize(args)
	case "stringlistsum":
		return builtinStringListSum(args)
	case "stringlistavg":
		return builtinStringListAvg(args)
	case "stringlistmin":
		return builtinStringListMin(args)
	case "stringlistmax":
		return builtinStringListMax(args)
	case "stringlistsintersect":
		return builtinStringListsIntersect(args)
	case "stringlistsubsetmatch":
		return builtinStringListSubsetMatch(args)
	case "stringlistregexpmember":
		return builtinStringListRegexpMember(args)

	// Regex functions
	case "regexpmember":
		return builtinRegexpMember(args)
	case "regexps":
		return builtinRegexps(args)
	case "replace":
		return builtinReplace(args)
	case "replaceall":
		return builtinReplaceAll(args)

	default:
		// Unknown function
		return NewErrorValue()
	}
}
