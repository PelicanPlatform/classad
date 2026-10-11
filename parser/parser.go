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
	return parsePooled(input, false)
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
// an error if the input is not a single well-formed expression: like libclassad's
// ClassAdParser::ParseExpression with full set, it rejects anything after the
// expression, including a ';'.
func ParseExpr(input string) (ast.Expr, error) {
	return parseExpr(input, false)
}

// ParseExprOld parses a standalone expression the way HTCondor's
// ParseClassAdRvalExpr does (ClassAdParser::SetOldClassAd(true), then a full
// ParseExpression), which reads config macro functions ($INT, $EVAL, ...),
// submit expressions and job_queue.log values. It differs from ParseExpr in two
// ways:
//
//   - Old-ClassAd string literals: an unrecognized escape sequence (e.g.
//     `"a\,\ b"`, the escaped comma and space HTCondor writes into a
//     TransferInput list, or `"\S"` from /etc/issue) is kept literally rather than
//     rejected, as in ParseOldClassAd (see StreamingLexer.lenientEscapes); and
//     adjacent string literals are an error rather than concatenated.
//   - libclassad returns from a top-level conditional (a ? b : c) before it
//     checks for the end of the input, so it ignores whatever follows one:
//     "1 ? 2 : 3 4" is 1 ? 2 : 3. ParseExprOld does the same.
func ParseExprOld(input string) (ast.Expr, error) {
	expr, failAt, err := parseExprAt(input, true)
	if err == nil || failAt <= 0 || failAt >= len(input) {
		return expr, err
	}
	// The parse failed at the token starting at failAt. If what precedes it is
	// a whole top-level conditional, libclassad would have stopped there.
	if prefix, _, perr := parseExprAt(input[:failAt], true); perr == nil {
		if _, ok := prefix.(*ast.ConditionalExpr); ok {
			return prefix, nil
		}
	}
	return nil, err
}

func parseExpr(input string, lenient bool) (ast.Expr, error) {
	expr, _, err := parseExprAt(input, lenient)
	return expr, err
}

// parseExprAt parses input as one expression. On failure it also returns the
// input offset at which the lexer started the token that failed the parse.
func parseExprAt(input string, lenient bool) (ast.Expr, int, error) {
	ep, ok := exprParserPool.Get().(*exprParser)
	if !ok {
		panic("exprParserPool held an unexpected type") // pool's New only makes *exprParser
	}
	ep.reset(input, lenient)
	ep.p.Parse(ep.lex)
	node, err := ep.lex.Result()
	failAt := ep.lex.errAt
	ep.lex.result = nil // do not retain the parsed AST in the pooled instance
	ep.sr.Reset("")
	exprParserPool.Put(ep)
	if err != nil {
		return nil, failAt, err
	}
	expr, ok := node.(ast.Expr)
	if !ok || expr == nil {
		return nil, failAt, fmt.Errorf("input is not a single expression")
	}
	return expr, 0, nil
}

// ReaderParser parses consecutive ClassAds from a buffered reader without
// requiring delimiters between ads. It reuses a single streaming lexer instance
// for efficiency.
type ReaderParser struct {
	lex *StreamingLexer
	// p is reused across ads: yyParse() allocates a fresh parser (with an inline parse
	// stack) per call, which on a multi-ad stream is per-ad garbage for no reason.
	p yyParserImpl
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

	p.p.Parse(p.lex)
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
	// Allocation-free: split on the first '.' with a byte index and compare the
	// prefix case-insensitively (no strings.SplitN slice, no strings.ToUpper copy).
	// The returned name is a sub-slice of identifier. This runs for every attribute
	// reference the parsers emit.
	if dot := strings.IndexByte(identifier, '.'); dot > 0 {
		switch prefix := identifier[:dot]; {
		case strings.EqualFold(prefix, "MY"):
			return identifier[dot+1:], ast.MyScope
		case strings.EqualFold(prefix, "TARGET"):
			return identifier[dot+1:], ast.TargetScope
		case strings.EqualFold(prefix, "PARENT"):
			return identifier[dot+1:], ast.ParentScope
		}
	}
	return identifier, ast.NoScope
}
