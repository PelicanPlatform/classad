package classad

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

// Tests for ClassAd methods with 0% coverage

func TestGetParent(t *testing.T) {
	ad := New()
	parent := New()

	// Initially no parent
	if ad.GetParent() != nil {
		t.Error("Expected nil parent initially")
	}

	// Set parent
	ad.SetParent(parent)
	if ad.GetParent() != parent {
		t.Error("Expected GetParent to return the set parent")
	}
}

func TestSetTarget(t *testing.T) {
	ad := New()
	target := New()

	// Initially no target
	if ad.GetTarget() != nil {
		t.Error("Expected nil target initially")
	}

	// Set target
	ad.SetTarget(target)
	if ad.GetTarget() != target {
		t.Error("Expected GetTarget to return the set target")
	}
}

func TestEvaluateExpr(t *testing.T) {
	ad := New()
	ad.InsertAttr("x", 10)
	ad.InsertAttr("y", 20)

	// Create an expression: x + y
	expr := &ast.BinaryOp{
		Op:    "+",
		Left:  &ast.AttributeReference{Name: "x"},
		Right: &ast.AttributeReference{Name: "y"},
	}

	result := ad.EvaluateExpr(expr)
	if !result.IsInteger() {
		t.Fatalf("Expected integer result")
	}

	intVal, _ := result.IntValue()
	if intVal != 30 {
		t.Errorf("Expected 30, got %d", intVal)
	}
}

func TestEvaluateExprString(t *testing.T) {
	ad := New()
	ad.InsertAttr("x", 10)
	ad.InsertAttr("y", 20)

	tests := []struct {
		name     string
		exprStr  string
		expected int64
		wantErr  bool
	}{
		{
			name:     "simple addition",
			exprStr:  "[__tmp__ = x + y]",
			expected: 30,
			wantErr:  false,
		},
		{
			name:     "multiplication",
			exprStr:  "[__tmp__ = x * y]",
			expected: 200,
			wantErr:  false,
		},
		{
			name:    "invalid expression",
			exprStr: "[__tmp__ = x +]",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ad.EvaluateExprString(tt.exprStr)
			if tt.wantErr {
				if err == nil {
					t.Error("Expected error but got none")
				}
				return
			}

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if !result.IsInteger() {
				t.Fatal("Expected integer result")
			}

			intVal, _ := result.IntValue()
			if intVal != tt.expected {
				t.Errorf("Expected %d, got %d", tt.expected, intVal)
			}
		})
	}
}

// Tests for MatchClassAd methods with 0% coverage

func TestMatchClassAdReplaceRightAd(t *testing.T) {
	left := New()
	left.InsertAttr("x", 10)

	right := New()
	right.InsertAttr("y", 20)

	match := NewMatchClassAd(left, right)

	// Replace right ad
	newRight := New()
	newRight.InsertAttr("z", 30)

	match.ReplaceRightAd(newRight)

	// Verify the replacement
	result := match.GetRightAd()
	if result != newRight {
		t.Error("Expected newRight ad")
	}

	// Test with nil
	match.ReplaceRightAd(nil)
	if match.GetRightAd() != nil {
		t.Error("Expected nil right ad")
	}
}

func TestMatchClassAdEvaluateExprLeft(t *testing.T) {
	left := New()
	left.InsertAttr("x", 10)

	right := New()
	right.InsertAttr("y", 20)

	match := NewMatchClassAd(left, right)

	// Create expression referencing left
	expr := &ast.AttributeReference{Name: "x"}
	result := match.EvaluateExprLeft(expr)

	if !result.IsInteger() {
		t.Fatal("Expected integer result")
	}

	intVal, _ := result.IntValue()
	if intVal != 10 {
		t.Errorf("Expected 10, got %d", intVal)
	}

	// Test with nil left
	match2 := NewMatchClassAd(nil, right)
	result2 := match2.EvaluateExprLeft(expr)
	if !result2.IsUndefined() {
		t.Error("Expected undefined result with nil left")
	}
}

