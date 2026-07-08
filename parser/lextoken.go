package parser

import "io"

// TokEOF is the Kind of the token Next returns at end of input.
const TokEOF = 0

// LexToken is one lexical token, for a consumer that drives the lexer directly (via
// Next) instead of through the goyacc parser -- e.g. an alternative parser that
// emits a different representation. Kind is a named token constant for
// multi-character tokens:
//
//	IDENTIFIER, STRING_LITERAL, INTEGER_LITERAL, REAL_LITERAL, BOOLEAN_LITERAL,
//	UNDEFINED, ERROR, INT64_MIN_MAGNITUDE, and the operators/keywords
//	OR AND EQ NE IS ISNT LE GE LSHIFT RSHIFT URSHIFT ELVIS
//
// or the byte value of a single-character operator or punctuation
// ('+', '-', '*', '/', '%', '(', ')', '[', ']', '{', '}', '.', ',', ';', '?',
// ':', '<', '>', '&', '|', '^', '~', '!', '=') -- goyacc's single-char-token
// convention. The value carried depends on Kind:
//
//	IDENTIFIER, STRING_LITERAL -> Str
//	INTEGER_LITERAL            -> Int
//	REAL_LITERAL               -> Real
//	BOOLEAN_LITERAL            -> Bool
type LexToken struct {
	Kind int
	Str  string
	Int  int64
	Real float64
	Bool bool
}

// NewExprLexer returns a StreamingLexer configured to tokenize a standalone
// expression: unlike the default lexer it does not stop after a bracketed record,
// so a record literal used as a value ([a = 1]) is lexed all the way through.
// Drive it with Next.
func NewExprLexer(r io.Reader) *StreamingLexer {
	l := NewStreamingLexer(r)
	l.stopAfterClassAd = false
	return l
}

// ResetForExpr re-arms the lexer to tokenize another standalone expression from its
// current reader, reusing internal buffers. Reset the underlying reader (e.g.
// bufio.Reader.Reset) first; this clears the lexer's parse state. It lets a caller
// pool a lexer across many small parses instead of allocating one per parse.
func (l *StreamingLexer) ResetForExpr() {
	l.resetForNext()
	l.pos = 0
	l.stopAfterClassAd = false
}

// Next scans and returns the next token. At end of input its Kind is TokEOF. A
// lexical error (e.g. an unterminated string or bad escape) is returned as a
// non-nil error, matching what the goyacc path would report via Error.
func (l *StreamingLexer) Next() (LexToken, error) {
	var lval yySymType
	kind := l.Lex(&lval)
	if l.err != nil {
		return LexToken{}, l.err
	}
	return LexToken{
		Kind: kind,
		Str:  lval.str,
		Int:  lval.integer,
		Real: lval.real,
		Bool: lval.boolean,
	}, nil
}
