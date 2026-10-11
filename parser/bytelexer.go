package parser

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ByteLexer tokenizes a ClassAd expression held entirely in memory as a string,
// indexing directly into that string instead of pulling runes through a
// bufio.Reader. It produces the same LexToken stream as StreamingLexer.Next for
// the same input, but on the hot paths it avoids per-token allocation: an
// identifier or an escape-free string/quoted-name token carries a sub-slice of
// the source (zero copy), numbers are parsed straight from a sub-slice, and no
// per-rune history is recorded (line/column for an error is computed lazily from
// the byte offset). Only a string literal that actually contains a backslash
// escape allocates, to materialize the unescaped value.
//
// It is meant for the wire-native expression parser, which already has the whole
// value as a string. For streaming input (an io.Reader) use StreamingLexer.
type ByteLexer struct {
	src string
	pos int
	err error
	// prevKind is the kind of the last token returned, and signed is set
	// when that token is a NEG before an integer-looking number (number.go).
	prevKind int
	signed   bool
}

// NewByteLexer returns a lexer over src. Drive it with Next.
func NewByteLexer(src string) *ByteLexer { return &ByteLexer{src: src} }

// Reset re-arms the lexer over a new source, reusing the receiver so a caller can
// pool one lexer across many small parses.
func (l *ByteLexer) Reset(src string) {
	l.src = src
	l.pos = 0
	l.err = nil
	l.prevKind = 0
	l.signed = false
}

// Next scans and returns the next token; at end of input its Kind is TokEOF. A
// lexical error (unterminated string, bad escape, malformed number, ...) is
// returned as a non-nil error, matching what the StreamingLexer path reports.
func (l *ByteLexer) Next() (LexToken, error) {
	tok, err := l.next()
	l.prevKind = tok.Kind
	return tok, err
}

func (l *ByteLexer) next() (LexToken, error) {
	l.skipTrivia()
	if l.err != nil {
		return LexToken{}, l.err
	}
	if l.pos >= len(l.src) {
		return LexToken{Kind: TokEOF}, nil
	}

	c := l.src[l.pos]
	switch c {
	case '-':
		// Where an operand is expected, a '-' directly before a number is its
		// sign (NEG), which changes how the number lexes (see number.go).
		l.pos++
		if !endsOperand(l.prevKind) && l.pos < len(l.src) {
			if isASCIIDigit(rune(l.src[l.pos])) {
				l.signed = true
				return LexToken{Kind: NEG}, nil
			}
			if l.src[l.pos] == '.' && l.pos+1 < len(l.src) && isASCIIDigit(rune(l.src[l.pos+1])) {
				return LexToken{Kind: NEG}, nil
			}
		}
		return LexToken{Kind: '-'}, nil
	case '[', ']', '{', '}', '(', ')', ';', ',', ':', '^', '~', '+', '*', '%', '/':
		// '/' reaches here only as division; comments are consumed in skipTrivia.
		l.pos++
		return LexToken{Kind: int(c)}, nil
	case '?':
		// '?' then ':' with only whitespace between is the elvis operator, as
		// in StreamingLexer.
		l.pos++
		for l.pos < len(l.src) && isCSpace(rune(l.src[l.pos])) {
			l.pos++
		}
		if l.pos < len(l.src) && l.src[l.pos] == ':' {
			l.pos++
			return LexToken{Kind: ELVIS}, nil
		}
		return LexToken{Kind: '?'}, nil
	case '.':
		// ".5" begins a fractional float; otherwise '.' is the selection operator.
		// A second '.' is taken with the first: "..5" is a real (0.0) and "a..b"
		// is a.b, as in libclassad and StreamingLexer.
		if l.pos+1 < len(l.src) && isASCIIDigit(rune(l.src[l.pos+1])) {
			return l.scanNumber(l.pos)
		}
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '.' {
			if l.pos+2 < len(l.src) && isASCIIDigit(rune(l.src[l.pos+2])) {
				return l.scanNumber(l.pos)
			}
			l.pos++
		}
		l.pos++
		return LexToken{Kind: '.'}, nil
	case '"':
		return l.scanString()
	case '\'':
		return l.scanQuotedIdentifier()
	case '=':
		// "==", "=?=" (IS), "=!=" (ISNT); a lone '=' otherwise. When "=?" or "=!"
		// is not completed by '=', only the '=' is consumed (the '?' / '!' stays).
		if l.pos+1 < len(l.src) {
			switch l.src[l.pos+1] {
			case '=':
				l.pos += 2
				return LexToken{Kind: EQ}, nil
			case '?':
				if l.pos+2 < len(l.src) && l.src[l.pos+2] == '=' {
					l.pos += 3
					return LexToken{Kind: IS}, nil
				}
			case '!':
				if l.pos+2 < len(l.src) && l.src[l.pos+2] == '=' {
					l.pos += 3
					return LexToken{Kind: ISNT}, nil
				}
			}
		}
		l.pos++
		return LexToken{Kind: '='}, nil
	case '!':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '=' {
			l.pos += 2
			return LexToken{Kind: NE}, nil
		}
		l.pos++
		return LexToken{Kind: '!'}, nil
	case '<':
		if l.pos+1 < len(l.src) {
			switch l.src[l.pos+1] {
			case '=':
				l.pos += 2
				return LexToken{Kind: LE}, nil
			case '<':
				l.pos += 2
				return LexToken{Kind: LSHIFT}, nil
			}
		}
		l.pos++
		return LexToken{Kind: '<'}, nil
	case '>':
		if l.pos+1 < len(l.src) {
			switch l.src[l.pos+1] {
			case '=':
				l.pos += 2
				return LexToken{Kind: GE}, nil
			case '>':
				if l.pos+2 < len(l.src) && l.src[l.pos+2] == '>' {
					l.pos += 3
					return LexToken{Kind: URSHIFT}, nil
				}
				l.pos += 2
				return LexToken{Kind: RSHIFT}, nil
			}
		}
		l.pos++
		return LexToken{Kind: '>'}, nil
	case '&':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '&' {
			l.pos += 2
			return LexToken{Kind: AND}, nil
		}
		l.pos++
		return LexToken{Kind: '&'}, nil
	case '|':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '|' {
			l.pos += 2
			return LexToken{Kind: OR}, nil
		}
		l.pos++
		return LexToken{Kind: '|'}, nil
	}

	if isASCIIDigit(rune(c)) {
		return l.scanNumber(l.pos)
	}
	if isASCIILetter(rune(c)) || c == '_' {
		return l.scanIdentifierOrKeyword()
	}

	// Match StreamingLexer: decode the offending rune for the message.
	r, _ := utf8.DecodeRuneInString(l.src[l.pos:])
	return LexToken{}, l.fail(l.pos, fmt.Sprintf("unexpected character: %c", r))
}

