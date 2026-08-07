package parser

import (
	"bufio"
	"strings"
	"sync"

	"github.com/PelicanPlatform/classad/ast"
)

// adParser bundles the reader, lexer and goyacc parser for one whole-ad parse, pooled the
// way exprParser is for expressions.
//
// Each Parse otherwise allocated a fresh 4 KB bufio.Reader plus a yyParserImpl carrying an
// inline parse stack -- around 7 KB of fixed setup per ad, independent of how small the ad
// is. Decoding rows off the wire is a per-row parse, so that setup, not the parsing, was the
// dominant allocation in every client that reads ads (a 20k-row query allocated ~140 MB in
// bufio buffers and parser stacks alone).
type adParser struct {
	sr  strings.Reader
	br  *bufio.Reader
	slx StreamingLexer
	lex Lexer
	p   yyParserImpl
}

var adParserPool = sync.Pool{
	New: func() any {
		ap := &adParser{}
		ap.br = bufio.NewReader(&ap.sr)
		ap.slx.r = ap.br
		ap.lex.lex = &ap.slx
		return ap
	},
}

// reset points the pooled instance at a new input and clears every field the previous parse
// touched. The whole-ad entry points parse one complete ad from a string, so unlike the
// streaming lexer this one must not stop early or carry state across calls.
func (ap *adParser) reset(input string, lenientEscapes bool) {
	ap.sr.Reset(input)
	ap.br.Reset(&ap.sr)
	ap.slx.resetForNext()
	ap.slx.pos = 0
	ap.slx.stopAfterClassAd = false
	ap.slx.lenientEscapes = lenientEscapes
	ap.lex.input = input
	ap.lex.pos = 0
	ap.lex.result = nil
	ap.lex.err = nil
}

// parsePooled parses one whole ClassAd from input with a pooled parser. lenientEscapes
// selects the old-ClassAd string semantics (see StreamingLexer.lenientEscapes).
func parsePooled(input string, lenientEscapes bool) (ast.Node, error) {
	ap, ok := adParserPool.Get().(*adParser)
	if !ok {
		panic("adParserPool held an unexpected type") // pool's New only makes *adParser
	}
	ap.reset(input, lenientEscapes)
	ap.p.Parse(&ap.lex)
	node, err := ap.lex.Result()
	// Return nothing to the pool that outlives this call: the parsed AST is the caller's,
	// and holding the input string would pin it for as long as the instance is idle.
	ap.slx.result = nil
	ap.lex.result = nil
	ap.lex.input = ""
	ap.sr.Reset("")
	adParserPool.Put(ap)
	return node, err
}
