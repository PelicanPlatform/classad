package classad

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

// Helper function to compare values
func elvisValuesEqual(a, b Value) bool {
	if a.IsUndefined() && b.IsUndefined() {
		return true
	}
	if a.IsError() && b.IsError() {
		return true
	}
	if a.IsInteger() && b.IsInteger() {
		aVal, _ := a.IntValue()
		bVal, _ := b.IntValue()
		return aVal == bVal
	}
	if a.IsReal() && b.IsReal() {
		aVal, _ := a.RealValue()
		bVal, _ := b.RealValue()
		return aVal == bVal
	}
	if a.IsBool() && b.IsBool() {
		aVal, _ := a.BoolValue()
		bVal, _ := b.BoolValue()
		return aVal == bVal
	}
	if a.IsString() && b.IsString() {
		aVal, _ := a.StringValue()
		bVal, _ := b.StringValue()
		return aVal == bVal
	}
	return false
}

// TestElvisOperatorParsing tests that the Elvis operator (?:) can be parsed correctly
func TestElvisOperatorParsing(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name:    "basic elvis with attribute",
			input:   "foo ?: 3",
			wantErr: false,
		},
		{
			name:    "elvis with undefined literal",
			input:   "undefined ?: 42",
			wantErr: false,
		},
		{
			name:    "elvis with complex left side",
			input:   "(x + y) ?: 10",
			wantErr: false,
		},
		{
			name:    "elvis with complex right side",
			input:   "x ?: (y * 2)",
			wantErr: false,
		},
		{
			name:    "nested elvis operators",
			input:   "a ?: b ?: c",
			wantErr: false,
		},
		{
			name:    "elvis with string",
			input:   `Name ?: "Unknown"`,
			wantErr: false,
		},
		{
			name:    "elvis with boolean",
			input:   "Flag ?: true",
			wantErr: false,
		},
		{
			name:    "elvis in arithmetic expression",
			input:   "(x ?: 5) + 10",
			wantErr: false,
		},
		{
			name:    "elvis in comparison",
			input:   "(Memory ?: 1024) > 512",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr, err := ParseExpr(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseExpr() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err == nil {
				if expr == nil {
					t.Error("ParseExpr() returned nil expression")
				}
			}
		})
	}
}

// TestElvisOperatorAST tests that Elvis operator creates correct AST nodes
func TestElvisOperatorAST(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		checkFunc func(*testing.T, *Expr)
	}{
		{
			name:  "simple elvis creates ElvisExpr node",
			input: "x ?: 5",
			checkFunc: func(t *testing.T, expr *Expr) {
				elvis, ok := expr.expr.(*ast.ElvisExpr)
				if !ok {
					t.Errorf("Expected *ast.ElvisExpr, got %T", expr.expr)
					return
				}
				if elvis.Left == nil {
					t.Error("Elvis Left expression is nil")
				}
				if elvis.Right == nil {
					t.Error("Elvis Right expression is nil")
				}
			},
		},
		{
			name:  "nested elvis",
			input: "a ?: b ?: c",
			checkFunc: func(t *testing.T, expr *Expr) {
				// The outer expression should be ElvisExpr
				outerElvis, ok := expr.expr.(*ast.ElvisExpr)
				if !ok {
					t.Errorf("Expected outer *ast.ElvisExpr, got %T", expr.expr)
					return
				}
				// The right side should also be ElvisExpr (right associative)
				innerElvis, ok := outerElvis.Right.(*ast.ElvisExpr)
				if !ok {
					t.Errorf("Expected nested *ast.ElvisExpr on right, got %T", outerElvis.Right)
				}
				_ = innerElvis // Verify it exists
			},
		},
		{
			name:  "elvis string representation",
			input: "foo ?: 3",
			checkFunc: func(t *testing.T, expr *Expr) {
				str := expr.String()
				// The string representation should contain the Elvis operator
				if str == "" {
					t.Error("String() returned empty string")
				}
				// Should contain ?: somewhere in the output
				// Note: Exact format depends on implementation
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr, err := ParseExpr(tt.input)
			if err != nil {
				t.Fatalf("ParseExpr() error = %v", err)
			}
			tt.checkFunc(t, expr)
		})
	}
}

