package classad

import (
	"testing"
)

// TestBuiltinLength tests the length() function (alias for size())
func TestBuiltinLength(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int64
		isError  bool
		isUndef  bool
	}{
		{
			name:     "string length",
			input:    `[x = length("hello")]`,
			expected: 5,
		},
		{
			name:     "list length",
			input:    `[x = length({1, 2, 3, 4})]`,
			expected: 4,
		},
		{
			name:     "empty list",
			input:    `[x = length({})]`,
			expected: 0,
		},
		{
			name:    "undefined",
			input:   `[x = length(undefined)]`,
			isUndef: true,
		},
		{
			name:    "error",
			input:   `[x = length(error)]`,
			isError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.input)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			val := ad.EvaluateAttr("x")
			if tt.isError {
				if !val.IsError() {
					t.Errorf("Expected error, got %v", val)
				}
				return
			}
			if tt.isUndef {
				if !val.IsUndefined() {
					t.Errorf("Expected undefined, got %v", val)
				}
				return
			}
			if !val.IsInteger() {
				t.Errorf("Expected integer, got %v", val.Type())
				return
			}
			result, _ := val.IntValue()
			if result != tt.expected {
				t.Errorf("length() = %d, want %d", result, tt.expected)
			}
		})
	}
}

// TestBuiltinQuantize tests the quantize() function
func TestBuiltinQuantize(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected float64
		isInt    bool
		isError  bool
		isUndef  bool
	}{
		{
			name:     "basic quantize integers",
			input:    `[x = quantize(10, 3)]`,
			expected: 12,
			isInt:    true,
		},
		{
			name:     "quantize with floats",
			input:    `[x = quantize(10.5, 3.0)]`,
			expected: 12.0,
		},
		{
			name:     "quantize negative",
			input:    `[x = quantize(-10, 3)]`,
			expected: -9,
			isInt:    true,
		},
		{
			name:     "quantize with list - exact match",
			input:    `[x = quantize(15, {5, 10, 15, 20})]`,
			expected: 15,
			isInt:    true,
		},
		{
			name:     "quantize with list - find next",
			input:    `[x = quantize(12, {5, 10, 15, 20})]`,
			expected: 15,
			isInt:    true,
		},
		{
			name:     "quantize with list - beyond all",
			input:    `[x = quantize(25, {5, 10, 15, 20})]`,
			expected: 40,
			isInt:    true,
		},
		{
			name:    "quantize divide by zero",
			input:   `[x = quantize(10, 0)]`,
			isError: true,
		},
		{
			name:    "quantize undefined",
			input:   `[x = quantize(undefined, 3)]`,
			isUndef: true,
		},
		{
			name:    "quantize error",
			input:   `[x = quantize(error, 3)]`,
			isError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.input)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			val := ad.EvaluateAttr("x")
			if tt.isError {
				if !val.IsError() {
					t.Errorf("Expected error, got %v", val)
				}
				return
			}
			if tt.isUndef {
				if !val.IsUndefined() {
					t.Errorf("Expected undefined, got %v", val)
				}
				return
			}
			if tt.isInt {
				if !val.IsInteger() {
					t.Errorf("Expected integer, got %v", val.Type())
					return
				}
				result, _ := val.IntValue()
				if float64(result) != tt.expected {
					t.Errorf("quantize() = %d, want %v", result, tt.expected)
				}
			} else {
				if !val.IsReal() {
					t.Errorf("Expected real, got %v", val.Type())
					return
				}
				result, _ := val.RealValue()
				if result != tt.expected {
					t.Errorf("quantize() = %f, want %f", result, tt.expected)
				}
			}
		})
	}
}

