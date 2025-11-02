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

func TestStringListIMember(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		expected bool
	}{
		{
			name:     "case insensitive match lowercase",
			expr:     `stringListIMember("apple", "Apple,Banana,Cherry")`,
			expected: true,
		},
		{
			name:     "case insensitive match uppercase",
			expr:     `stringListIMember("BANANA", "apple,banana,cherry")`,
			expected: true,
		},
		{
			name:     "case insensitive match mixed",
			expr:     `stringListIMember("BaNaNa", "apple,banana,cherry")`,
			expected: true,
		},
		{
			name:     "no match",
			expr:     `stringListIMember("grape", "apple,banana,cherry")`,
			expected: false,
		},
		{
			name:     "match with spaces",
			expr:     `stringListIMember("APPLE", "apple, banana, cherry")`,
			expected: true,
		},
		{
			name:     "empty list",
			expr:     `stringListIMember("apple", "")`,
			expected: false,
		},
		{
			name:     "single element match",
			expr:     `stringListIMember("APPLE", "apple")`,
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

// TestBuiltinString tests the string() conversion function
func TestBuiltinString(t *testing.T) {
	tests := []struct {
		name     string
		classad  string
		expr     string
		expected string
	}{
		{
			name:     "integer to string",
			classad:  "[]",
			expr:     "string(42)",
			expected: "42",
		},
		{
			name:     "real to string",
			classad:  "[]",
			expr:     "string(3.14)",
			expected: "3.14",
		},
		{
			name:     "boolean true to string",
			classad:  "[]",
			expr:     "string(true)",
			expected: "true",
		},
		{
			name:     "boolean false to string",
			classad:  "[]",
			expr:     "string(false)",
			expected: "false",
		},
		{
			name:     "already string",
			classad:  "[]",
			expr:     `string("hello")`,
			expected: "hello",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.classad)
			if err != nil {
				t.Fatalf("Parse failed: %v", err)
			}

			expr, err := ParseExpr(tt.expr)
			if err != nil {
				t.Fatalf("ParseExpr failed: %v", err)
			}

			val := ad.EvaluateExprWithTarget(expr, nil)
			if val.IsError() {
				t.Fatalf("Expected string value, got ERROR")
			}
			if val.IsUndefined() {
				t.Fatalf("Expected string value, got UNDEFINED")
			}

			str, err := val.StringValue()
			if err != nil {
				t.Fatalf("StringValue() error: %v", err)
			}
			if str != tt.expected {
				t.Errorf("string() = %q, want %q", str, tt.expected)
			}
		})
	}
}

// TestBuiltinBool tests the bool() conversion function
func TestBuiltinBool(t *testing.T) {
	tests := []struct {
		name     string
		classad  string
		expr     string
		expected bool
		isError  bool
	}{
		{
			name:     "string true",
			classad:  "[]",
			expr:     `bool("true")`,
			expected: true,
		},
		{
			name:     "string false",
			classad:  "[]",
			expr:     `bool("false")`,
			expected: false,
		},
		{
			name:     "integer 1",
			classad:  "[]",
			expr:     "bool(1)",
			expected: true,
		},
		{
			name:     "integer 0",
			classad:  "[]",
			expr:     "bool(0)",
			expected: false,
		},
		{
			name:     "integer non-zero",
			classad:  "[]",
			expr:     "bool(42)",
			expected: true,
		},
		{
			name:     "real non-zero",
			classad:  "[]",
			expr:     "bool(3.14)",
			expected: true,
		},
		{
			name:     "real zero",
			classad:  "[]",
			expr:     "bool(0.0)",
			expected: false,
		},
		{
			name:    "invalid string",
			classad: "[]",
			expr:    `bool("invalid")`,
			isError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.classad)
			if err != nil {
				t.Fatalf("Parse failed: %v", err)
			}

			expr, err := ParseExpr(tt.expr)
			if err != nil {
				t.Fatalf("ParseExpr failed: %v", err)
			}

			val := ad.EvaluateExprWithTarget(expr, nil)

			if tt.isError {
				if !val.IsError() {
					t.Errorf("Expected ERROR, got %v", val)
				}
				return
			}

			if val.IsError() {
				t.Fatalf("Expected boolean value, got ERROR")
			}
			if val.IsUndefined() {
				t.Fatalf("Expected boolean value, got UNDEFINED")
			}

			boolVal, err := val.BoolValue()
			if err != nil {
				t.Fatalf("BoolValue() error: %v", err)
			}
			if boolVal != tt.expected {
				t.Errorf("bool() = %v, want %v", boolVal, tt.expected)
			}
		})
	}
}

