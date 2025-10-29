package classad

import (
	"testing"
)

func TestValueTypes(t *testing.T) {
	tests := []struct {
		name      string
		value     Value
		typeCheck func(Value) bool
	}{
		{"undefined", NewUndefinedValue(), func(v Value) bool { return v.IsUndefined() }},
		{"error", NewErrorValue(), func(v Value) bool { return v.IsError() }},
		{"boolean", NewBoolValue(true), func(v Value) bool { return v.IsBool() }},
		{"integer", NewIntValue(42), func(v Value) bool { return v.IsInteger() }},
		{"real", NewRealValue(3.14), func(v Value) bool { return v.IsReal() }},
		{"string", NewStringValue("test"), func(v Value) bool { return v.IsString() }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.typeCheck(tt.value) {
				t.Errorf("Type check failed for %s", tt.name)
			}
		})
	}
}

func TestValueGetters(t *testing.T) {
	// Test Boolean
	bval := NewBoolValue(true)
	if b, err := bval.BoolValue(); err != nil || !b {
		t.Errorf("BoolValue() failed: got %v, err=%v", b, err)
	}

	// Test Integer
	ival := NewIntValue(42)
	if i, err := ival.IntValue(); err != nil || i != 42 {
		t.Errorf("IntValue() failed: got %d, err=%v", i, err)
	}

	// Test Real
	rval := NewRealValue(3.14)
	if r, err := rval.RealValue(); err != nil || r != 3.14 {
		t.Errorf("RealValue() failed: got %g, err=%v", r, err)
	}

	// Test String
	sval := NewStringValue("hello")
	if s, err := sval.StringValue(); err != nil || s != "hello" {
		t.Errorf("StringValue() failed: got %s, err=%v", s, err)
	}

	// Test invalid access
	if _, err := ival.BoolValue(); err == nil {
		t.Error("BoolValue() on Integer should fail")
	}
}

func TestValueToString(t *testing.T) {
	tests := []struct {
		name     string
		value    Value
		expected string
	}{
		{"undefined", NewUndefinedValue(), "undefined"},
		{"error", NewErrorValue(), "error"},
		{"boolean true", NewBoolValue(true), "true"},
		{"boolean false", NewBoolValue(false), "false"},
		{"integer", NewIntValue(42), "42"},
		{"real", NewRealValue(3.14), "3.14"},
		{"string", NewStringValue("test"), `"test"`}, // Strings include quotes
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.value.String()
			if result != tt.expected {
				t.Errorf("Expected %s, got %s", tt.expected, result)
			}
		})
	}
}

func TestArithmeticOperations(t *testing.T) {
	tests := []struct {
		name     string
		left     Value
		evalFunc func(*Evaluator, Value, Value) Value
		right    Value
		expected Value
	}{
		{"add", NewIntValue(2), (*Evaluator).evaluateAdd, NewIntValue(3), NewIntValue(5)},
		{"subtract", NewIntValue(10), (*Evaluator).evaluateSubtract, NewIntValue(3), NewIntValue(7)},
		{"multiply", NewIntValue(4), (*Evaluator).evaluateMultiply, NewIntValue(5), NewIntValue(20)},
		{"divide", NewIntValue(20), (*Evaluator).evaluateDivide, NewIntValue(4), NewIntValue(5)},
		{"modulo", NewIntValue(17), (*Evaluator).evaluateModulo, NewIntValue(5), NewIntValue(2)},
		{"real add", NewRealValue(2.5), (*Evaluator).evaluateAdd, NewRealValue(3.5), NewRealValue(6.0)},
		{"real multiply", NewRealValue(2.5), (*Evaluator).evaluateMultiply, NewRealValue(4.0), NewRealValue(10.0)},
		{"mixed add", NewIntValue(2), (*Evaluator).evaluateAdd, NewRealValue(3.5), NewRealValue(5.5)},
		{"divide by zero", NewIntValue(10), (*Evaluator).evaluateDivide, NewIntValue(0), NewErrorValue()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eval := &Evaluator{}
			result := tt.evalFunc(eval, tt.left, tt.right)

			if result.IsError() != tt.expected.IsError() {
				t.Errorf("Error mismatch: got %v, expected %v", result, tt.expected)
				return
			}

			if result.IsError() {
				return // Both are errors, test passes
			}

			if result.Type() != tt.expected.Type() {
				t.Errorf("Type mismatch: got %v, expected %v", result.Type(), tt.expected.Type())
				return
			}

			switch result.Type() {
			case IntegerValue:
				r, _ := result.IntValue()
				e, _ := tt.expected.IntValue()
				if r != e {
					t.Errorf("Expected %d, got %d", e, r)
				}
			case RealValue:
				r, _ := result.RealValue()
				e, _ := tt.expected.RealValue()
				if r != e {
					t.Errorf("Expected %g, got %g", e, r)
				}
			}
		})
	}
}

