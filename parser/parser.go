// Package parser provides the ClassAd parser implementation.
// The parser is generated from classad.y using goyacc.
package parser

import (
	"fmt"
	"io"
	"strings"

	"github.com/PelicanPlatform/classad/ast"
)

// Parse parses a ClassAd string (a bracketed record, "[ a = 1; ... ]") and
// returns its AST. It does not accept a bare expression; to parse a standalone
// expression such as "a + 1" or "{1, 2}", use ParseExpr.
func Parse(input string) (ast.Node, error) {
	lex := NewLexer(input)
	yyParse(lex)
	return lex.Result()
}

// ParseClassAd parses a ClassAd and returns a ClassAd AST node. It returns an
// error if the input is not a ClassAd (for example a bare expression).
func ParseClassAd(input string) (*ast.ClassAd, error) {
	node, err := Parse(input)
	if err != nil {
		return nil, err
	}
	if classad, ok := node.(*ast.ClassAd); ok {
		return classad, nil
	}
	return nil, fmt.Errorf("parsed input is not a ClassAd, got %T", node)
}

// ParseExpr parses a standalone ClassAd expression (for example "a + 1",
// `strcat("x", "y")`, or "{1, 2, 3}") and returns its expression AST. It returns
// an error if the input is not a single well-formed expression.
//
// The grammar's top-level production is a ClassAd, so the expression is parsed
// inside a throwaway single-attribute wrapper and the attribute's value is
// returned. This keeps Parse ClassAd-only -- avoiding grammar ambiguity between
// a record literal and a bare expression -- while still giving callers direct
// expression access.
func ParseExpr(input string) (ast.Expr, error) {
	ep := exprParserPool.Get().(*exprParser)
	ep.reset(input)
	ep.p.Parse(ep.lex)
	node, err := ep.lex.Result()
	ep.lex.result = nil // do not retain the parsed AST in the pooled instance
	exprParserPool.Put(ep)
	if err != nil {
		return nil, err
	}
	ad, ok := node.(*ast.ClassAd)
	if !ok || len(ad.Attributes) != 1 {
		return nil, fmt.Errorf("input is not a single expression")
	}
	return ad.Attributes[0].Value, nil
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
