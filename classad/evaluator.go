package classad

import (
	"fmt"
	"math"

	"github.com/PelicanPlatform/classad/ast"
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
type Value struct {
	valueType  ValueType
	boolVal    bool
	intVal     int64
	realVal    float64
	strVal     string
	listVal    []Value
	classAdVal *ClassAd
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
	return Value{valueType: BooleanValue, boolVal: b}
}

// NewIntValue creates an integer value.
func NewIntValue(i int64) Value {
	return Value{valueType: IntegerValue, intVal: i}
}

// NewRealValue creates a real value.
func NewRealValue(r float64) Value {
	return Value{valueType: RealValue, realVal: r}
}

// NewStringValue creates a string value.
func NewStringValue(s string) Value {
	return Value{valueType: StringValue, strVal: s}
}

// NewListValue creates a list value.
func NewListValue(list []Value) Value {
	return Value{valueType: ListValue, listVal: list}
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
	return v.boolVal, nil
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
	return v.realVal, nil
}

// NumberValue returns the numeric value as a float64, converting integers if needed.
func (v Value) NumberValue() (float64, error) {
	switch v.valueType {
	case IntegerValue:
		return float64(v.intVal), nil
	case RealValue:
		return v.realVal, nil
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
	return v.listVal, nil
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
		if v.boolVal {
			return "true"
		}
		return "false"
	case IntegerValue:
		return fmt.Sprintf("%d", v.intVal)
	case RealValue:
		return fmt.Sprintf("%g", v.realVal)
	case StringValue:
		return fmt.Sprintf("%q", v.strVal)
	case ListValue:
		return fmt.Sprintf("%v", v.listVal)
	case ClassAdValue:
		if v.classAdVal != nil {
			return v.classAdVal.String()
		}
		return "[]"
	default:
		return "unknown"
	}
}

// Evaluator handles evaluation of ClassAd expressions.
type Evaluator struct {
	classad *ClassAd
}

// NewEvaluator creates a new evaluator for the given ClassAd.
func NewEvaluator(ad *ClassAd) *Evaluator {
	return &Evaluator{classad: ad}
}

// Evaluate evaluates an expression in the context of the ClassAd.
func (e *Evaluator) Evaluate(expr ast.Expr) Value {
	if expr == nil {
		return NewUndefinedValue()
	}

	switch v := expr.(type) {
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
	var targetClassAd *ClassAd

	// Determine which ClassAd to look up the attribute in based on scope
	switch ref.Scope {
	case ast.MyScope:
		// MY.attr - always refers to the current ClassAd
		targetClassAd = e.classad
	case ast.TargetScope:
		// TARGET.attr - refers to the target ClassAd
		if e.classad != nil {
			targetClassAd = e.classad.target
		}
	case ast.ParentScope:
		// PARENT.attr - refers to the parent ClassAd
		if e.classad != nil {
			targetClassAd = e.classad.parent
		}
	default:
		// No scope - look in current ClassAd
		targetClassAd = e.classad
	}

	if targetClassAd == nil {
		return NewUndefinedValue()
	}

	expr := targetClassAd.lookupInternal(ref.Name)
	if expr == nil {
		return NewUndefinedValue()
	}

	// Create evaluator for the target ClassAd
	evaluator := NewEvaluator(targetClassAd)
	return evaluator.Evaluate(expr)
}

func (e *Evaluator) evaluateBinaryOp(op *ast.BinaryOp) Value {
	left := e.Evaluate(op.Left)
	right := e.Evaluate(op.Right)

	// For 'is' and 'isnt' operators, don't propagate errors - they are part of the comparison
	if op.Op == "is" {
		return e.evaluateIs(left, right)
	}
	if op.Op == "isnt" {
		return e.evaluateIsnt(left, right)
	}

	// Handle error propagation for other operators
	if left.IsError() || right.IsError() {
		return NewErrorValue()
	}

	// Arithmetic operators
	switch op.Op {
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

	// Logical operators
	case "&&":
		return e.evaluateAnd(left, right)
	case "||":
		return e.evaluateOr(left, right)

	default:
		return NewErrorValue()
	}
}

func (e *Evaluator) evaluateUnaryOp(op *ast.UnaryOp) Value {
	val := e.Evaluate(op.Expr)

	if val.IsError() {
		return val
	}

	switch op.Op {
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
		if val.IsBool() {
			boolVal, _ := val.BoolValue()
			return NewBoolValue(!boolVal)
		}
		return NewErrorValue()

	default:
		return NewErrorValue()
	}
}

func (e *Evaluator) evaluateConditional(cond *ast.ConditionalExpr) Value {
	condVal := e.Evaluate(cond.Condition)

	if condVal.IsError() {
		return NewErrorValue()
	}

	if condVal.IsUndefined() {
		return NewUndefinedValue()
	}

	if !condVal.IsBool() {
		return NewErrorValue()
	}

	boolVal, _ := condVal.BoolValue()
	if boolVal {
		return e.Evaluate(cond.TrueExpr)
	}
	return e.Evaluate(cond.FalseExpr)
}

func (e *Evaluator) evaluateList(list *ast.ListLiteral) Value {
	values := make([]Value, len(list.Elements))
	for i, elem := range list.Elements {
		values[i] = e.Evaluate(elem)
	}
	return NewListValue(values)
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

	// Must be a ClassAd
	if !recordVal.IsClassAd() {
		return NewErrorValue()
	}

	// Get the ClassAd and lookup the attribute
	ad, _ := recordVal.ClassAdValue()
	return ad.EvaluateAttr(sel.Attr)
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

		list, _ := containerVal.ListValue()
		index, _ := indexVal.IntValue()

		// Check bounds
		if index < 0 || index >= int64(len(list)) {
			return NewUndefinedValue()
		}

		return list[index]
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
func (e *Evaluator) evaluateAdd(left, right Value) Value {
	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}

	if !left.IsNumber() || !right.IsNumber() {
		return NewErrorValue()
	}

	leftNum, _ := left.NumberValue()
	rightNum, _ := right.NumberValue()

	if left.IsInteger() && right.IsInteger() {
		leftInt, _ := left.IntValue()
		rightInt, _ := right.IntValue()
		return NewIntValue(leftInt + rightInt)
	}

	return NewRealValue(leftNum + rightNum)
}

func (e *Evaluator) evaluateSubtract(left, right Value) Value {
	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}

	if !left.IsNumber() || !right.IsNumber() {
		return NewErrorValue()
	}

	leftNum, _ := left.NumberValue()
	rightNum, _ := right.NumberValue()

	if left.IsInteger() && right.IsInteger() {
		leftInt, _ := left.IntValue()
		rightInt, _ := right.IntValue()
		return NewIntValue(leftInt - rightInt)
	}

	return NewRealValue(leftNum - rightNum)
}

func (e *Evaluator) evaluateMultiply(left, right Value) Value {
	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}

	if !left.IsNumber() || !right.IsNumber() {
		return NewErrorValue()
	}

	leftNum, _ := left.NumberValue()
	rightNum, _ := right.NumberValue()

	if left.IsInteger() && right.IsInteger() {
		leftInt, _ := left.IntValue()
		rightInt, _ := right.IntValue()
		return NewIntValue(leftInt * rightInt)
	}

	return NewRealValue(leftNum * rightNum)
}