// TestBuiltinPow tests the pow() function
func TestBuiltinPow(t *testing.T) {
	tests := []struct {
		name     string
		classad  string
		expr     string
		expected interface{} // int64 or float64
	}{
		{
			name:     "integer power positive",
			classad:  "[]",
			expr:     "pow(2, 3)",
			expected: int64(8),
		},
		{
			name:     "integer power zero",
			classad:  "[]",
			expr:     "pow(5, 0)",
			expected: int64(1),
		},
		{
			name:     "integer power negative",
			classad:  "[]",
			expr:     "pow(2, -2)",
			expected: 0.25,
		},
		{
			name:     "real base",
			classad:  "[]",
			expr:     "pow(2.0, 3)",
			expected: 8.0,
		},
		{
			name:     "real exponent",
			classad:  "[]",
			expr:     "pow(4, 0.5)",
			expected: 2.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.classad)
			if err != nil {
				t.Fatalf("Parse failed: %v", err)
			}

			expr, err := ParseExpr(tt.expr)
			if err != nil {
				t.Fatalf("ParseExpr failed: %v", err)
			}

			val := ad.EvaluateExprWithTarget(expr, nil)
			if val.IsError() || val.IsUndefined() {
				t.Fatalf("Expected numeric value, got %v", val)
			}

			switch exp := tt.expected.(type) {
			case int64:
				if !val.IsInteger() {
					t.Errorf("Expected integer, got %v", val.Type())
				}
				intVal, _ := val.IntValue()
				if intVal != exp {
					t.Errorf("pow() = %d, want %d", intVal, exp)
				}
			case float64:
				if !val.IsReal() {
					t.Errorf("Expected real, got %v", val.Type())
				}
				realVal, _ := val.RealValue()
				if realVal != exp {
					t.Errorf("pow() = %f, want %f", realVal, exp)
				}
			}
		})
	}
}

// TestBuiltinSum tests the sum() function
func TestBuiltinSum(t *testing.T) {
	tests := []struct {
		name     string
		classad  string
		expr     string
		expected interface{} // int64 or float64
	}{
		{
			name:     "integer list",
			classad:  "[]",
			expr:     "sum({1, 2, 3, 4})",
			expected: int64(10),
		},
		{
			name:     "real list",
			classad:  "[]",
			expr:     "sum({1.5, 2.5, 3.0})",
			expected: 7.0,
		},
		{
			name:     "mixed list",
			classad:  "[]",
			expr:     "sum({1, 2.5, 3})",
			expected: 6.5,
		},
		{
			name:     "empty list",
			classad:  "[]",
			expr:     "sum({})",
			expected: int64(0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.classad)
			if err != nil {
				t.Fatalf("Parse failed: %v", err)
			}

			expr, err := ParseExpr(tt.expr)
			if err != nil {
				t.Fatalf("ParseExpr failed: %v", err)
			}

			val := ad.EvaluateExprWithTarget(expr, nil)
			if val.IsError() || val.IsUndefined() {
				t.Fatalf("Expected numeric value, got %v", val)
			}

			switch exp := tt.expected.(type) {
			case int64:
				intVal, _ := val.IntValue()
				if intVal != exp {
					t.Errorf("sum() = %d, want %d", intVal, exp)
				}
			case float64:
				realVal, _ := val.RealValue()
				if realVal != exp {
					t.Errorf("sum() = %f, want %f", realVal, exp)
				}
			}
		})
	}
}

func TestBuiltinJoin(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		expected string
	}{
		{"join with separator and varargs", `join(",", "a", "b", "c")`, "a,b,c"},
		{"join with list", `join(",", {"hello", "world"})`, "hello,world"},
		{"join no separator", `join({"a", "b", "c"})`, "abc"},
		{"join mixed types", `join("-", 1, 2, 3)`, "1-2-3"},
		{"join empty sep", `join("", "x", "y")`, "xy"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse("[test = " + tt.expr + "]")
			if err != nil {
				t.Fatalf("Failed to parse: %v", err)
			}
			val := ad.EvaluateAttr("test")
			if val.IsError() {
				t.Errorf("join() returned ERROR")
			} else if !val.IsString() {
				t.Errorf("join() did not return string, got %v", val.Type())
			} else {
				str, _ := val.StringValue()
				if str != tt.expected {
					t.Errorf("join() = %q, want %q", str, tt.expected)
				}
			}
		})
	}
}