func TestComparisonOperations(t *testing.T) {
	tests := []struct {
		name     string
		left     Value
		evalFunc func(*Evaluator, Value, Value) Value
		right    Value
		expected bool
	}{
		{"less than", NewIntValue(3), (*Evaluator).evaluateLessThan, NewIntValue(5), true},
		{"not less", NewIntValue(5), (*Evaluator).evaluateLessThan, NewIntValue(3), false},
		{"greater", NewIntValue(5), (*Evaluator).evaluateGreaterThan, NewIntValue(3), true},
		{"less or equal true", NewIntValue(3), (*Evaluator).evaluateLessOrEqual, NewIntValue(3), true},
		{"less or equal false", NewIntValue(5), (*Evaluator).evaluateLessOrEqual, NewIntValue(3), false},
		{"equal", NewIntValue(5), (*Evaluator).evaluateEqual, NewIntValue(5), true},
		{"not equal", NewIntValue(5), (*Evaluator).evaluateNotEqual, NewIntValue(3), true},
		{"real comparison", NewRealValue(3.14), (*Evaluator).evaluateLessThan, NewRealValue(3.15), true},
		{"string equal", NewStringValue("hello"), (*Evaluator).evaluateEqual, NewStringValue("hello"), true},
		{"string not equal", NewStringValue("hello"), (*Evaluator).evaluateNotEqual, NewStringValue("world"), true},
		{"bool equal", NewBoolValue(true), (*Evaluator).evaluateEqual, NewBoolValue(true), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eval := &Evaluator{}
			result := tt.evalFunc(eval, tt.left, tt.right)

			if result.IsError() {
				t.Fatalf("Comparison returned error: %v", result)
			}

			b, err := result.BoolValue()
			if err != nil {
				t.Fatal("Comparison did not return boolean")
			}

			if b != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, b)
			}
		})
	}
}

func TestLogicalOperations(t *testing.T) {
	tests := []struct {
		name     string
		left     Value
		evalFunc func(*Evaluator, Value, Value) Value
		right    Value
		expected bool
	}{
		{"and true", NewBoolValue(true), (*Evaluator).evaluateAnd, NewBoolValue(true), true},
		{"and false left", NewBoolValue(false), (*Evaluator).evaluateAnd, NewBoolValue(true), false},
		{"and false right", NewBoolValue(true), (*Evaluator).evaluateAnd, NewBoolValue(false), false},
		{"and false both", NewBoolValue(false), (*Evaluator).evaluateAnd, NewBoolValue(false), false},
		{"or true left", NewBoolValue(true), (*Evaluator).evaluateOr, NewBoolValue(false), true},
		{"or true right", NewBoolValue(false), (*Evaluator).evaluateOr, NewBoolValue(true), true},
		{"or true both", NewBoolValue(true), (*Evaluator).evaluateOr, NewBoolValue(true), true},
		{"or false", NewBoolValue(false), (*Evaluator).evaluateOr, NewBoolValue(false), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eval := &Evaluator{}
			result := tt.evalFunc(eval, tt.left, tt.right)

			if result.IsError() {
				t.Fatalf("Logical operation returned error: %v", result)
			}

			b, err := result.BoolValue()
			if err != nil {
				t.Fatal("Logical operation did not return boolean")
			}

			if b != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, b)
			}
		})
	}
}

func TestUnaryOperations(t *testing.T) {
	// Test through high-level Parse and Evaluate since unary operations work on AST nodes
	tests := []struct {
		name      string
		classad   string
		attr      string
		checkFunc func(*testing.T, Value)
	}{
		{
			"negate int",
			"[x = 5; y = -x]",
			"y",
			func(t *testing.T, v Value) {
				val, _ := v.IntValue()
				if val != -5 {
					t.Errorf("Expected -5, got %d", val)
				}
			},
		},
		{
			"plus int",
			"[x = 5; y = +x]",
			"y",
			func(t *testing.T, v Value) {
				val, _ := v.IntValue()
				if val != 5 {
					t.Errorf("Expected 5, got %d", val)
				}
			},
		},
		{
			"not true",
			"[x = true; y = !x]",
			"y",
			func(t *testing.T, v Value) {
				val, _ := v.BoolValue()
				if val != false {
					t.Errorf("Expected false, got %v", val)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.classad)
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}

			result := ad.EvaluateAttr(tt.attr)
			if result.IsError() {
				t.Fatalf("Evaluation returned error: %v", result)
			}

			tt.checkFunc(t, result)
		})
	}
}

func TestTypePromotion(t *testing.T) {
	// Test that integer + real promotes to real
	eval := &Evaluator{}
	result := eval.evaluateAdd(NewIntValue(2), NewRealValue(3.5))

	if !result.IsReal() {
		t.Errorf("Expected real value, got %v", result.Type())
	}

	r, err := result.RealValue()
	if err != nil {
		t.Fatal("Failed to get real value")
	}

	if r != 5.5 {
		t.Errorf("Expected 5.5, got %g", r)
	}
}

func TestErrorPropagation(t *testing.T) {
	eval := &Evaluator{}

	// Error in binary operation
	result := eval.evaluateAdd(NewErrorValue(), NewIntValue(5))
	if !result.IsError() {
		t.Error("Expected error propagation in binary op")
	}

	// Undefined in binary operation
	result = eval.evaluateAdd(NewUndefinedValue(), NewIntValue(5))
	if !result.IsError() && !result.IsUndefined() {
		t.Error("Expected error/undefined propagation")
	}
}