func (e *Evaluator) evaluateDivide(left, right Value) Value {
	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}

	if !left.IsNumber() || !right.IsNumber() {
		return NewErrorValue()
	}

	rightNum, _ := right.NumberValue()
	if rightNum == 0 {
		return NewErrorValue()
	}

	leftNum, _ := left.NumberValue()

	if left.IsInteger() && right.IsInteger() {
		leftInt, _ := left.IntValue()
		rightInt, _ := right.IntValue()
		if leftInt%rightInt == 0 {
			return NewIntValue(leftInt / rightInt)
		}
	}

	return NewRealValue(leftNum / rightNum)
}

func (e *Evaluator) evaluateModulo(left, right Value) Value {
	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}

	if !left.IsInteger() || !right.IsInteger() {
		return NewErrorValue()
	}

	leftInt, _ := left.IntValue()
	rightInt, _ := right.IntValue()

	if rightInt == 0 {
		return NewErrorValue()
	}

	return NewIntValue(leftInt % rightInt)
}

// Comparison operations
func (e *Evaluator) evaluateLessThan(left, right Value) Value {
	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}

	if left.IsNumber() && right.IsNumber() {
		leftNum, _ := left.NumberValue()
		rightNum, _ := right.NumberValue()
		return NewBoolValue(leftNum < rightNum)
	}

	if left.IsString() && right.IsString() {
		leftStr, _ := left.StringValue()
		rightStr, _ := right.StringValue()
		return NewBoolValue(leftStr < rightStr)
	}

	return NewErrorValue()
}

func (e *Evaluator) evaluateGreaterThan(left, right Value) Value {
	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}

	if left.IsNumber() && right.IsNumber() {
		leftNum, _ := left.NumberValue()
		rightNum, _ := right.NumberValue()
		return NewBoolValue(leftNum > rightNum)
	}

	if left.IsString() && right.IsString() {
		leftStr, _ := left.StringValue()
		rightStr, _ := right.StringValue()
		return NewBoolValue(leftStr > rightStr)
	}

	return NewErrorValue()
}

func (e *Evaluator) evaluateLessOrEqual(left, right Value) Value {
	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}

	if left.IsNumber() && right.IsNumber() {
		leftNum, _ := left.NumberValue()
		rightNum, _ := right.NumberValue()
		return NewBoolValue(leftNum <= rightNum)
	}

	if left.IsString() && right.IsString() {
		leftStr, _ := left.StringValue()
		rightStr, _ := right.StringValue()
		return NewBoolValue(leftStr <= rightStr)
	}

	return NewErrorValue()
}