// TestElvisOperatorEvaluation tests Elvis operator evaluation semantics
func TestElvisOperatorEvaluation(t *testing.T) {
	tests := []struct {
		name     string
		adStr    string
		exprStr  string
		expected Value
	}{
		{
			name:     "undefined attribute returns fallback",
			adStr:    "[]",
			exprStr:  "foo ?: 3",
			expected: NewIntValue(3),
		},
		{
			name:     "defined attribute returns attribute",
			adStr:    "[foo = 5]",
			exprStr:  "foo ?: 3",
			expected: NewIntValue(5),
		},
		{
			name:     "undefined literal returns fallback",
			adStr:    "[]",
			exprStr:  "undefined ?: 42",
			expected: NewIntValue(42),
		},
		{
			name:     "zero value is not undefined",
			adStr:    "[x = 0]",
			exprStr:  "x ?: 10",
			expected: NewIntValue(0),
		},
		{
			name:     "false value is not undefined",
			adStr:    "[flag = false]",
			exprStr:  "flag ?: true",
			expected: NewBoolValue(false),
		},
		{
			name:     "empty string is not undefined",
			adStr:    `[name = ""]`,
			exprStr:  `name ?: "Unknown"`,
			expected: NewStringValue(""),
		},
		{
			name:     "string fallback",
			adStr:    "[]",
			exprStr:  `Name ?: "Unknown"`,
			expected: NewStringValue("Unknown"),
		},
		{
			name:     "expression on left side",
			adStr:    "[x = 5]",
			exprStr:  "(x + 10) ?: 100",
			expected: NewIntValue(15),
		},
		{
			name:     "undefined expression on left side",
			adStr:    "[]",
			exprStr:  "(foo + 10) ?: 100",
			expected: NewIntValue(100),
		},
		{
			name:     "nested elvis - first defined",
			adStr:    "[a = 1]",
			exprStr:  "a ?: b ?: c",
			expected: NewIntValue(1),
		},
		{
			name:     "nested elvis - second defined",
			adStr:    "[b = 2]",
			exprStr:  "a ?: b ?: c",
			expected: NewIntValue(2),
		},
		{
			name:     "nested elvis - use fallback",
			adStr:    "[c = 3]",
			exprStr:  "a ?: b ?: c",
			expected: NewIntValue(3),
		},
		{
			name:     "elvis in arithmetic",
			adStr:    "[x = 5]",
			exprStr:  "(x ?: 10) + 3",
			expected: NewIntValue(8),
		},
		{
			name:     "elvis in arithmetic with undefined",
			adStr:    "[]",
			exprStr:  "(x ?: 10) + 3",
			expected: NewIntValue(13),
		},
		{
			name:     "elvis with boolean",
			adStr:    "[ready = true]",
			exprStr:  "ready ?: false",
			expected: NewBoolValue(true),
		},
		{
			name:     "real number",
			adStr:    "[pi = 3.14]",
			exprStr:  "pi ?: 3.0",
			expected: NewRealValue(3.14),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.adStr)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}

			expr, err := ParseExpr(tt.exprStr)
			if err != nil {
				t.Fatalf("ParseExpr() error = %v", err)
			}

			result := expr.Eval(ad)
			if !elvisValuesEqual(result, tt.expected) {
				t.Errorf("Evaluate() = %v, want %v", result, tt.expected)
			}
		})
	}
}

// TestElvisOperatorWithErrors tests that errors are propagated correctly
func TestElvisOperatorWithErrors(t *testing.T) {
	tests := []struct {
		name      string
		adStr     string
		exprStr   string
		wantErr   bool
		wantUndef bool
	}{
		{
			name:      "error on left side returns error (not fallback)",
			adStr:     "[]",
			exprStr:   "error ?: 3",
			wantErr:   true,
			wantUndef: false,
		},
		{
			name:      "error in expression on left side",
			adStr:     "[]",
			exprStr:   `(1 / 0) ?: 42`,
			wantErr:   true,
			wantUndef: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.adStr)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}

			expr, err := ParseExpr(tt.exprStr)
			if err != nil {
				t.Fatalf("ParseExpr() error = %v", err)
			}

			result := expr.Eval(ad)
			if tt.wantErr && !result.IsError() {
				t.Errorf("Expected error value, got %v", result)
			}
			if tt.wantUndef && !result.IsUndefined() {
				t.Errorf("Expected undefined value, got %v", result)
			}
		})
	}
}

// TestElvisVsTernary tests the difference between Elvis and ternary operators
func TestElvisVsTernary(t *testing.T) {
	tests := []struct {
		name         string
		adStr        string
		elvisExpr    string
		ternaryExpr  string
		shouldBeSame bool
	}{
		{
			name:         "Elvis vs equivalent ternary with undefined",
			adStr:        "[]",
			elvisExpr:    "foo ?: 3",
			ternaryExpr:  "isUndefined(foo) ? 3 : foo",
			shouldBeSame: true,
		},
		{
			name:         "Elvis vs equivalent ternary with defined",
			adStr:        "[foo = 7]",
			elvisExpr:    "foo ?: 3",
			ternaryExpr:  "isUndefined(foo) ? 3 : foo",
			shouldBeSame: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.adStr)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}

			elvisExpr, err := ParseExpr(tt.elvisExpr)
			if err != nil {
				t.Fatalf("ParseExpr(elvis) error = %v", err)
			}

			ternaryExpr, err := ParseExpr(tt.ternaryExpr)
			if err != nil {
				t.Fatalf("ParseExpr(ternary) error = %v", err)
			}

			elvisResult := elvisExpr.Eval(ad)
			ternaryResult := ternaryExpr.Eval(ad)

			if tt.shouldBeSame && !elvisValuesEqual(elvisResult, ternaryResult) {
				t.Errorf("Elvis result %v != ternary result %v", elvisResult, ternaryResult)
			}
		})
	}
}

