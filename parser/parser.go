// Package parser provides the ClassAd parser implementation.
// The parser is generated from classad.y using goyacc.
package parser

import (
	"strings"

	"github.com/PelicanPlatform/classad/ast"
)

// Parse parses a ClassAd expression string and returns the AST.
func Parse(input string) (ast.Node, error) {
	lex := NewLexer(input)
	yyParse(lex)
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