func TestBuiltinSplit(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		expected []string
	}{
		{"split whitespace", `split("one two three")`, []string{"one", "two", "three"}},
		{"split custom delim", `split("a,b,c", ",")`, []string{"a", "b", "c"}},
		{"split multiple delims", `split("a,b;c", ",;")`, []string{"a", "b", "c"}},
		{"split empty", `split("")`, []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse("[test = " + tt.expr + "]")
			if err != nil {
				t.Fatalf("Failed to parse: %v", err)
			}
			val := ad.EvaluateAttr("test")
			if val.IsError() {
				t.Errorf("split() returned ERROR")
			} else if !val.IsList() {
				t.Errorf("split() did not return list, got %v", val.Type())
			} else {
				list, _ := val.ListValue()
				if len(list) != len(tt.expected) {
					t.Errorf("split() length = %d, want %d", len(list), len(tt.expected))
				} else {
					for i, item := range list {
						if !item.IsString() {
							t.Errorf("split()[%d] is not string", i)
						} else {
							str, _ := item.StringValue()
							if str != tt.expected[i] {
								t.Errorf("split()[%d] = %q, want %q", i, str, tt.expected[i])
							}
						}
					}
				}
			}
		})
	}
}

func TestBuiltinSplitUserName(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		expected []string
	}{
		{"with domain", `splitUserName("alice@example.com")`, []string{"alice", "example.com"}},
		{"no domain", `splitUserName("bob")`, []string{"bob", ""}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse("[test = " + tt.expr + "]")
			if err != nil {
				t.Fatalf("Failed to parse: %v", err)
			}
			val := ad.EvaluateAttr("test")
			if val.IsError() {
				t.Errorf("splitUserName() returned ERROR")
			} else if !val.IsList() {
				t.Errorf("splitUserName() did not return list, got %v", val.Type())
			} else {
				list, _ := val.ListValue()
				if len(list) != 2 {
					t.Errorf("splitUserName() length = %d, want 2", len(list))
				} else {
					for i := 0; i < 2; i++ {
						if !list[i].IsString() {
							t.Errorf("splitUserName()[%d] is not string", i)
						} else {
							str, _ := list[i].StringValue()
							if str != tt.expected[i] {
								t.Errorf("splitUserName()[%d] = %q, want %q", i, str, tt.expected[i])
							}
						}
					}
				}
			}
		})
	}
}

func TestBuiltinStrcmp(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		expected int64
	}{
		{"equal", `strcmp("abc", "abc")`, 0},
		{"less", `strcmp("abc", "def")`, -1},
		{"greater", `strcmp("def", "abc")`, 1},
		{"case matters", `strcmp("ABC", "abc")`, -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse("[test = " + tt.expr + "]")
			if err != nil {
				t.Fatalf("Failed to parse: %v", err)
			}
			val := ad.EvaluateAttr("test")
			if val.IsError() {
				t.Errorf("strcmp() returned ERROR")
			} else if !val.IsInteger() {
				t.Errorf("strcmp() did not return integer, got %v", val.Type())
			} else {
				result, _ := val.IntValue()
				// Compare signs only
				if (result < 0) != (tt.expected < 0) || (result > 0) != (tt.expected > 0) || (result == 0) != (tt.expected == 0) {
					t.Errorf("strcmp() = %d, want %d", result, tt.expected)
				}
			}
		})
	}
}

func TestBuiltinStricmp(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		expected int64
	}{
		{"equal", `stricmp("abc", "ABC")`, 0},
		{"less", `stricmp("ABC", "DEF")`, -1},
		{"greater", `stricmp("DEF", "ABC")`, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse("[test = " + tt.expr + "]")
			if err != nil {
				t.Fatalf("Failed to parse: %v", err)
			}
			val := ad.EvaluateAttr("test")
			if val.IsError() {
				t.Errorf("stricmp() returned ERROR")
			} else if !val.IsInteger() {
				t.Errorf("stricmp() did not return integer, got %v", val.Type())
			} else {
				result, _ := val.IntValue()
				// Compare signs only
				if (result < 0) != (tt.expected < 0) || (result > 0) != (tt.expected > 0) || (result == 0) != (tt.expected == 0) {
					t.Errorf("stricmp() = %d, want %d", result, tt.expected)
				}
			}
		})
	}
}

