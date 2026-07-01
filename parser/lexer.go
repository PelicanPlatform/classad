// Package parser implements the ClassAd lexer and parser.
package parser

import (
	"bufio"
	"strings"

	"github.com/PelicanPlatform/classad/ast"
)

// Token represents a lexical token.
type Token struct {
	Type int
	Text string
	Pos  int
}

// Lexer wraps the streaming lexer for string inputs while retaining the input
// and position fields used in existing tests.
type Lexer struct {
	input  string
	pos    int
	lex    *StreamingLexer
	result ast.Node
	err    error
}

// NewLexer creates a new lexer for the given input.
func NewLexer(input string) *Lexer {
	br := bufio.NewReader(strings.NewReader(input))
	lex := NewStreamingLexer(br)
	lex.stopAfterClassAd = false
	return &Lexer{
		input: input,
		pos:   0,
		lex:   lex,
	}
}

// Lex implements the goyacc Lexer interface.
func (l *Lexer) Lex(lval *yySymType) int {
	tok := l.lex.Lex(lval)
	// Mirror position for tests.
	l.pos = l.lex.pos
	return tok
}

// Error implements the goyacc Lexer interface.
func (l *Lexer) Error(s string) {
	l.lex.Error(s)
	// Mirror error for tests that inspect Result.
	l.err = l.lex.err
}

// Result returns the parsed result and any error. It must surface an error
// recorded on the underlying streaming lexer even when the parser produced a
// (partial) result -- e.g. trailing input that triggers a lexer error after a
// complete ClassAd, which the reference parser rejects.
func (l *Lexer) Result() (ast.Node, error) {
	res, err := l.lex.Result()
	if res == nil {
		res = l.result
	}
	if err == nil {
		err = l.err
	}
	l.result, l.err = res, err
	return res, err
}

// SetResult sets the parse result.
func (l *Lexer) SetResult(node ast.Node) {
	l.lex.SetResult(node)
	l.result = node
}
