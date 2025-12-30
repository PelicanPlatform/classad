// Package parser provides the ClassAd parser implementation.
// The parser is generated from classad.y using goyacc.
package parser

import (
	"fmt"
	"io"
	"strings"

	"github.com/PelicanPlatform/classad/ast"
)

// Parse parses a ClassAd or expression string and returns the AST.
// For ClassAd-only parsing, use ParseClassAd. For expression-only parsing, use ParseExpr.
func Parse(input string) (ast.Node, error) {
	lex := NewLexer(input)
	yyParse(lex)
	return lex.Result()
}

// ParseClassAd parses a ClassAd and returns a ClassAd AST node.
// Returns an error if the input is not a valid ClassAd (e.g., if it's a bare expression).
func ParseClassAd(input string) (*ast.ClassAd, error) {
	node, err := Parse(input)
	if err != nil {
		return nil, err
	}
	if classad, ok := node.(*ast.ClassAd); ok {
		return classad, nil
	}
	return nil, fmt.Errorf("parsed input is not a ClassAd")
}

// ParseExpr parses an expression string and returns the expression AST.
// Returns an error if the input is not a valid expression.
func ParseExpr(input string) (ast.Expr, error) {
	node, err := Parse(input)
	if err != nil {
		return nil, err
	}
	if expr, ok := node.(ast.Expr); ok {
		return expr, nil
	}
	return nil, fmt.Errorf("parsed input is not an expression")
}

// ReaderParser parses consecutive ClassAds from a buffered reader without
// requiring delimiters between ads. It reuses a single streaming lexer instance
// for efficiency.
type ReaderParser struct {
	lex *StreamingLexer
}

// NewReaderParser creates a reusable parser that pulls consecutive ClassAds
// from the provided reader without requiring delimiters. Non-buffered readers
// are wrapped internally for efficiency.
func NewReaderParser(r io.Reader) *ReaderParser {
	return &ReaderParser{lex: NewStreamingLexer(r)}
}

// ParseClassAd parses the next ClassAd from the underlying reader.
// It reuses the same streaming lexer instance to avoid per-call allocations.
// It returns io.EOF when there is no more data to parse.
func (p *ReaderParser) ParseClassAd() (*ast.ClassAd, error) {
	p.lex.resetForNext()
	if err := p.lex.skipTrivia(); err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, err
	}

	yyParse(p.lex)
	node, err := p.lex.Result()
	if err != nil {
		return nil, err
	}
	if classad, ok := node.(*ast.ClassAd); ok {
		return classad, nil
	}
	return nil, fmt.Errorf("failed to parse ClassAd")
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