// TestBuiltinAvg tests the avg() function
func TestBuiltinAvg(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected float64
		isError  bool
		isUndef  bool
	}{
		{
			name:     "average of integers",
			input:    `[x = avg({1, 2, 3, 4, 5})]`,
			expected: 3.0,
		},
		{
			name:     "average of floats",
			input:    `[x = avg({1.5, 2.5, 3.5})]`,
			expected: 2.5,
		},
		{
			name:     "average of mixed",
			input:    `[x = avg({1, 2.5, 3})]`,
			expected: 2.16666666666667, // Update precision to match
		},
		{
			name:     "average with undefined values",
			input:    `[x = avg({1, undefined, 3})]`,
			expected: 2.0,
		},
		{
			name:     "empty list",
			input:    `[x = avg({})]`,
			expected: 0.0,
		},
		{
			name:     "no arguments",
			input:    `[x = avg()]`,
			expected: 0.0,
		},
		{
			name:    "all undefined",
			input:   `[x = avg({undefined, undefined})]`,
			isUndef: true,
		},
		{
			name:    "error in list",
			input:   `[x = avg({1, error, 3})]`,
			isError: true,
		},
		{
			name:    "undefined argument",
			input:   `[x = avg(undefined)]`,
			isUndef: true,
		},
		{
			name:    "not a list",
			input:   `[x = avg(5)]`,
			isError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.input)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			val := ad.EvaluateAttr("x")
			if tt.isError {
				if !val.IsError() {
					t.Errorf("Expected error, got %v", val)
				}
				return
			}
			if tt.isUndef {
				if !val.IsUndefined() {
					t.Errorf("Expected undefined, got %v", val)
				}
				return
			}
			if !val.IsReal() {
				t.Errorf("Expected real, got %v", val.Type())
				return
			}
			result, _ := val.RealValue()
			// Use tolerance for floating point comparison
			diff := result - tt.expected
			if diff < 0 {
				diff = -diff
			}
			if diff > 1e-10 {
				t.Errorf("avg() = %f, want %f", result, tt.expected)
			}
		})
	}
}

// TestBuiltinMin tests the min() function
func TestBuiltinMin(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected float64
		isInt    bool
		isError  bool
		isUndef  bool
	}{
		{
			name:     "min of integers",
			input:    `[x = min({5, 2, 8, 1, 9})]`,
			expected: 1,
			isInt:    true,
		},
		{
			name:     "min of floats",
			input:    `[x = min({5.5, 2.2, 8.8, 1.1})]`,
			expected: 1.1,
		},
		{
			name:     "min with undefined",
			input:    `[x = min({5, undefined, 2})]`,
			expected: 2,
			isInt:    true,
		},
		{
			name:    "empty list",
			input:   `[x = min({})]`,
			isUndef: true,
		},
		{
			name:    "no arguments",
			input:   `[x = min()]`,
			isUndef: true,
		},
		{
			name:    "all undefined",
			input:   `[x = min({undefined, undefined})]`,
			isUndef: true,
		},
		{
			name:    "error in list",
			input:   `[x = min({1, error, 3})]`,
			isError: true,
		},
		{
			name:    "undefined argument",
			input:   `[x = min(undefined)]`,
			isUndef: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.input)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			val := ad.EvaluateAttr("x")
			if tt.isError {
				if !val.IsError() {
					t.Errorf("Expected error, got %v", val)
				}
				return
			}
			if tt.isUndef {
				if !val.IsUndefined() {
					t.Errorf("Expected undefined, got %v", val)
				}
				return
			}
			if tt.isInt {
				if !val.IsInteger() {
					t.Errorf("Expected integer, got %v", val.Type())
					return
				}
				result, _ := val.IntValue()
				if float64(result) != tt.expected {
					t.Errorf("min() = %d, want %v", result, tt.expected)
				}
			} else {
				if !val.IsReal() {
					t.Errorf("Expected real, got %v", val.Type())
					return
				}
				result, _ := val.RealValue()
				if result != tt.expected {
					t.Errorf("min() = %f, want %f", result, tt.expected)
				}
			}
		})
	}
}

// TestBuiltinMax tests the max() function
func TestBuiltinMax(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected float64
		isInt    bool
		isError  bool
		isUndef  bool
	}{
		{
			name:     "max of integers",
			input:    `[x = max({5, 2, 8, 1, 9})]`,
			expected: 9,
			isInt:    true,
		},
		{
			name:     "max of floats",
			input:    `[x = max({5.5, 2.2, 8.8, 1.1})]`,
			expected: 8.8,
		},
		{
			name:     "max with undefined",
			input:    `[x = max({5, undefined, 8})]`,
			expected: 8,
			isInt:    true,
		},
		{
			name:    "empty list",
			input:   `[x = max({})]`,
			isUndef: true,
		},
		{
			name:    "no arguments",
			input:   `[x = max()]`,
			isUndef: true,
		},
		{
			name:    "all undefined",
			input:   `[x = max({undefined, undefined})]`,
			isUndef: true,
		},
		{
			name:    "error in list",
			input:   `[x = max({1, error, 3})]`,
			isError: true,
		},
		{
			name:    "undefined argument",
			input:   `[x = max(undefined)]`,
			isUndef: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.input)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			val := ad.EvaluateAttr("x")
			if tt.isError {
				if !val.IsError() {
					t.Errorf("Expected error, got %v", val)
				}
				return
			}
			if tt.isUndef {
				if !val.IsUndefined() {
					t.Errorf("Expected undefined, got %v", val)
				}
				return
			}
			if tt.isInt {
				if !val.IsInteger() {
					t.Errorf("Expected integer, got %v", val.Type())
					return
				}
				result, _ := val.IntValue()
				if float64(result) != tt.expected {
					t.Errorf("max() = %d, want %v", result, tt.expected)
				}
			} else {
				if !val.IsReal() {
					t.Errorf("Expected real, got %v", val.Type())
					return
				}
				result, _ := val.RealValue()
				if result != tt.expected {
					t.Errorf("max() = %f, want %f", result, tt.expected)
				}
			}
		})
	}
}

