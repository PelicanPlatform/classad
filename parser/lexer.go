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

// Result returns the parsed result and any error.
func (l *Lexer) Result() (ast.Node, error) {
	if l.result != nil || l.err != nil {
		return l.result, l.err
	}
	res, err := l.lex.Result()
	l.result = res
	l.err = err
	return res, err
}

// SetResult sets the parse result.
func (l *Lexer) SetResult(node ast.Node) {
	l.lex.SetResult(node)
	l.result = node
}