func TestMatchClassAdEvaluateExprRight(t *testing.T) {
	left := New()
	left.InsertAttr("x", 10)

	right := New()
	right.InsertAttr("y", 20)

	match := NewMatchClassAd(left, right)

	// Create expression referencing right
	expr := &ast.AttributeReference{Name: "y"}
	result := match.EvaluateExprRight(expr)

	if !result.IsInteger() {
		t.Fatal("Expected integer result")
	}

	intVal, _ := result.IntValue()
	if intVal != 20 {
		t.Errorf("Expected 20, got %d", intVal)
	}

	// Test with nil right
	match2 := NewMatchClassAd(left, nil)
	result2 := match2.EvaluateExprRight(expr)
	if !result2.IsUndefined() {
		t.Error("Expected undefined result with nil right")
	}
}

// Tests for struct.go functions with 0% coverage

func TestMarshalClassAd(t *testing.T) {
	type TestStruct struct {
		Name  string
		Value int
	}

	s := TestStruct{
		Name:  "test",
		Value: 42,
	}

	result, err := MarshalClassAd(s)
	if err != nil {
		t.Fatalf("MarshalClassAd failed: %v", err)
	}

	if result == "" {
		t.Error("Expected non-empty result")
	}
}

func TestUnmarshalClassAd(t *testing.T) {
	type TestStruct struct {
		Name  string
		Value int
	}

	data := `[Name = "test"; Value = 42]`

	var result TestStruct
	err := UnmarshalClassAd(data, &result)
	if err != nil {
		t.Fatalf("UnmarshalClassAd failed: %v", err)
	}

	if result.Name != "test" {
		t.Errorf("Expected Name='test', got %q", result.Name)
	}

	if result.Value != 42 {
		t.Errorf("Expected Value=42, got %d", result.Value)
	}
}

// Tests for low-coverage functions in functions.go

func TestBuiltinToLowerError(t *testing.T) {
	ad := New()

	// Test with non-string argument (should return error)
	ad.InsertAttr("num", 123)
	expr, _ := ParseExpr("toLower(num)")
	result := expr.Eval(ad)

	if !result.IsError() {
		t.Error("Expected error for non-string argument")
	}
}

func TestBuiltinToUpperError(t *testing.T) {
	ad := New()

	// Test with non-string argument (should return error)
	ad.InsertAttr("num", 123)
	expr, _ := ParseExpr("toUpper(num)")
	result := expr.Eval(ad)

	if !result.IsError() {
		t.Error("Expected error for non-string argument")
	}
}

func TestBuiltinFloorError(t *testing.T) {
	ad := New()

	// Test with non-numeric argument
	ad.InsertAttrString("str", "hello")
	expr, _ := ParseExpr("floor(str)")
	result := expr.Eval(ad)

	if !result.IsError() {
		t.Error("Expected error for non-numeric argument")
	}
}

func TestBuiltinCeilingError(t *testing.T) {
	ad := New()

	// Test with non-numeric argument
	ad.InsertAttrString("str", "hello")
	expr, _ := ParseExpr("ceiling(str)")
	result := expr.Eval(ad)

	if !result.IsError() {
		t.Error("Expected error for non-numeric argument")
	}
}

func TestBuiltinRoundError(t *testing.T) {
	ad := New()

	// Test with non-numeric argument
	ad.InsertAttrString("str", "hello")
	expr, _ := ParseExpr("round(str)")
	result := expr.Eval(ad)

	if !result.IsError() {
		t.Error("Expected error for non-numeric argument")
	}
}

func TestBuiltinIntEdgeCases(t *testing.T) {
	ad := New()

	tests := []struct {
		name     string
		input    interface{}
		expected int64
		isError  bool
	}{
		{"bool to int true", true, 1, false},
		{"bool to int false", false, 0, false},
		{"float to int", 3.7, 3, false},
		{"int to int", int64(42), 42, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad.Clear()
			switch v := tt.input.(type) {
			case bool:
				ad.InsertAttrBool("val", v)
			case float64:
				ad.InsertAttrFloat("val", v)
			case int64:
				ad.InsertAttr("val", v)
			}

			expr, _ := ParseExpr("int(val)")
			result := expr.Eval(ad)

			if tt.isError {
				if !result.IsError() {
					t.Error("Expected error result")
				}
			} else {
				if result.IsError() {
					t.Error("Unexpected error result")
				}
				if result.IsInteger() {
					intVal, _ := result.IntValue()
					if intVal != tt.expected {
						t.Errorf("Expected %d, got %d", tt.expected, intVal)
					}
				}
			}
		})
	}
}

