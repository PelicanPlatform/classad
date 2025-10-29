// Package parser provides the ClassAd parser implementation.
// The parser is generated from classad.y using goyacc.
package parser

import (
	"github.com/bbockelm/golang-classads/ast"
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
