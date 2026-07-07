package parser

import (
	"bufio"
	"io"
	"sync"
)

// The grammar's top production is a ClassAd, so a bare expression is parsed inside
// a throwaway single-attribute record. These pieces are compile-time constants, so
// wrapping allocates nothing.
const (
	exprWrapAttr   = "__classad_parse_expr__"
	exprWrapPrefix = "[" + exprWrapAttr + " = "
	exprWrapSuffix = "]"
)

// wrappedReader streams exprWrapPrefix, the caller's expression text, then
// exprWrapSuffix in sequence, without concatenating them into a new string. It is
// reset per parse so the pooled parser allocates nothing for input handling.
type wrappedReader struct {
	parts [3]string
	stage int
	off   int
}

func (w *wrappedReader) reset(input string) {
	w.parts[0] = exprWrapPrefix
	w.parts[1] = input
	w.parts[2] = exprWrapSuffix
	w.stage = 0
	w.off = 0
}

func (w *wrappedReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) && w.stage < len(w.parts) {
		s := w.parts[w.stage]
		if w.off >= len(s) {
			w.stage++
			w.off = 0
			continue
		}
		c := copy(p[n:], s[w.off:])
		n += c
		w.off += c
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// exprParser bundles the reader, lexer and goyacc parser for one expression parse.
// All of it is reused across calls (the parser re-initializes its inline stack each
// Parse, the bufio.Reader/StreamingLexer are Reset), so a pooled instance turns the
// per-attribute ParseExpr -- previously a fresh bufio.Reader + lexer + parser each
// call -- into an allocation-light hot path.
type exprParser struct {
	wr  wrappedReader
	br  *bufio.Reader
	lex *StreamingLexer
	p   yyParserImpl
}

var exprParserPool = sync.Pool{
	New: func() any {
		ep := &exprParser{}
		ep.br = bufio.NewReader(&ep.wr)
		ep.lex = &StreamingLexer{r: ep.br}
		return ep
	},
}

func (ep *exprParser) reset(input string) {
	ep.wr.reset(input)
	ep.br.Reset(&ep.wr)
	ep.lex.resetForNext()
	ep.lex.pos = 0
	ep.lex.stopAfterClassAd = false
}