// TestBuiltinSplitSlotName tests the splitslotname() function
func TestBuiltinSplitSlotName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
		isError  bool
		isUndef  bool
	}{
		{
			name:     "slot with machine",
			input:    `[x = splitSlotName("slot1@machine.example.com")]`,
			expected: []string{"slot1", "machine.example.com"},
		},
		{
			name:     "just machine name",
			input:    `[x = splitSlotName("machine.example.com")]`,
			expected: []string{"", "machine.example.com"},
		},
		{
			name:     "multiple @ signs",
			input:    `[x = splitSlotName("slot1@sub@machine.example.com")]`,
			expected: []string{"slot1", "sub@machine.example.com"},
		},
		{
			name:    "undefined",
			input:   `[x = splitSlotName(undefined)]`,
			isUndef: true,
		},
		{
			name:    "error",
			input:   `[x = splitSlotName(error)]`,
			isError: true,
		},
		{
			name:    "not a string",
			input:   `[x = splitSlotName(123)]`,
			isError: true,
		},
		{
			name:    "wrong number of args",
			input:   `[x = splitSlotName("a", "b")]`,
			isError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.input)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			val := ad.EvaluateAttr("x")
			if tt.isError {
				if !val.IsError() {
					t.Errorf("Expected error, got %v", val)
				}
				return
			}
			if tt.isUndef {
				if !val.IsUndefined() {
					t.Errorf("Expected undefined, got %v", val)
				}
				return
			}
			if !val.IsList() {
				t.Errorf("Expected list, got %v", val.Type())
				return
			}
			list, _ := val.ListValue()
			if len(list) != len(tt.expected) {
				t.Errorf("Expected list length %d, got %d", len(tt.expected), len(list))
				return
			}
			for i, item := range list {
				if !item.IsString() {
					t.Errorf("List item %d is not a string", i)
					continue
				}
				str, _ := item.StringValue()
				if str != tt.expected[i] {
					t.Errorf("List item %d = %q, want %q", i, str, tt.expected[i])
				}
			}
		})
	}
}

// TestBuiltinRandom tests the random() function edge cases
func TestBuiltinRandom(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		isError    bool
		isUndef    bool
		checkRange bool
		maxVal     float64
	}{
		{
			name:       "random with max",
			input:      `[x = random(100)]`,
			checkRange: true,
			maxVal:     100.0,
		},
		{
			name:       "random no args",
			input:      `[x = random()]`,
			checkRange: true,
			maxVal:     1.0,
		},
		{
			name:    "random undefined",
			input:   `[x = random(undefined)]`,
			isUndef: true,
		},
		{
			name:    "random error",
			input:   `[x = random(error)]`,
			isError: true,
		},
		{
			name:    "random not a number",
			input:   `[x = random("string")]`,
			isError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.input)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			val := ad.EvaluateAttr("x")
			if tt.isError {
				if !val.IsError() {
					t.Errorf("Expected error, got %v", val)
				}
				return
			}
			if tt.isUndef {
				if !val.IsUndefined() {
					t.Errorf("Expected undefined, got %v", val)
				}
				return
			}
			if tt.checkRange {
				if !val.IsReal() {
					t.Errorf("Expected real, got %v", val.Type())
					return
				}
				result, _ := val.RealValue()
				if result < 0 || result > tt.maxVal {
					t.Errorf("random() = %f, want value in range [0, %f]", result, tt.maxVal)
				}
			}
		})
	}
}
