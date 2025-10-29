package classad

import (
	"testing"
)

func TestStringListMember(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		expected bool
	}{
		{
			name:     "simple match",
			expr:     `stringListMember("apple", "apple,banana,cherry")`,
			expected: true,
		},
		{
			name:     "no match",
			expr:     `stringListMember("grape", "apple,banana,cherry")`,
			expected: false,
		},
		{
			name:     "match with spaces",
			expr:     `stringListMember("banana", "apple, banana, cherry")`,
			expected: true,
		},
		{
			name:     "case sensitive no match",
			expr:     `stringListMember("Apple", "apple,banana,cherry")`,
			expected: false,
		},
		{
			name:     "case insensitive match",
			expr:     `stringListMember("Apple", "apple,banana,cherry", "i")`,
			expected: true,
		},
		{
			name:     "case insensitive uppercase option",
			expr:     `stringListMember("BANANA", "apple,banana,cherry", "I")`,
			expected: true,
		},
		{
			name:     "empty list",
			expr:     `stringListMember("apple", "")`,
			expected: false,
		},
		{
			name:     "single element match",
			expr:     `stringListMember("apple", "apple")`,
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse("[test = " + tt.expr + "]")
			if err != nil {
				t.Fatalf("Failed to parse: %v", err)
			}
			val := ad.EvaluateAttr("test")
			if !val.IsBool() {
				t.Fatalf("Expected bool, got %v", val.Type())
			}
			result, _ := val.BoolValue()
			if result != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestRegexp(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		expected bool
		isError  bool
	}{
		{
			name:     "simple match",
			expr:     `regexp("ab+c", "abbbc")`,
			expected: true,
		},
		{
			name:     "no match",
			expr:     `regexp("ab+c", "ac")`,
			expected: false,
		},
		{
			name:     "dot metacharacter",
			expr:     `regexp("a.c", "abc")`,
			expected: true,
		},
		{
			name:     "case sensitive no match",
			expr:     `regexp("ABC", "abc")`,
			expected: false,
		},
		{
			name:     "case insensitive match",
			expr:     `regexp("ABC", "abc", "i")`,
			expected: true,
		},
		{
			name:     "multiline mode",
			expr:     `regexp("^test", "line1\ntest", "m")`,
			expected: true,
		},
		{
			name:     "character class",
			expr:     `regexp("[0-9]+", "abc123def")`,
			expected: true,
		},
		{
			name:     "anchors",
			expr:     `regexp("^hello$", "hello")`,
			expected: true,
		},
		{
			name:     "anchors no match",
			expr:     `regexp("^hello$", "hello world")`,
			expected: false,
		},
		{
			name:     "invalid regex",
			expr:     `regexp("[", "test")`,
			expected: false,
			isError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse("[test = " + tt.expr + "]")
			if err != nil {
				t.Fatalf("Failed to parse: %v", err)
			}
			val := ad.EvaluateAttr("test")
			if tt.isError {
				if !val.IsError() {
					t.Errorf("Expected error, got %v", val.Type())
				}
				return
			}
			if !val.IsBool() {
				t.Fatalf("Expected bool, got %v", val.Type())
			}
			result, _ := val.BoolValue()
			if result != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestIfThenElse(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		expected interface{}
		isError  bool
		isUndef  bool
	}{
		{
			name:     "true condition returns second arg",
			expr:     `ifThenElse(true, 42, 99)`,
			expected: int64(42),
		},
		{
			name:     "false condition returns third arg",
			expr:     `ifThenElse(false, 42, 99)`,
			expected: int64(99),
		},
		{
			name:     "expression condition true",
			expr:     `ifThenElse(5 > 3, "yes", "no")`,
			expected: "yes",
		},
		{
			name:     "expression condition false",
			expr:     `ifThenElse(5 < 3, "yes", "no")`,
			expected: "no",
		},
		{
			name:     "different types in branches",
			expr:     `ifThenElse(true, 42, "string")`,
			expected: int64(42),
		},
		{
			name:     "undefined condition",
			expr:     `ifThenElse(undefined, 42, 99)`,
			expected: nil,
			isUndef:  true,
		},
		{
			name:     "error condition",
			expr:     `ifThenElse(error, 42, 99)`,
			expected: nil,
			isError:  true,
		},
		{
			name:     "non-boolean condition",
			expr:     `ifThenElse(42, "yes", "no")`,
			expected: nil,
			isError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse("[test = " + tt.expr + "]")
			if err != nil {
				t.Fatalf("Failed to parse: %v", err)
			}
			val := ad.EvaluateAttr("test")

			if tt.isError {
				if !val.IsError() {
					t.Errorf("Expected error, got %v", val.Type())
				}
				return
			}

			if tt.isUndef {
				if !val.IsUndefined() {
					t.Errorf("Expected undefined, got %v", val.Type())
				}
				return
			}

			switch expected := tt.expected.(type) {
			case int64:
				if !val.IsInteger() {
					t.Fatalf("Expected integer, got %v", val.Type())
				}
				result, _ := val.IntValue()
				if result != expected {
					t.Errorf("Expected %v, got %v", expected, result)
				}
			case string:
				if !val.IsString() {
					t.Fatalf("Expected string, got %v", val.Type())
				}
				result, _ := val.StringValue()
				if result != expected {
					t.Errorf("Expected %v, got %v", expected, result)
				}
			}
		})
	}
}