func TestBuiltinRealEdgeCases(t *testing.T) {
	ad := New()

	tests := []struct {
		name     string
		input    interface{}
		expected float64
		isError  bool
	}{
		{"int to real", int64(42), 42.0, false},
		{"float to real", 3.14, 3.14, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad.Clear()
			switch v := tt.input.(type) {
			case int64:
				ad.InsertAttr("val", v)
			case float64:
				ad.InsertAttrFloat("val", v)
			}

			expr, _ := ParseExpr("real(val)")
			result := expr.Eval(ad)

			if tt.isError {
				if !result.IsError() {
					t.Error("Expected error result")
				}
			} else {
				if result.IsError() {
					t.Error("Unexpected error result")
				}
				if result.IsReal() {
					realVal, _ := result.RealValue()
					if realVal != tt.expected {
						t.Errorf("Expected %f, got %f", tt.expected, realVal)
					}
				}
			}
		})
	}
}

func TestBuiltinMemberEdgeCases(t *testing.T) {
	ad := New()

	// Test with non-list second argument
	ad.InsertAttr("val", 5)
	expr, _ := ParseExpr("member(5, val)")
	result := expr.Eval(ad)

	if !result.IsError() {
		t.Error("Expected error when second argument is not a list")
	}

	// Test with empty list
	InsertAttrList(ad, "emptyList", []int64{})
	expr2, _ := ParseExpr("member(5, emptyList)")
	result2 := expr2.Eval(ad)

	if !result2.IsBool() {
		t.Error("Expected boolean result")
	} else {
		boolVal, _ := result2.BoolValue()
		if boolVal {
			t.Error("Expected false for member in empty list")
		}
	}
}

func TestBuiltinStrcmpEdgeCases(t *testing.T) {
	ad := New()

	tests := []struct {
		name     string
		str1     string
		str2     string
		expected int64
	}{
		{"equal strings", "hello", "hello", 0},
		{"first less", "abc", "xyz", -1},
		{"first greater", "xyz", "abc", 1},
		{"case sensitive", "Hello", "hello", -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad.Clear()
			ad.InsertAttrString("s1", tt.str1)
			ad.InsertAttrString("s2", tt.str2)

			expr, _ := ParseExpr("strcmp(s1, s2)")
			result := expr.Eval(ad)

			if !result.IsInteger() {
				t.Fatal("Expected integer result")
			}

			intVal, _ := result.IntValue()
			if intVal != tt.expected {
				t.Errorf("Expected %d, got %d", tt.expected, intVal)
			}
		})
	}

	// Test error cases - boolean type
	ad.Clear()
	ad.InsertAttrBool("bool", true)
	expr, _ := ParseExpr("strcmp(bool, \"hello\")")
	result := expr.Eval(ad)

	if !result.IsError() {
		t.Error("Expected error for boolean argument")
	}
}

func TestBuiltinStricmpEdgeCases(t *testing.T) {
	ad := New()

	tests := []struct {
		name     string
		str1     string
		str2     string
		expected int64
	}{
		{"equal strings", "hello", "HELLO", 0},
		{"first less", "abc", "XYZ", -1},
		{"first greater", "xyz", "ABC", 1},
		{"mixed case", "HeLLo", "hello", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad.Clear()
			ad.InsertAttrString("s1", tt.str1)
			ad.InsertAttrString("s2", tt.str2)

			expr, _ := ParseExpr("stricmp(s1, s2)")
			result := expr.Eval(ad)

			if !result.IsInteger() {
				t.Fatal("Expected integer result")
			}

			intVal, _ := result.IntValue()
			if intVal != tt.expected {
				t.Errorf("Expected %d, got %d", tt.expected, intVal)
			}
		})
	}

	// Test error cases - boolean type
	ad.Clear()
	ad.InsertAttrBool("bool", true)
	expr, _ := ParseExpr("stricmp(bool, \"hello\")")
	result := expr.Eval(ad)

	if !result.IsError() {
		t.Error("Expected error for boolean argument")
	}
}