func TestBuiltinVersioncmp(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		expected int64
	}{
		{"equal", `versioncmp("1.2.3", "1.2.3")`, 0},
		{"less major", `versioncmp("1.2.3", "2.0.0")`, -1},
		{"greater minor", `versioncmp("1.3.0", "1.2.9")`, 1},
		{"numeric vs text", `versioncmp("1.10", "1.9")`, 1},
		{"different lengths", `versioncmp("1.2", "1.2.0")`, -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse("[test = " + tt.expr + "]")
			if err != nil {
				t.Fatalf("Failed to parse: %v", err)
			}
			val := ad.EvaluateAttr("test")
			if val.IsError() {
				t.Errorf("versioncmp() returned ERROR")
			} else if !val.IsInteger() {
				t.Errorf("versioncmp() did not return integer, got %v", val.Type())
			} else {
				result, _ := val.IntValue()
				// Compare signs only
				if (result < 0) != (tt.expected < 0) || (result > 0) != (tt.expected > 0) || (result == 0) != (tt.expected == 0) {
					t.Errorf("versioncmp() = %d, want %d", result, tt.expected)
				}
			}
		})
	}
}

func TestBuiltinVersionComparisonFunctions(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		expected bool
	}{
		{"version_gt true", `version_gt("2.0", "1.9")`, true},
		{"version_gt false", `version_gt("1.9", "2.0")`, false},
		{"version_ge equal", `version_ge("1.5", "1.5")`, true},
		{"version_lt true", `version_lt("1.0", "2.0")`, true},
		{"version_le equal", `version_le("3.0", "3.0")`, true},
		{"version_eq true", `version_eq("1.2.3", "1.2.3")`, true},
		{"version_in_range true", `version_in_range("1.5", "1.0", "2.0")`, true},
		{"version_in_range false", `version_in_range("2.5", "1.0", "2.0")`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse("[test = " + tt.expr + "]")
			if err != nil {
				t.Fatalf("Failed to parse: %v", err)
			}
			val := ad.EvaluateAttr("test")
			if val.IsError() {
				t.Errorf("version function returned ERROR")
			} else if !val.IsBool() {
				t.Errorf("version function did not return boolean, got %v", val.Type())
			} else {
				result, _ := val.BoolValue()
				if result != tt.expected {
					t.Errorf("version function = %v, want %v", result, tt.expected)
				}
			}
		})
	}
}

func TestBuiltinFormatTime(t *testing.T) {
	tests := []struct {
		name  string
		expr  string
		check func(string) bool
	}{
		{"default format", `formatTime(1234567890)`, func(s string) bool { return len(s) > 10 }},
		{"year only", `formatTime(1234567890, "%Y")`, func(s string) bool { return s == "2009" }},
		{"date", `formatTime(1234567890, "%Y-%m-%d")`, func(s string) bool { return s == "2009-02-13" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse("[test = " + tt.expr + "]")
			if err != nil {
				t.Fatalf("Failed to parse: %v", err)
			}
			val := ad.EvaluateAttr("test")
			if val.IsError() {
				t.Errorf("formatTime() returned ERROR")
			} else if !val.IsString() {
				t.Errorf("formatTime() did not return string, got %v", val.Type())
			} else {
				str, _ := val.StringValue()
				if !tt.check(str) {
					t.Errorf("formatTime() = %q, check failed", str)
				}
			}
		})
	}
}

func TestBuiltinInterval(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		expected string
	}{
		{"seconds only", `interval(45)`, "0:45"},
		{"minutes", `interval(125)`, "2:05"},
		{"hours", `interval(3665)`, "1:01:05"},
		{"days", `interval(90125)`, "1+01:02:05"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse("[test = " + tt.expr + "]")
			if err != nil {
				t.Fatalf("Failed to parse: %v", err)
			}
			val := ad.EvaluateAttr("test")
			if val.IsError() {
				t.Errorf("interval() returned ERROR")
			} else if !val.IsString() {
				t.Errorf("interval() did not return string, got %v", val.Type())
			} else {
				str, _ := val.StringValue()
				if str != tt.expected {
					t.Errorf("interval() = %q, want %q", str, tt.expected)
				}
			}
		})
	}
}