// skipTrivia advances past whitespace and // ... and /* ... */ comments, leaving
// pos at the next significant byte (or end of input, or an unterminated-comment
// error).
func (l *ByteLexer) skipTrivia() {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if isCSpace(rune(c)) {
			l.pos++
			continue
		}
		if c == '/' && l.pos+1 < len(l.src) {
			switch l.src[l.pos+1] {
			case '/':
				l.pos += 2
				for l.pos < len(l.src) && l.src[l.pos] != '\n' {
					l.pos++
				}
				continue
			case '*':
				l.pos += 2
				l.skipBlockComment()
				if l.err != nil {
					return
				}
				continue
			}
		}
		return
	}
}

func (l *ByteLexer) skipBlockComment() {
	for l.pos < len(l.src) {
		if l.src[l.pos] == '*' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '/' {
			l.pos += 2
			return
		}
		l.pos++
	}
	l.err = errors.New(l.formatError(l.pos, "unterminated block comment"))
}

// scanNumber scans an integer or real literal beginning at start (which may be a
// '.' for a leading-dot fraction) and returns the token, parsing the final text
// from a source sub-slice. See number.go.
func (l *ByteLexer) scanNumber(start int) (LexToken, error) {
	signed := l.signed
	l.signed = false
	c := numCursor{src: l.src, pos: start + 1}
	isReal, msg := scanNumberText(l.src[start], &c)
	l.pos = c.pos
	text := l.src[start:c.pos]
	if msg != "" {
		return LexToken{}, l.fail(start, fmt.Sprintf(msg, text))
	}
	kind, i, r, err := numberToken(text, isReal, signed)
	if err != nil {
		return LexToken{}, l.fail(start, err.Error())
	}
	return LexToken{Kind: kind, Int: i, Real: r}, nil
}

