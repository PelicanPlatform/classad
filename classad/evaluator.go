package classad

import (
	"fmt"
	"math"

	"github.com/bbockelm/golang-classads/ast"
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

	default:
		return NewErrorValue()
	}
}

func (e *Evaluator) evaluateAttributeReference(ref *ast.AttributeReference) Value {
	if e.classad == nil {
		return NewUndefinedValue()
	}

	expr := e.classad.Lookup(ref.Name)
	if expr == nil {
		return NewUndefinedValue()
	}

	return e.Evaluate(expr)
}

func (e *Evaluator) evaluateBinaryOp(op *ast.BinaryOp) Value {
	left := e.Evaluate(op.Left)
	right := e.Evaluate(op.Right)

	// Handle error propagation
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