func TestBuiltinIdenticalMember(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		expected bool
	}{
		{"found integer", `identicalMember(2, {1, 2, 3})`, true},
		{"not found", `identicalMember(4, {1, 2, 3})`, false},
		{"found string", `identicalMember("b", {"a", "b", "c"})`, true},
		{"type mismatch", `identicalMember(2, {"1", "2", "3"})`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse("[test = " + tt.expr + "]")
			if err != nil {
				t.Fatalf("Failed to parse: %v", err)
			}
			val := ad.EvaluateAttr("test")
			if val.IsError() {
				t.Errorf("identicalMember() returned ERROR")
			} else if !val.IsBool() {
				t.Errorf("identicalMember() did not return boolean, got %v", val.Type())
			} else {
				result, _ := val.BoolValue()
				if result != tt.expected {
					t.Errorf("identicalMember() = %v, want %v", result, tt.expected)
				}
			}
		})
	}
}

func TestAnyCompare(t *testing.T) {
	tests := []struct {
		name     string
		op       string
		list     []Value
		target   Value
		expected bool
	}{
		{
			name:     "any greater than",
			op:       ">",
			list:     []Value{NewIntValue(1), NewIntValue(5), NewIntValue(3)},
			target:   NewIntValue(4),
			expected: true,
		},
		{
			name:     "any less than",
			op:       "<",
			list:     []Value{NewIntValue(10), NewIntValue(5), NewIntValue(8)},
			target:   NewIntValue(7),
			expected: true,
		},
		{
			name:     "any equals",
			op:       "==",
			list:     []Value{NewIntValue(1), NewIntValue(2), NewIntValue(3)},
			target:   NewIntValue(2),
			expected: true,
		},
		{
			name:     "none match",
			op:       ">",
			list:     []Value{NewIntValue(1), NewIntValue(2), NewIntValue(3)},
			target:   NewIntValue(5),
			expected: false,
		},
		{
			name:     "string comparison",
			op:       "==",
			list:     []Value{NewStringValue("a"), NewStringValue("b"), NewStringValue("c")},
			target:   NewStringValue("b"),
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []Value{NewStringValue(tt.op), NewListValue(tt.list), tt.target}
			val := builtinAnyCompare(args)
			if !val.IsBool() {
				t.Errorf("anyCompare() did not return boolean, got %v", val.Type())
			} else {
				result, _ := val.BoolValue()
				if result != tt.expected {
					t.Errorf("anyCompare() = %v, want %v", result, tt.expected)
				}
			}
		})
	}
}

func TestAllCompare(t *testing.T) {
	tests := []struct {
		name     string
		op       string
		list     []Value
		target   Value
		expected bool
	}{
		{
			name:     "all greater than",
			op:       ">",
			list:     []Value{NewIntValue(5), NewIntValue(6), NewIntValue(7)},
			target:   NewIntValue(4),
			expected: true,
		},
		{
			name:     "all less than",
			op:       "<",
			list:     []Value{NewIntValue(1), NewIntValue(2), NewIntValue(3)},
			target:   NewIntValue(5),
			expected: true,
		},
		{
			name:     "not all match",
			op:       ">",
			list:     []Value{NewIntValue(5), NewIntValue(3), NewIntValue(7)},
			target:   NewIntValue(4),
			expected: false,
		},
		{
			name:     "empty list",
			op:       ">",
			list:     []Value{},
			target:   NewIntValue(5),
			expected: true, // vacuously true
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []Value{NewStringValue(tt.op), NewListValue(tt.list), tt.target}
			val := builtinAllCompare(args)
			if !val.IsBool() {
				t.Errorf("allCompare() did not return boolean, got %v", val.Type())
			} else {
				result, _ := val.BoolValue()
				if result != tt.expected {
					t.Errorf("allCompare() = %v, want %v", result, tt.expected)
				}
			}
		})
	}
}

