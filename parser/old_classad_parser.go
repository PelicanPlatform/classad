package parser

import (
	"fmt"
	"strings"

	"github.com/bbockelm/golang-classads/ast"
)

// ParseOldClassAd parses a ClassAd in the "old" HTCondor format.
// Old ClassAds have attributes separated by newlines without surrounding brackets.
// Example:
//
//	Foo = 3
//	Bar = "hello"
//	Moo = Foo =!= Undefined
//
// This implementation converts the old format to new format and reuses
// the existing parser.
func ParseOldClassAd(input string) (*ast.ClassAd, error) {
	// Convert old format to new format
	newFormat := convertOldToNewFormat(input)

	// Parse using the standard parser
	result, err := Parse(newFormat)
	if err != nil {
		return nil, fmt.Errorf("error parsing old ClassAd format: %w", err)
	}

	// The result should be a ClassAd
	classAd, ok := result.(*ast.ClassAd)
	if !ok {
		return nil, fmt.Errorf("expected ClassAd, got %T", result)
	}

	return classAd, nil
}

// convertOldToNewFormat converts old ClassAd format to new format
// Old format:
//
//	Foo = 3
//	Bar = "hello"
//
// New format:
//
//	[
//	Foo = 3;
//	Bar = "hello"
//	]
func convertOldToNewFormat(input string) string {
	var result strings.Builder
	result.WriteString("[\n")

	lines := strings.Split(input, "\n")
	inBlockComment := false

	for _, line := range lines {
		// Handle block comments
		if strings.Contains(line, "/*") {
			inBlockComment = true
		}
		if inBlockComment {
			result.WriteString(line)
			result.WriteString("\n")
			if strings.Contains(line, "*/") {
				inBlockComment = false
			}
			continue
		}

		trimmed := strings.TrimSpace(line)

		// Skip empty lines
		if trimmed == "" {
			continue
		}

		// Skip comment-only lines
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#") {
			result.WriteString(line)
			result.WriteString("\n")
			continue
		}

		// Check if line has an assignment operator
		// Old ClassAd lines should be: AttributeName = Expression
		if strings.Contains(trimmed, "=") {
			// Add the line with a semicolon if it doesn't already have one
			if !strings.HasSuffix(trimmed, ";") {
				result.WriteString(trimmed)
				result.WriteString(";\n")
			} else {
				result.WriteString(trimmed)
				result.WriteString("\n")
			}
		} else {
			// Non-assignment line, keep as is (might be part of multi-line expression)
			result.WriteString(line)
			result.WriteString("\n")
		}
	}

	result.WriteString("]")
	return result.String()
}
