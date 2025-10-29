package classad

import (
	"sort"
	"testing"
)

// TestQuote tests the Quote function
func TestQuote(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple string",
			input:    "hello",
			expected: `"hello"`,
		},
		{
			name:     "string with quotes",
			input:    `value with "quotes"`,
			expected: `"value with \"quotes\""`,
		},
		{
			name:     "string with backslash",
			input:    `path\to\file`,
			expected: `"path\\to\\file"`,
		},
		{
			name:     "string with newline",
			input:    "line1\nline2",
			expected: "\"line1\\nline2\"",
		},
		{
			name:     "string with tab",
			input:    "col1\tcol2",
			expected: "\"col1\\tcol2\"",
		},
		{
			name:     "empty string",
			input:    "",
			expected: `""`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Quote(tt.input)
			if result != tt.expected {
				t.Errorf("Quote(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

// TestUnquote tests the Unquote function
func TestUnquote(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
		wantErr  bool
	}{
		{
			name:     "simple quoted string",
			input:    `"hello"`,
			expected: "hello",
			wantErr:  false,
		},
		{
			name:     "string with escaped quotes",
			input:    `"value with \"quotes\""`,
			expected: `value with "quotes"`,
			wantErr:  false,
		},
		{
			name:     "string with escaped backslash",
			input:    `"path\\to\\file"`,
			expected: `path\to\file`,
			wantErr:  false,
		},
		{
			name:     "string with newline",
			input:    "\"line1\\nline2\"",
			expected: "line1\nline2",
			wantErr:  false,
		},
		{
			name:     "empty quoted string",
			input:    `""`,
			expected: "",
			wantErr:  false,
		},
		{
			name:    "unquoted string",
			input:   "hello",
			wantErr: true,
		},
		{
			name:    "missing closing quote",
			input:   `"hello`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Unquote(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("Unquote(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && result != tt.expected {
				t.Errorf("Unquote(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

// TestQuoteUnquoteRoundTrip tests that Quote and Unquote are inverses
func TestQuoteUnquoteRoundTrip(t *testing.T) {
	testStrings := []string{
		"simple",
		"with spaces",
		`with "quotes"`,
		"with\nnewlines",
		"with\ttabs",
		`complex: "quotes", \backslashes\, and newlines\n`,
	}

	for _, original := range testStrings {
		t.Run(original, func(t *testing.T) {
			quoted := Quote(original)
			unquoted, err := Unquote(quoted)
			if err != nil {
				t.Fatalf("Unquote failed: %v", err)
			}
			if unquoted != original {
				t.Errorf("Round trip failed: %q -> %q -> %q", original, quoted, unquoted)
			}
		})
	}
}

// TestMarshalOld tests the MarshalOld method
func TestMarshalOld(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple classad",
			input:    "[Cpus = 4; Memory = 8192]",
			expected: "Cpus = 4\nMemory = 8192",
		},
		{
			name:     "single attribute",
			input:    "[Name = \"worker-01\"]",
			expected: "Name = \"worker-01\"",
		},
		{
			name:     "with expressions",
			input:    "[x = 10; y = x * 2]",
			expected: "x = 10\ny = (x * 2)",
		},
		{
			name:     "empty classad",
			input:    "[]",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.input)
			if err != nil {
				t.Fatalf("Parse failed: %v", err)
			}
			result := ad.MarshalOld()
			if result != tt.expected {
				t.Errorf("MarshalOld() = %q, want %q", result, tt.expected)
			}
		})
	}
}

// TestOldFormatRoundTrip tests parsing old format and converting back
func TestOldFormatRoundTrip(t *testing.T) {
	original := "Cpus = 4\nMemory = 8192\nName = \"worker\""

	// Parse as old format
	ad, err := ParseOld(original)
	if err != nil {
		t.Fatalf("ParseOld failed: %v", err)
	}

	// Convert back to old format
	result := ad.MarshalOld()

	if result != original {
		t.Errorf("Round trip failed:\nOriginal: %q\nResult:   %q", original, result)
	}
}

// TestExternalRefs tests the ExternalRefs method
func TestExternalRefs(t *testing.T) {
	tests := []struct {
		name     string
		classad  string
		expr     string
		expected []string
	}{
		{
			name:     "single external ref",
			classad:  "[Cpus = 4; Memory = 8192]",
			expr:     "Cpus * 2 + ExternalAttr",
			expected: []string{"ExternalAttr"},
		},
		{
			name:     "multiple external refs",
			classad:  "[x = 10]",
			expr:     "x + y + z",
			expected: []string{"y", "z"},
		},
		{
			name:     "no external refs",
			classad:  "[Cpus = 4; Memory = 8192]",
			expr:     "Cpus * 2 + Memory / 1024",
			expected: []string{},
		},
		{
			name:     "nested expression",
			classad:  "[a = 1; b = 2]",
			expr:     "(a + c) * (b + d)",
			expected: []string{"c", "d"},
		},
		{
			name:     "in function call",
			classad:  "[x = 10]",
			expr:     "ifThenElse(x > y, x, z)",
			expected: []string{"y", "z"},
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

			result := ad.ExternalRefs(expr)

			// Sort for comparison
			sort.Strings(result)
			sort.Strings(tt.expected)

			if len(result) != len(tt.expected) {
				t.Errorf("ExternalRefs() returned %d refs, want %d", len(result), len(tt.expected))
				t.Errorf("Got: %v", result)
				t.Errorf("Want: %v", tt.expected)
				return
			}

			for i := range result {
				if result[i] != tt.expected[i] {
					t.Errorf("ExternalRefs()[%d] = %q, want %q", i, result[i], tt.expected[i])
				}
			}
		})
	}
}

// TestInternalRefs tests the InternalRefs method
func TestInternalRefs(t *testing.T) {
	tests := []struct {
		name     string
		classad  string
		expr     string
		expected []string
	}{
		{
			name:     "single internal ref",
			classad:  "[Cpus = 4; Memory = 8192]",
			expr:     "Cpus * 2 + ExternalAttr",
			expected: []string{"Cpus"},
		},
		{
			name:     "multiple internal refs",
			classad:  "[x = 10; y = 20; z = 30]",
			expr:     "x + y + z",
			expected: []string{"x", "y", "z"},
		},
		{
			name:     "no internal refs",
			classad:  "[Cpus = 4; Memory = 8192]",
			expr:     "UndefinedX + UndefinedY",
			expected: []string{},
		},
		{
			name:     "mixed refs",
			classad:  "[a = 1; b = 2]",
			expr:     "(a + c) * (b + d)",
			expected: []string{"a", "b"},
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

			result := ad.InternalRefs(expr)

			// Sort for comparison
			sort.Strings(result)
			sort.Strings(tt.expected)

			if len(result) != len(tt.expected) {
				t.Errorf("InternalRefs() returned %d refs, want %d", len(result), len(tt.expected))
				t.Errorf("Got: %v", result)
				t.Errorf("Want: %v", tt.expected)
				return
			}

			for i := range result {
				if result[i] != tt.expected[i] {
					t.Errorf("InternalRefs()[%d] = %q, want %q", i, result[i], tt.expected[i])
				}
			}
		})
	}
}

// TestFlatten tests the Flatten method
func TestFlatten(t *testing.T) {
	tests := []struct {
		name     string
		classad  string
		expr     string
		expected string
	}{
		{
			name:     "fully evaluable",
			classad:  "[RequestMemory = 2048]",
			expr:     "RequestMemory * 1024 * 1024",
			expected: "2147483648",
		},
		{
			name:     "partial evaluation",
			classad:  "[x = 10]",
			expr:     "x + y",
			expected: "(10 + y)",
		},
		{
			name:     "no evaluation possible",
			classad:  "[]",
			expr:     "UndefinedX + UndefinedY",
			expected: "(UndefinedX + UndefinedY)",
		},
		{
			name:     "nested evaluation",
			classad:  "[a = 5; b = 3]",
			expr:     "(a + b) * 2",
			expected: "16",
		},
		{
			name:     "conditional with known condition",
			classad:  "[x = 10]",
			expr:     "x > 5 ? 100 : 200",
			expected: "100",
		},
		{
			name:     "conditional with unknown condition",
			classad:  "[]",
			expr:     "y > 5 ? 100 : 200",
			expected: "((y > 5) ? 100 : 200)",
		},
		{
			name:     "mixed evaluation",
			classad:  "[Cpus = 4; Memory = 8192]",
			expr:     "Cpus * 1000 + Memory / 1024 + Unknown",
			expected: "(4008 + Unknown)",
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

			flattened := ad.Flatten(expr)
			result := flattened.String()

			if result != tt.expected {
				t.Errorf("Flatten() = %q, want %q", result, tt.expected)
			}
		})
	}
}

// TestFlattenPreservesSemantics tests that flattened expressions evaluate the same
func TestFlattenPreservesSemantics(t *testing.T) {
	// Create a ClassAd with some defined attributes
	ad, _ := Parse("[x = 10; y = 20; z = 5]")

	// Test expressions that should evaluate to the same value before and after flattening
	exprs := []string{
		"x + y",
		"x * y + z",
		"(x + y) / z",
		"x > 5 ? y : z",
	}

	for _, exprStr := range exprs {
		t.Run(exprStr, func(t *testing.T) {
			expr, err := ParseExpr(exprStr)
			if err != nil {
				t.Fatalf("ParseExpr failed: %v", err)
			}

			// Evaluate original
			originalResult := expr.Eval(ad)

			// Flatten and evaluate
			flattened := ad.Flatten(expr)
			flattenedResult := flattened.Eval(ad)

			// Results should be identical
			if originalResult.Type() != flattenedResult.Type() {
				t.Errorf("Type mismatch after flatten: %v vs %v", originalResult.Type(), flattenedResult.Type())
				return
			}

			// Compare values
			switch originalResult.Type() {
			case IntegerValue:
				origVal, _ := originalResult.IntValue()
				flatVal, _ := flattenedResult.IntValue()
				if origVal != flatVal {
					t.Errorf("Value mismatch: %d vs %d", origVal, flatVal)
				}
			case RealValue:
				origVal, _ := originalResult.RealValue()
				flatVal, _ := flattenedResult.RealValue()
				if origVal != flatVal {
					t.Errorf("Value mismatch: %f vs %f", origVal, flatVal)
				}
			case BooleanValue:
				origVal, _ := originalResult.BoolValue()
				flatVal, _ := flattenedResult.BoolValue()
				if origVal != flatVal {
					t.Errorf("Value mismatch: %v vs %v", origVal, flatVal)
				}
			}
		})
	}
}