func TestStringListSize(t *testing.T) {
	tests := []struct {
		name      string
		listStr   string
		delimiter string
		expected  int64
	}{
		{
			name:     "comma separated",
			listStr:  "a,b,c,d",
			expected: 4,
		},
		{
			name:      "semicolon separated",
			listStr:   "x;y;z",
			delimiter: ";",
			expected:  3,
		},
		{
			name:     "with spaces",
			listStr:  "one, two, three",
			expected: 3,
		},
		{
			name:     "empty string",
			listStr:  "",
			expected: 0,
		},
		{
			name:     "single item",
			listStr:  "solo",
			expected: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var args []Value
			if tt.delimiter != "" {
				args = []Value{NewStringValue(tt.listStr), NewStringValue(tt.delimiter)}
			} else {
				args = []Value{NewStringValue(tt.listStr)}
			}

			val := builtinStringListSize(args)
			if !val.IsInteger() {
				t.Errorf("stringListSize() did not return integer, got %v", val.Type())
			} else {
				result, _ := val.IntValue()
				if result != tt.expected {
					t.Errorf("stringListSize() = %v, want %v", result, tt.expected)
				}
			}
		})
	}
}

func TestStringListSum(t *testing.T) {
	tests := []struct {
		name     string
		listStr  string
		expected float64
		isInt    bool
	}{
		{
			name:     "integers",
			listStr:  "1,2,3,4",
			expected: 10,
			isInt:    true,
		},
		{
			name:     "reals",
			listStr:  "1.5,2.5,3.0",
			expected: 7.0,
			isInt:    false,
		},
		{
			name:     "mixed",
			listStr:  "1,2.5,3",
			expected: 6.5,
			isInt:    false,
		},
		{
			name:     "with spaces",
			listStr:  "10, 20, 30",
			expected: 60,
			isInt:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []Value{NewStringValue(tt.listStr)}
			val := builtinStringListSum(args)
			if tt.isInt && !val.IsInteger() {
				t.Errorf("stringListSum() expected int, got %v", val.Type())
			} else if !tt.isInt && !val.IsReal() {
				t.Errorf("stringListSum() expected real, got %v", val.Type())
			} else {
				result, _ := val.NumberValue()
				if result != tt.expected {
					t.Errorf("stringListSum() = %v, want %v", result, tt.expected)
				}
			}
		})
	}
}

func TestStringListAvg(t *testing.T) {
	tests := []struct {
		name     string
		listStr  string
		expected float64
	}{
		{
			name:     "integers",
			listStr:  "2,4,6,8",
			expected: 5.0,
		},
		{
			name:     "reals",
			listStr:  "1.0,2.0,3.0",
			expected: 2.0,
		},
		{
			name:     "single value",
			listStr:  "42",
			expected: 42.0,
		},
		{
			name:     "empty list",
			listStr:  "",
			expected: 0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []Value{NewStringValue(tt.listStr)}
			val := builtinStringListAvg(args)
			if !val.IsReal() {
				t.Errorf("stringListAvg() did not return real, got %v", val.Type())
			} else {
				result, _ := val.RealValue()
				if result != tt.expected {
					t.Errorf("stringListAvg() = %v, want %v", result, tt.expected)
				}
			}
		})
	}
}

func TestStringListMin(t *testing.T) {
	tests := []struct {
		name        string
		listStr     string
		expected    float64
		isInt       bool
		isUndefined bool
	}{
		{
			name:     "integers",
			listStr:  "5,2,8,1,9",
			expected: 1,
			isInt:    true,
		},
		{
			name:     "reals",
			listStr:  "5.5,2.1,8.9",
			expected: 2.1,
			isInt:    false,
		},
		{
			name:        "empty list",
			listStr:     "",
			isUndefined: true,
		},
		{
			name:     "negative numbers",
			listStr:  "5,-3,2",
			expected: -3,
			isInt:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []Value{NewStringValue(tt.listStr)}
			val := builtinStringListMin(args)
			if tt.isUndefined {
				if !val.IsUndefined() {
					t.Errorf("stringListMin() expected undefined, got %v", val.Type())
				}
			} else if tt.isInt && !val.IsInteger() {
				t.Errorf("stringListMin() expected int, got %v", val.Type())
			} else if !tt.isInt && !val.IsReal() {
				t.Errorf("stringListMin() expected real, got %v", val.Type())
			} else if !tt.isUndefined {
				result, _ := val.NumberValue()
				if result != tt.expected {
					t.Errorf("stringListMin() = %v, want %v", result, tt.expected)
				}
			}
		})
	}
}