// scanIdentifierOrKeyword scans a bare identifier (optionally folding a
// MY./TARGET./PARENT. scope prefix into the token, as StreamingLexer does) and
// reclassifies reserved words. The returned Str for a plain identifier is a
// sub-slice of the source.
func (l *ByteLexer) scanIdentifierOrKeyword() (LexToken, error) {
	start := l.pos
	l.pos++ // first byte already known to be a letter or '_'
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if isASCIILetter(rune(c)) || isASCIIDigit(rune(c)) || c == '_' {
			l.pos++
			continue
		}
		break
	}

	// Scope-prefix fold: MY./TARGET./PARENT. followed by an identifier becomes one
	// token, as in StreamingLexer. Otherwise the '.' stays a separate token.
	word := l.src[start:l.pos]
	if l.pos+1 < len(l.src) && l.src[l.pos] == '.' &&
		(isASCIILetter(rune(l.src[l.pos+1])) || l.src[l.pos+1] == '_') &&
		(strings.EqualFold(word, "MY") || strings.EqualFold(word, "TARGET") || strings.EqualFold(word, "PARENT")) {
		l.pos += 2 // the '.' and the name's first byte
		for l.pos < len(l.src) {
			c := l.src[l.pos]
			if isASCIILetter(rune(c)) || isASCIIDigit(rune(c)) || c == '_' {
				l.pos++
				continue
			}
			break
		}
	}

	scoped := l.src[start:l.pos]
	switch {
	case strings.EqualFold(scoped, "true"):
		return LexToken{Kind: BOOLEAN_LITERAL, Bool: true}, nil
	case strings.EqualFold(scoped, "false"):
		return LexToken{Kind: BOOLEAN_LITERAL, Bool: false}, nil
	case strings.EqualFold(scoped, "undefined"):
		return LexToken{Kind: UNDEFINED}, nil
	case strings.EqualFold(scoped, "error"):
		return LexToken{Kind: ERROR}, nil
	case strings.EqualFold(scoped, "is"):
		return LexToken{Kind: IS}, nil
	case strings.EqualFold(scoped, "isnt"):
		return LexToken{Kind: ISNT}, nil
	}
	return LexToken{Kind: IDENTIFIER, Str: scoped}, nil
}

// scanString scans a double-quoted string literal (pos is at the opening quote)
// and returns a STRING_LITERAL. When the body has no backslash escape the value
// is a zero-copy sub-slice; a backslash switches to a builder to materialize the
// unescaped bytes, matching StreamingLexer.scanString exactly.
func (l *ByteLexer) scanString() (LexToken, error) {
	start := l.pos // at '"'
	i := l.pos + 1
	// Fast path: no escape -> the value is src[i:end].
	for i < len(l.src) {
		c := l.src[i]
		if c == '"' {
			s := l.src[l.pos+1 : i]
			l.pos = i + 1
			// The bytes are kept as they are, valid UTF-8 or not: libclassad
			// strings are bytes.
			return LexToken{Kind: STRING_LITERAL, Str: s}, nil
		}
		if c == '\\' {
			break
		}
		i++
	}
	if i >= len(l.src) {
		l.pos = i
		return LexToken{}, l.fail(start, fmt.Sprintf("unterminated string starting at byte %d", start))
	}

	// Escape path: copy what we have, then decode escapes.
	var b strings.Builder
	b.WriteString(l.src[l.pos+1 : i])
	l.pos = i
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '"' {
			l.pos++
			return LexToken{Kind: STRING_LITERAL, Str: b.String()}, nil
		}
		if c == '\\' {
			l.pos++
			if l.pos >= len(l.src) {
				return LexToken{}, l.fail(start, fmt.Sprintf("unterminated escape sequence in string starting at position %d", start))
			}
			esc := l.src[l.pos]
			l.pos++
			switch esc {
			case 'b':
				b.WriteByte('\b')
			case 't':
				b.WriteByte('\t')
			case 'n':
				b.WriteByte('\n')
			case 'f':
				b.WriteByte('\f')
			case 'r':
				b.WriteByte('\r')
			case '\\':
				b.WriteByte('\\')
			case '"':
				b.WriteByte('"')
			case '\'':
				b.WriteByte('\'')
			case '0', '1', '2', '3', '4', '5', '6', '7':
				if !l.scanOctalEscape(esc, &b) {
					return LexToken{}, l.err
				}
			default:
				// Match the reference: report the bad escape but keep the char.
				l.err = errors.New(l.formatError(l.pos-2, fmt.Sprintf("invalid escape sequence \\%c at position %d", esc, l.pos-2)))
				b.WriteByte(esc)
				return LexToken{}, l.err
			}
			continue
		}
		b.WriteByte(c)
		l.pos++
	}
	return LexToken{}, l.fail(start, fmt.Sprintf("unterminated string starting at byte %d", start))
}