func TestBuiltinJoinEdgeCases(t *testing.T) {
	ad := New()

	// Test with empty list
	InsertAttrList(ad, "emptyList", []string{})
	expr, _ := ParseExpr("join(\",\", emptyList)")
	result := expr.Eval(ad)

	if !result.IsString() {
		t.Fatal("Expected string result")
	}

	strVal, _ := result.StringValue()
	if strVal != "" {
		t.Errorf("Expected empty string, got %q", strVal)
	}

	// Test with non-list argument (multiple args form)
	ad.InsertAttr("notList", 123)
	expr2, _ := ParseExpr("join(\",\", notList)")
	result2 := expr2.Eval(ad)

	// With multiple args, it returns the joined values
	if !result2.IsString() {
		t.Fatal("Expected string result")
	}

	// Test with non-string separator
	InsertAttrList(ad, "list", []string{"a", "b", "c"})
	expr3, _ := ParseExpr("join(123, list)")
	result3 := expr3.Eval(ad)

	if !result3.IsError() {
		t.Error("Expected error for non-string separator")
	}
}

func TestBuiltinIdenticalMemberEdgeCases(t *testing.T) {
	ad := New()

	// Test with different types (should be false)
	InsertAttrList(ad, "list", []int64{1, 2, 3})
	ad.InsertAttrString("str", "1")

	expr, _ := ParseExpr("identicalMember(str, list)")
	result := expr.Eval(ad)

	if !result.IsBool() {
		t.Fatal("Expected boolean result")
	}

	boolVal, _ := result.BoolValue()
	if boolVal {
		t.Error("Expected false for different types")
	}

	// Test with matching type
	ad.InsertAttr("num", 2)
	expr2, _ := ParseExpr("identicalMember(num, list)")
	result2 := expr2.Eval(ad)

	if !result2.IsBool() {
		t.Fatal("Expected boolean result")
	}

	boolVal2, _ := result2.BoolValue()
	if !boolVal2 {
		t.Error("Expected true for matching value")
	}
}

func TestBuiltinRegexpMember(t *testing.T) {
	ad := New()

	// Test with list of strings
	InsertAttrList(ad, "list", []string{"hello", "world", "test"})
	ad.InsertAttrString("pattern", "^he.*")

	expr, _ := ParseExpr("regexpMember(pattern, list)")
	result := expr.Eval(ad)

	if !result.IsBool() {
		t.Fatal("Expected boolean result")
	}

	boolVal, _ := result.BoolValue()
	if !boolVal {
		t.Error("Expected true for matching pattern")
	}

	// Test with non-matching pattern
	ad.InsertAttrString("pattern2", "^xyz")
	expr2, _ := ParseExpr("regexpMember(pattern2, list)")
	result2 := expr2.Eval(ad)

	if !result2.IsBool() {
		t.Fatal("Expected boolean result")
	}

	boolVal2, _ := result2.BoolValue()
	if boolVal2 {
		t.Error("Expected false for non-matching pattern")
	}

	// Test error case - invalid pattern
	ad.InsertAttrString("badPattern", "[")
	expr3, _ := ParseExpr("regexpMember(badPattern, list)")
	result3 := expr3.Eval(ad)

	if !result3.IsError() {
		t.Error("Expected error for invalid pattern")
	}
}

func TestBuiltinReplace(t *testing.T) {
	ad := New()

	tests := []struct {
		name     string
		pattern  string
		target   string
		replace  string
		expected string
	}{
		{"simple replace", "world", "hello world", "Go", "hello Go"},
		{"multiple occurrences", "foo", "foo bar foo", "baz", "baz bar foo"}, // Replace only replaces first
		{"no match", "xyz", "hello", "abc", "hello"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad.Clear()
			ad.InsertAttrString("pattern", tt.pattern)
			ad.InsertAttrString("target", tt.target)
			ad.InsertAttrString("replace", tt.replace)

			expr, _ := ParseExpr("replace(pattern, target, replace)")
			result := expr.Eval(ad)

			if !result.IsString() {
				t.Fatal("Expected string result")
			}

			strVal, _ := result.StringValue()
			if strVal != tt.expected {
				t.Errorf("Expected %q, got %q", tt.expected, strVal)
			}
		})
	}

	// Test error case - non-string arguments
	ad.Clear()
	ad.InsertAttr("num", 123)
	expr, _ := ParseExpr("replace(num, \"x\", \"y\")")
	result := expr.Eval(ad)

	if !result.IsError() {
		t.Error("Expected error for non-string argument")
	}
}