func TestStringListMax(t *testing.T) {
	tests := []struct {
		name        string
		listStr     string
		expected    float64
		isInt       bool
		isUndefined bool
	}{
		{
			name:     "integers",
			listStr:  "5,2,8,1,9",
			expected: 9,
			isInt:    true,
		},
		{
			name:     "reals",
			listStr:  "5.5,2.1,8.9",
			expected: 8.9,
			isInt:    false,
		},
		{
			name:        "empty list",
			listStr:     "",
			isUndefined: true,
		},
		{
			name:     "negative numbers",
			listStr:  "-5,-3,-2",
			expected: -2,
			isInt:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []Value{NewStringValue(tt.listStr)}
			val := builtinStringListMax(args)
			if tt.isUndefined {
				if !val.IsUndefined() {
					t.Errorf("stringListMax() expected undefined, got %v", val.Type())
				}
			} else if tt.isInt && !val.IsInteger() {
				t.Errorf("stringListMax() expected int, got %v", val.Type())
			} else if !tt.isInt && !val.IsReal() {
				t.Errorf("stringListMax() expected real, got %v", val.Type())
			} else if !tt.isUndefined {
				result, _ := val.NumberValue()
				if result != tt.expected {
					t.Errorf("stringListMax() = %v, want %v", result, tt.expected)
				}
			}
		})
	}
}

func TestStringListsIntersect(t *testing.T) {
	tests := []struct {
		name     string
		list1    string
		list2    string
		expected bool
	}{
		{
			name:     "common elements",
			list1:    "a,b,c",
			list2:    "c,d,e",
			expected: true,
		},
		{
			name:     "no common elements",
			list1:    "a,b,c",
			list2:    "x,y,z",
			expected: false,
		},
		{
			name:     "multiple common",
			list1:    "1,2,3,4",
			list2:    "3,4,5,6",
			expected: true,
		},
		{
			name:     "empty list1",
			list1:    "",
			list2:    "a,b,c",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []Value{NewStringValue(tt.list1), NewStringValue(tt.list2)}
			val := builtinStringListsIntersect(args)
			if !val.IsBool() {
				t.Errorf("stringListsIntersect() did not return boolean, got %v", val.Type())
			} else {
				result, _ := val.BoolValue()
				if result != tt.expected {
					t.Errorf("stringListsIntersect() = %v, want %v", result, tt.expected)
				}
			}
		})
	}
}

func TestStringListSubsetMatch(t *testing.T) {
	tests := []struct {
		name     string
		list1    string
		list2    string
		expected bool
	}{
		{
			name:     "is subset",
			list1:    "a,b",
			list2:    "a,b,c,d",
			expected: true,
		},
		{
			name:     "not subset",
			list1:    "a,b,x",
			list2:    "a,b,c,d",
			expected: false,
		},
		{
			name:     "equal lists",
			list1:    "a,b,c",
			list2:    "a,b,c",
			expected: true,
		},
		{
			name:     "empty subset",
			list1:    "",
			list2:    "a,b,c",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []Value{NewStringValue(tt.list1), NewStringValue(tt.list2)}
			val := builtinStringListSubsetMatch(args)
			if !val.IsBool() {
				t.Errorf("stringListSubsetMatch() did not return boolean, got %v", val.Type())
			} else {
				result, _ := val.BoolValue()
				if result != tt.expected {
					t.Errorf("stringListSubsetMatch() = %v, want %v", result, tt.expected)
				}
			}
		})
	}
}

func TestStringListRegexpMember(t *testing.T) {
	tests := []struct {
		name     string
		pattern  string
		listStr  string
		expected bool
	}{
		{
			name:     "match found",
			pattern:  "^foo",
			listStr:  "bar,foobar,baz",
			expected: true,
		},
		{
			name:     "no match",
			pattern:  "^xyz",
			listStr:  "foo,bar,baz",
			expected: false,
		},
		{
			name:     "case insensitive",
			pattern:  "FOO",
			listStr:  "bar,foo,baz",
			expected: false, // no case-insensitive flag
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []Value{NewStringValue(tt.pattern), NewStringValue(tt.listStr)}
			val := builtinStringListRegexpMember(args)
			if !val.IsBool() {
				t.Errorf("stringListRegexpMember() did not return boolean, got %v", val.Type())
			} else {
				result, _ := val.BoolValue()
				if result != tt.expected {
					t.Errorf("stringListRegexpMember() = %v, want %v", result, tt.expected)
				}
			}
		})
	}
}

