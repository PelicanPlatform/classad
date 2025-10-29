package classad

import (
	"testing"
)

// TestParseExpr tests parsing expressions directly
func TestParseExpr(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name:    "simple arithmetic",
			input:   "2 + 3",
			wantErr: false,
		},
		{
			name:    "with variables",
			input:   "Cpus * 2",
			wantErr: false,
		},
		{
			name:    "complex expression",
			input:   "(Memory / 1024) + Disk",
			wantErr: false,
		},
		{
			name:    "boolean expression",
			input:   "Cpus >= 4 && Memory >= 8192",
			wantErr: false,
		},
		{
			name:    "with MY scope",
			input:   "MY.Cpus > 2",
			wantErr: false,
		},
		{
			name:    "with TARGET scope",
			input:   "TARGET.Memory >= 1024",
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
				// Just verify we got a valid expression
				if expr == nil {
					t.Error("ParseExpr() returned nil expression")
				}
				// Verify String() works
				s := expr.String()
				if s == "" {
					t.Error("expr.String() returned empty string")
				}
			}
		})
	}
}

// TestExprEval tests evaluating expressions in a ClassAd context
func TestExprEval(t *testing.T) {
	ad := New()
	ad.InsertAttr("Cpus", 8)
	ad.InsertAttr("Memory", 16384)

	tests := []struct {
		name     string
		exprStr  string
		expected Value
	}{
		{
			name:     "simple reference",
			exprStr:  "Cpus",
			expected: NewIntValue(8),
		},
		{
			name:     "arithmetic",
			exprStr:  "Cpus * 2",
			expected: NewIntValue(16),
		},
		{
			name:     "division",
			exprStr:  "Memory / 1024",
			expected: NewIntValue(16),
		},
		{
			name:     "comparison",
			exprStr:  "Cpus >= 4",
			expected: NewBoolValue(true),
		},
		{
			name:     "complex boolean",
			exprStr:  "Cpus >= 4 && Memory >= 8192",
			expected: NewBoolValue(true),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr, err := ParseExpr(tt.exprStr)
			if err != nil {
				t.Fatalf("ParseExpr() error = %v", err)
			}

			result := expr.Eval(ad)
			if result.Type() != tt.expected.Type() {
				t.Errorf("Type mismatch: got %v, want %v", result.Type(), tt.expected.Type())
			}

			// Compare values based on type
			switch tt.expected.Type() {
			case IntegerValue:
				gotInt, _ := result.IntValue()
				wantInt, _ := tt.expected.IntValue()
				if gotInt != wantInt {
					t.Errorf("Value mismatch: got %d, want %d", gotInt, wantInt)
				}
			case BooleanValue:
				gotBool, _ := result.BoolValue()
				wantBool, _ := tt.expected.BoolValue()
				if gotBool != wantBool {
					t.Errorf("Value mismatch: got %v, want %v", gotBool, wantBool)
				}
			}
		})
	}
}

// TestExprEvalWithContext tests evaluating expressions with MY and TARGET contexts
func TestExprEvalWithContext(t *testing.T) {
	job := New()
	job.InsertAttr("RequestCpus", 4)
	job.InsertAttr("RequestMemory", 8192)

	machine := New()
	machine.InsertAttr("Cpus", 8)
	machine.InsertAttr("Memory", 16384)

	tests := []struct {
		name     string
		exprStr  string
		scope    *ClassAd
		target   *ClassAd
		expected Value
	}{
		{
			name:     "MY scope reference",
			exprStr:  "MY.RequestCpus",
			scope:    job,
			target:   machine,
			expected: NewIntValue(4),
		},
		{
			name:     "TARGET scope reference",
			exprStr:  "TARGET.Cpus",
			scope:    job,
			target:   machine,
			expected: NewIntValue(8),
		},
		{
			name:     "comparison with both scopes",
			exprStr:  "TARGET.Cpus >= MY.RequestCpus",
			scope:    job,
			target:   machine,
			expected: NewBoolValue(true),
		},
		{
			name:     "complex expression",
			exprStr:  "(TARGET.Memory >= MY.RequestMemory) && (TARGET.Cpus >= MY.RequestCpus)",
			scope:    job,
			target:   machine,
			expected: NewBoolValue(true),
		},
		{
			name:     "reversed scopes",
			exprStr:  "TARGET.RequestCpus <= MY.Cpus",
			scope:    machine,
			target:   job,
			expected: NewBoolValue(true),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr, err := ParseExpr(tt.exprStr)
			if err != nil {
				t.Fatalf("ParseExpr() error = %v", err)
			}

			result := expr.EvalWithContext(tt.scope, tt.target)
			if result.Type() != tt.expected.Type() {
				t.Errorf("Type mismatch: got %v, want %v", result.Type(), tt.expected.Type())
			}

			// Compare values based on type
			switch tt.expected.Type() {
			case IntegerValue:
				gotInt, _ := result.IntValue()
				wantInt, _ := tt.expected.IntValue()
				if gotInt != wantInt {
					t.Errorf("Value mismatch: got %d, want %d", gotInt, wantInt)
				}
			case BooleanValue:
				gotBool, _ := result.BoolValue()
				wantBool, _ := tt.expected.BoolValue()
				if gotBool != wantBool {
					t.Errorf("Value mismatch: got %v, want %v", gotBool, wantBool)
				}
			}
		})
	}
}

