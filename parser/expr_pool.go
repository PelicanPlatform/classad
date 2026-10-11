package parser

import (
	"bufio"
	"strings"
	"sync"
)

// exprParser bundles the reader, lexer and goyacc parser for one expression parse.
// All of it is reused across calls (the parser re-initializes its inline stack each
// Parse, the bufio.Reader/StreamingLexer are Reset), so a pooled instance turns the
// per-attribute ParseExpr -- previously a fresh bufio.Reader + lexer + parser each
// call -- into an allocation-light hot path.
type exprParser struct {
	sr  strings.Reader
	br  *bufio.Reader
	lex *StreamingLexer
	p   yyParserImpl
}

var exprParserPool = sync.Pool{
	New: func() any {
		ep := &exprParser{}
		ep.br = bufio.NewReader(&ep.sr)
		ep.lex = &StreamingLexer{r: ep.br}
		return ep
	},
}

func (ep *exprParser) reset(input string, lenient bool) {
	ep.sr.Reset(input)
	ep.br.Reset(&ep.sr)
	ep.lex.resetForNext()
	ep.lex.pos = 0
	ep.lex.stopAfterClassAd = false
	// The lexer opens with EXPR_START, which selects the grammar's expression
	// start rule: the input must be exactly one expression.
	ep.lex.exprStart = true
	// Set explicitly every reset: resetForNext does not clear it, so a pooled instance
	// reused after a lenient parse would otherwise leak lenience into a strict one.
	ep.lex.lenientEscapes = lenient
	ep.lex.noStringConcat = lenient
}