func TestRegexpMember(t *testing.T) {
	tests := []struct {
		name     string
		pattern  string
		list     []Value
		expected bool
	}{
		{
			name:     "match found",
			pattern:  "^foo",
			list:     []Value{NewStringValue("bar"), NewStringValue("foobar"), NewStringValue("baz")},
			expected: true,
		},
		{
			name:     "no match",
			pattern:  "^xyz",
			list:     []Value{NewStringValue("foo"), NewStringValue("bar"), NewStringValue("baz")},
			expected: false,
		},
		{
			name:     "partial match",
			pattern:  "test",
			list:     []Value{NewStringValue("testing"), NewStringValue("foo"), NewStringValue("bar")},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []Value{NewStringValue(tt.pattern), NewListValue(tt.list)}
			val := builtinRegexpMember(args)
			if !val.IsBool() {
				t.Errorf("regexpMember() did not return boolean, got %v", val.Type())
			} else {
				result, _ := val.BoolValue()
				if result != tt.expected {
					t.Errorf("regexpMember() = %v, want %v", result, tt.expected)
				}
			}
		})
	}
}

func TestRegexps(t *testing.T) {
	tests := []struct {
		name     string
		pattern  string
		target   string
		subst    string
		expected string
	}{
		{
			name:     "simple replace",
			pattern:  "foo",
			target:   "foo bar foo",
			subst:    "baz",
			expected: "baz bar baz",
		},
		{
			name:     "regex pattern",
			pattern:  "\\d+",
			target:   "test123and456",
			subst:    "X",
			expected: "testXandX",
		},
		{
			name:     "no match",
			pattern:  "xyz",
			target:   "hello world",
			subst:    "replacement",
			expected: "hello world",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []Value{NewStringValue(tt.pattern), NewStringValue(tt.target), NewStringValue(tt.subst)}
			val := builtinRegexps(args)
			if !val.IsString() {
				t.Errorf("regexps() did not return string, got %v", val.Type())
			} else {
				result, _ := val.StringValue()
				if result != tt.expected {
					t.Errorf("regexps() = %q, want %q", result, tt.expected)
				}
			}
		})
	}
}

func TestReplace(t *testing.T) {
	tests := []struct {
		name     string
		pattern  string
		target   string
		subst    string
		expected string
	}{
		{
			name:     "replace first",
			pattern:  "foo",
			target:   "foo bar foo",
			subst:    "baz",
			expected: "baz bar foo",
		},
		{
			name:     "regex pattern",
			pattern:  "\\d+",
			target:   "test123and456",
			subst:    "X",
			expected: "testXand456",
		},
		{
			name:     "no match",
			pattern:  "xyz",
			target:   "hello world",
			subst:    "replacement",
			expected: "hello world",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []Value{NewStringValue(tt.pattern), NewStringValue(tt.target), NewStringValue(tt.subst)}
			val := builtinReplace(args)
			if !val.IsString() {
				t.Errorf("replace() did not return string, got %v", val.Type())
			} else {
				result, _ := val.StringValue()
				if result != tt.expected {
					t.Errorf("replace() = %q, want %q", result, tt.expected)
				}
			}
		})
	}
}

func TestReplaceAll(t *testing.T) {
	tests := []struct {
		name     string
		pattern  string
		target   string
		subst    string
		expected string
	}{
		{
			name:     "replace all",
			pattern:  "foo",
			target:   "foo bar foo",
			subst:    "baz",
			expected: "baz bar baz",
		},
		{
			name:     "regex pattern",
			pattern:  "\\d+",
			target:   "test123and456end",
			subst:    "X",
			expected: "testXandXend",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []Value{NewStringValue(tt.pattern), NewStringValue(tt.target), NewStringValue(tt.subst)}
			val := builtinReplaceAll(args)
			if !val.IsString() {
				t.Errorf("replaceAll() did not return string, got %v", val.Type())
			} else {
				result, _ := val.StringValue()
				if result != tt.expected {
					t.Errorf("replaceAll() = %q, want %q", result, tt.expected)
				}
			}
		})
	}
}
