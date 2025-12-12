// Package parser provides the ClassAd parser implementation.
// The parser is generated from classad.y using goyacc.
package parser

import (
	"strings"

	"github.com/PelicanPlatform/classad/ast"
)

// Parse parses a ClassAd expression string and returns the AST.
// For backward compatibility, if the input contains a single ClassAd,
// it returns that ClassAd. If it contains multiple ClassAds, it returns
// the first one.
func Parse(input string) (ast.Node, error) {
	lex := NewLexer(input)
	yyParse(lex)

	// Check if we got a list of ClassAds
	if list, err := lex.ResultList(); err == nil && len(list) > 0 {
		// For backward compatibility, return the first ClassAd
		return list[0], nil
	}

	// Fall back to single result (for backward compatibility)
	return lex.Result()
}

// ParseClassAd parses a ClassAd and returns a ClassAd AST node.
func ParseClassAd(input string) (*ast.ClassAd, error) {
	node, err := Parse(input)
	if err != nil {
		return nil, err
	}
	if classad, ok := node.(*ast.ClassAd); ok {
		return classad, nil
	}
	return nil, nil
}

// ParseMultipleClassAds parses a string containing one or more concatenated ClassAds
// (e.g., "][" without whitespace) and returns a list of ClassAd AST nodes.
// This function handles the HTCondor format where ClassAds may be concatenated
// without whitespace between them.
func ParseMultipleClassAds(input string) ([]*ast.ClassAd, error) {
	lex := NewLexer(input)
	yyParse(lex)
	return lex.ResultList()
}

// ParseScopedIdentifier parses an identifier that may have a scope prefix.
// Returns the attribute name and scope.
func ParseScopedIdentifier(identifier string) (string, ast.AttributeScope) {
	parts := strings.SplitN(identifier, ".", 2)
	if len(parts) == 2 {
		scopeStr := strings.ToUpper(parts[0])
		attrName := parts[1]
		switch scopeStr {
		case "MY":
			return attrName, ast.MyScope
		case "TARGET":
			return attrName, ast.TargetScope
		case "PARENT":
			return attrName, ast.ParentScope
		}
	}
	return identifier, ast.NoScope
}