func (e *Evaluator) evaluateGreaterOrEqual(left, right Value) Value {
	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}

	if left.IsNumber() && right.IsNumber() {
		leftNum, _ := left.NumberValue()
		rightNum, _ := right.NumberValue()
		return NewBoolValue(leftNum >= rightNum)
	}

	if left.IsString() && right.IsString() {
		leftStr, _ := left.StringValue()
		rightStr, _ := right.StringValue()
		return NewBoolValue(leftStr >= rightStr)
	}

	return NewErrorValue()
}

func (e *Evaluator) evaluateEqual(left, right Value) Value {
	if left.IsUndefined() && right.IsUndefined() {
		return NewBoolValue(true)
	}

	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}

	if left.Type() != right.Type() {
		// Allow numeric comparison between int and real
		if left.IsNumber() && right.IsNumber() {
			leftNum, _ := left.NumberValue()
			rightNum, _ := right.NumberValue()
			return NewBoolValue(math.Abs(leftNum-rightNum) < 1e-9)
		}
		return NewBoolValue(false)
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
		return NewBoolValue(math.Abs(leftReal-rightReal) < 1e-9)
	case StringValue:
		leftStr, _ := left.StringValue()
		rightStr, _ := right.StringValue()
		return NewBoolValue(leftStr == rightStr)
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
	case ListValue:
		// Lists: compare element-wise
		leftList, _ := left.ListValue()
		rightList, _ := right.ListValue()
		if len(leftList) != len(rightList) {
			return NewBoolValue(false)
		}
		for i := range leftList {
			elemResult := e.evaluateIs(leftList[i], rightList[i])
			if elemResult.IsError() {
				return elemResult
			}
			match, _ := elemResult.BoolValue()
			if !match {
				return NewBoolValue(false)
			}
		}
		return NewBoolValue(true)
	case ClassAdValue:
		// ClassAds: pointer comparison (same object)
		leftAd, _ := left.ClassAdValue()
		rightAd, _ := right.ClassAdValue()
		return NewBoolValue(leftAd == rightAd)
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

// Logical operations
func (e *Evaluator) evaluateAnd(left, right Value) Value {
	if left.IsError() || right.IsError() {
		return NewErrorValue()
	}

	// Short-circuit evaluation
	if left.IsBool() {
		leftBool, _ := left.BoolValue()
		if !leftBool {
			return NewBoolValue(false)
		}
	}

	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}

	if !left.IsBool() || !right.IsBool() {
		return NewErrorValue()
	}

	leftBool, _ := left.BoolValue()
	rightBool, _ := right.BoolValue()
	return NewBoolValue(leftBool && rightBool)
}

func (e *Evaluator) evaluateOr(left, right Value) Value {
	if left.IsError() || right.IsError() {
		return NewErrorValue()
	}

	// Short-circuit evaluation
	if left.IsBool() {
		leftBool, _ := left.BoolValue()
		if leftBool {
			return NewBoolValue(true)
		}
	}

	if left.IsUndefined() || right.IsUndefined() {
		return NewUndefinedValue()
	}

	if !left.IsBool() || !right.IsBool() {
		return NewErrorValue()
	}

	leftBool, _ := left.BoolValue()
	rightBool, _ := right.BoolValue()
	return NewBoolValue(leftBool || rightBool)
}

// Built-in function evaluation
func (e *Evaluator) evaluateFunctionCall(fc *ast.FunctionCall) Value {
	// Evaluate all arguments
	args := make([]Value, len(fc.Args))
	for i, arg := range fc.Args {
		args[i] = e.Evaluate(arg)
	}

	// Dispatch to the appropriate function
	switch fc.Name {
	// String functions
	case "strcat":
		return builtinStrcat(args)
	case "substr":
		return builtinSubstr(args)
	case "size":
		return builtinSize(args)
	case "length":
		return builtinLength(args)
	case "toLower", "tolower":
		return builtinToLower(args)
	case "toUpper", "toupper":
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
	case "isUndefined":
		return builtinIsUndefined(args)
	case "isError":
		return builtinIsError(args)
	case "isString":
		return builtinIsString(args)
	case "isInteger":
		return builtinIsInteger(args)
	case "isReal":
		return builtinIsReal(args)
	case "isBoolean":
		return builtinIsBoolean(args)
	case "isList":
		return builtinIsList(args)
	case "isClassAd":
		return builtinIsClassAd(args)

	// Time functions
	case "time":
		return builtinTime(args)

	// List functions
	case "member":
		return builtinMember(args)
	case "stringListMember":
		return builtinStringListMember(args)

	// Pattern matching functions
	case "regexp":
		return builtinRegexp(args)

	// Control flow functions
	case "ifThenElse":
		return builtinIfThenElse(args)

	default:
		// Unknown function
		return NewErrorValue()
	}
}
