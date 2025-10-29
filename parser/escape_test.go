package parser

import (
	"testing"

	"github.com/bbockelm/golang-classads/ast"
)

func TestStringEscapeSequences(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "backspace escape",
			input:    `[s = "\b"]`,
			expected: "\b",
		},
		{
			name:     "tab escape",
			input:    `[s = "\t"]`,
			expected: "\t",
		},
		{
			name:     "newline escape",
			input:    `[s = "\n"]`,
			expected: "\n",
		},
		{
			name:     "formfeed escape",
			input:    `[s = "\f"]`,
			expected: "\f",
		},
		{
			name:     "carriage return escape",
			input:    `[s = "\r"]`,
			expected: "\r",
		},
		{
			name:     "backslash escape",
			input:    `[s = "\\"]`,
			expected: "\\",
		},
		{
			name:     "double quote escape",
			input:    `[s = "\""]`,
			expected: "\"",
		},
		{
			name:     "apostrophe escape",
			input:    `[s = "\'"]`,
			expected: "'",
		},
		{
			name:     "octal 3 digit (leading 0-3)",
			input:    `[s = "\101"]`,
			expected: "A", // 101 octal = 65 decimal = 'A'
		},
		{
			name:     "octal 3 digit leading zero",
			input:    `[s = "\012"]`,
			expected: "\n", // 012 octal = 10 decimal = newline
		},
		{
			name:     "octal 2 digit (leading 4-7)",
			input:    `[s = "\47"]`,
			expected: "'", // 47 octal = 39 decimal = apostrophe
		},
		{
			name:     "octal 2 digit leading 7",
			input:    `[s = "\72"]`,
			expected: ":", // 72 octal = 58 decimal = colon
		},
		{
			name:     "octal truncated by non-digit",
			input:    `[s = "\1x"]`,
			expected: "\x01x", // 1 octal = 1 decimal, then 'x'
		},
		{
			name:     "multiple escapes",
			input:    `[s = "a\nb\tc"]`,
			expected: "a\nb\tc",
		},
		{
			name:     "mixed octal and regular",
			input:    `[s = "a\47\n"]`,
			expected: "a'\n",
		},
		{
			name:     "spec example 1",
			input:    `[s = "a'\n"]`,
			expected: "a'\n", // 97, 39, 10 decimal
		},
		{
			name:     "spec example 2",
			input:    `[s = "a\'\n"]`,
			expected: "a'\n", // Same as above
		},
		{
			name:     "spec example 3",
			input:    `[s = "a\47\012"]`,
			expected: "a'\n", // Same as above using octals
		},
		{
			name:     "spec example 4",
			input:    `[s = "\141\047\012"]`,
			expected: "a'\n", // All octals
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseClassAd(tt.input)
			if err != nil {
				t.Fatalf("Failed to parse: %v", err)
			}

			if len(result.Attributes) != 1 {
				t.Fatalf("Expected 1 attribute, got %d", len(result.Attributes))
			}

			attr := result.Attributes[0]
			if attr.Name != "s" {
				t.Fatalf("Expected attribute name 's', got '%s'", attr.Name)
			}

			strLit, ok := attr.Value.(*ast.StringLiteral)
			if !ok {
				t.Fatalf("Expected StringLiteral, got %T", attr.Value)
			}

			if strLit.Value != tt.expected {
				t.Errorf("Expected %q (bytes: %v), got %q (bytes: %v)",
					tt.expected, []byte(tt.expected),
					strLit.Value, []byte(strLit.Value))
			}
		})
	}
}

func TestStringEscapeErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
		desc  string
	}{
		{
			name:  "null in octal",
			input: `[s = "\0"]`,
			desc:  "octal \\0 should be error (null not allowed)",
		},
		{
			name:  "null in 3-digit octal",
			input: `[s = "\000"]`,
			desc:  "octal \\000 should be error (null not allowed)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseClassAd(tt.input)
			if err == nil {
				t.Errorf("Expected error for %s, but parsing succeeded", tt.desc)
			}
		})
	}
}