// TestLookupAndInsertExpr tests copying expressions between ClassAds
func TestLookupAndInsertExpr(t *testing.T) {
	// Create source ClassAd with expressions
	source, err := Parse("[Formula = Cpus * 2; Condition = Memory >= 8192]")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	// Create target ClassAd with base values
	target := New()
	target.InsertAttr("Cpus", 4)
	target.InsertAttr("Memory", 16384)

	// Copy Formula expression
	if formula, ok := source.Lookup("Formula"); ok {
		target.InsertExpr("ComputedValue", formula)
	} else {
		t.Fatal("Failed to lookup Formula")
	}

	// Copy Condition expression
	if condition, ok := source.Lookup("Condition"); ok {
		target.InsertExpr("Check", condition)
	} else {
		t.Fatal("Failed to lookup Condition")
	}

	// Evaluate copied expressions in target context
	value, ok := target.EvaluateAttrInt("ComputedValue")
	if !ok {
		t.Fatal("Failed to evaluate ComputedValue")
	}
	if value != 8 {
		t.Errorf("Expected ComputedValue = 8, got %d", value)
	}

	check, ok := target.EvaluateAttrBool("Check")
	if !ok {
		t.Fatal("Failed to evaluate Check")
	}
	if !check {
		t.Error("Expected Check = true")
	}
}

// TestEvaluateExprWithTarget tests the ClassAd method for scoped evaluation
func TestEvaluateExprWithTarget(t *testing.T) {
	job := New()
	job.InsertAttr("RequestCpus", 4)
	job.InsertAttr("RequestMemory", 8192)

	machine := New()
	machine.InsertAttr("Cpus", 8)
	machine.InsertAttr("Memory", 16384)

	// Test job requirements expression
	expr, err := ParseExpr("MY.RequestCpus <= TARGET.Cpus && MY.RequestMemory <= TARGET.Memory")
	if err != nil {
		t.Fatalf("ParseExpr() error = %v", err)
	}

	result := job.EvaluateExprWithTarget(expr, machine)
	if !result.IsBool() {
		t.Fatalf("Expected boolean result, got %v", result.Type())
	}

	matches, _ := result.BoolValue()
	if !matches {
		t.Error("Expected job requirements to be satisfied")
	}

	// Test machine requirements expression
	machineExpr, err := ParseExpr("TARGET.RequestCpus <= MY.Cpus")
	if err != nil {
		t.Fatalf("ParseExpr() error = %v", err)
	}

	machineResult := machine.EvaluateExprWithTarget(machineExpr, job)
	if !machineResult.IsBool() {
		t.Fatalf("Expected boolean result, got %v", machineResult.Type())
	}

	machineAccepts, _ := machineResult.BoolValue()
	if !machineAccepts {
		t.Error("Expected machine to accept job")
	}
}

// TestExprUndefinedReferences tests handling of undefined attributes
func TestExprUndefinedReferences(t *testing.T) {
	ad := New()
	ad.InsertAttr("Cpus", 8)

	// Reference undefined attribute
	expr, err := ParseExpr("UndefinedAttr + Cpus")
	if err != nil {
		t.Fatalf("ParseExpr() error = %v", err)
	}

	result := expr.Eval(ad)
	if !result.IsUndefined() {
		t.Errorf("Expected undefined result when referencing undefined attribute, got %v", result.Type())
	}
}

// TestExprWithoutContext tests evaluating expression without any ClassAd
func TestExprWithoutContext(t *testing.T) {
	// Constant expression
	expr, err := ParseExpr("2 + 3 * 4")
	if err != nil {
		t.Fatalf("ParseExpr() error = %v", err)
	}

	result := expr.Eval(nil)
	if !result.IsInteger() {
		t.Fatalf("Expected integer result, got %v", result.Type())
	}

	value, _ := result.IntValue()
	if value != 14 {
		t.Errorf("Expected 14, got %d", value)
	}
}