// TestElvisOperatorIntrospection tests that Elvis operator works with introspection
func TestElvisOperatorIntrospection(t *testing.T) {
	tests := []struct {
		name         string
		exprStr      string
		expectedRefs []string
	}{
		{
			name:         "simple elvis collects refs from both sides",
			exprStr:      "foo ?: bar",
			expectedRefs: []string{"foo", "bar"},
		},
		{
			name:         "elvis with expression collects nested refs",
			exprStr:      "(x + y) ?: z",
			expectedRefs: []string{"x", "y", "z"},
		},
		{
			name:         "nested elvis collects all refs",
			exprStr:      "a ?: b ?: c",
			expectedRefs: []string{"a", "b", "c"},
		},
		{
			name:         "elvis with literals only collects attribute refs",
			exprStr:      "foo ?: 42",
			expectedRefs: []string{"foo"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr, err := ParseExpr(tt.exprStr)
			if err != nil {
				t.Fatalf("ParseExpr() error = %v", err)
			}

			ad := New()
			refs := ad.ExternalRefs(expr)

			// Check that all expected refs are present
			refMap := make(map[string]bool)
			for _, ref := range refs {
				refMap[ref] = true
			}

			for _, expected := range tt.expectedRefs {
				if !refMap[expected] {
					t.Errorf("Expected reference %q not found in %v", expected, refs)
				}
			}
		})
	}
}

// TestElvisOperatorFlattening tests that Elvis operator works with flattening
func TestElvisOperatorFlattening(t *testing.T) {
	tests := []struct {
		name           string
		adStr          string
		exprStr        string
		expectedFlat   string
		shouldEvaluate bool
	}{
		{
			name:           "flatten elvis with defined left side",
			adStr:          "[x = 5]",
			exprStr:        "x ?: 10",
			expectedFlat:   "5",
			shouldEvaluate: true,
		},
		{
			name:           "flatten elvis with undefined left side",
			adStr:          "[]",
			exprStr:        "x ?: 10",
			expectedFlat:   "10",
			shouldEvaluate: true,
		},
		{
			name:           "flatten elvis with undefined literal left",
			adStr:          "[]",
			exprStr:        "undefined ?: 42",
			expectedFlat:   "42",
			shouldEvaluate: true,
		},
		{
			name:           "flatten nested elvis",
			adStr:          "[b = 2]",
			exprStr:        "a ?: b ?: c",
			expectedFlat:   "2",
			shouldEvaluate: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.adStr)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}

			expr, err := ParseExpr(tt.exprStr)
			if err != nil {
				t.Fatalf("ParseExpr() error = %v", err)
			}

			flattened := ad.Flatten(expr)
			if flattened == nil {
				t.Fatal("Flatten() returned nil")
			}

			// If it should fully evaluate, check the result
			if tt.shouldEvaluate {
				result := flattened.Eval(ad)
				resultStr := result.String()
				if resultStr != tt.expectedFlat {
					t.Errorf("Flattened expression evaluated to %s, want %s", resultStr, tt.expectedFlat)
				}
			}
		})
	}
}

// TestElvisOperatorHTCondorExample tests the example from HTCondor documentation
func TestElvisOperatorHTCondorExample(t *testing.T) {
	// From HTCondor docs: "foo ?: 3" evaluates to 3 if foo is undefined
	ad, err := Parse("[]")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	expr, err := ParseExpr("foo ?: 3")
	if err != nil {
		t.Fatalf("ParseExpr() error = %v", err)
	}

	result := expr.Eval(ad)
	expected := NewIntValue(3)
	if !elvisValuesEqual(result, expected) {
		t.Errorf("HTCondor example: got %v, want %v", result, expected)
	}

	// Now test with foo defined
	ad2, err := Parse("[foo = 7]")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	result2 := expr.Eval(ad2)
	expected2 := NewIntValue(7)
	if !elvisValuesEqual(result2, expected2) {
		t.Errorf("HTCondor example with foo defined: got %v, want %v", result2, expected2)
	}
}