// scanOctalEscape consumes an octal escape (the leading digit already read as
// first) and appends its byte value. It returns false with l.err set on an
// invalid or null (\0) escape, matching StreamingLexer.
func (l *ByteLexer) scanOctalEscape(first byte, b *strings.Builder) bool {
	digits := []byte{first}
	maxDigits := 2
	if first >= '0' && first <= '3' {
		maxDigits = 3
	}
	for len(digits) < maxDigits && l.pos < len(l.src) {
		c := l.src[l.pos]
		if c >= '0' && c <= '7' {
			digits = append(digits, c)
			l.pos++
		} else {
			break
		}
	}
	val, err := strconv.ParseInt(string(digits), 8, 64)
	if err != nil {
		l.err = errors.New(l.formatError(l.pos, fmt.Sprintf("invalid octal escape %s at position %d", digits, l.pos)))
		return false
	}
	if val == 0 {
		l.err = errors.New(l.formatError(l.pos, fmt.Sprintf("null character (\\%s) not allowed in string at position %d", digits, l.pos)))
		return false
	}
	b.WriteByte(byte(val)) // one byte, as convert_escapes writes it
	return true
}

// scanQuotedIdentifier scans a single-quoted attribute name (pos at the opening
// quote) into an IDENTIFIER. A backslash escapes the next byte literally; a
// newline or end of input before the closing quote is an error. Escape-free
// names are a zero-copy sub-slice.
func (l *ByteLexer) scanQuotedIdentifier() (LexToken, error) {
	start := l.pos // at '\''
	i := l.pos + 1
	for i < len(l.src) {
		c := l.src[i]
		if c == '\'' {
			s := l.src[l.pos+1 : i]
			l.pos = i + 1
			return LexToken{Kind: IDENTIFIER, Str: s}, nil
		}
		if c == '\n' {
			l.pos = i
			return LexToken{}, l.fail(start, fmt.Sprintf("newline in quoted attribute name starting at byte %d", start))
		}
		if c == '\\' {
			break
		}
		i++
	}
	if i >= len(l.src) {
		l.pos = i
		return LexToken{}, l.fail(start, fmt.Sprintf("unterminated quoted attribute name starting at byte %d", start))
	}

	var b strings.Builder
	b.WriteString(l.src[l.pos+1 : i])
	l.pos = i
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch c {
		case '\'':
			l.pos++
			return LexToken{Kind: IDENTIFIER, Str: b.String()}, nil
		case '\n':
			return LexToken{}, l.fail(start, fmt.Sprintf("newline in quoted attribute name starting at byte %d", start))
		case '\\':
			l.pos++
			if l.pos >= len(l.src) {
				return LexToken{}, l.fail(start, fmt.Sprintf("unterminated quoted attribute name starting at byte %d", start))
			}
			// The reference reads the escaped rune whole and writes it verbatim.
			_, size := utf8.DecodeRuneInString(l.src[l.pos:])
			b.WriteString(l.src[l.pos : l.pos+size])
			l.pos += size
		default:
			b.WriteByte(c)
			l.pos++
		}
	}
	return LexToken{}, l.fail(start, fmt.Sprintf("unterminated quoted attribute name starting at byte %d", start))
}

// fail records and returns a formatted lexical error at byte offset errPos.
func (l *ByteLexer) fail(errPos int, msg string) error {
	l.err = errors.New(l.formatError(errPos, msg))
	return l.err
}

// formatError renders a parse error with line/column and a caret, computed from
// the source up to errPos. This runs only on the error path, so the hot path
// keeps no per-rune history.
func (l *ByteLexer) formatError(errPos int, msg string) string {
	if errPos > len(l.src) {
		errPos = len(l.src)
	}
	line, col := 1, 0
	lineStart := 0
	for i := 0; i < errPos; {
		r, size := utf8.DecodeRuneInString(l.src[i:])
		if r == '\n' {
			line++
			col = 0
			lineStart = i + size
		} else {
			col++
		}
		i += size
	}
	lineEnd := lineStart
	for lineEnd < len(l.src) && l.src[lineEnd] != '\n' {
		lineEnd++
	}
	lineText := l.src[lineStart:lineEnd]
	caret := strings.Repeat(" ", max(col-1, 0)) + "^"
	return fmt.Sprintf("parse error at line %d, col %d: %s\n%s\n%s", line, col, msg, lineText, caret)
}
