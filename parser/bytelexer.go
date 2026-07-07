package parser

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
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
}

// NewByteLexer returns a lexer over src. Drive it with Next.
func NewByteLexer(src string) *ByteLexer { return &ByteLexer{src: src} }

// Reset re-arms the lexer over a new source, reusing the receiver so a caller can
// pool one lexer across many small parses.
func (l *ByteLexer) Reset(src string) {
	l.src = src
	l.pos = 0
	l.err = nil
}

// Next scans and returns the next token; at end of input its Kind is TokEOF. A
// lexical error (unterminated string, bad escape, malformed number, ...) is
// returned as a non-nil error, matching what the StreamingLexer path reports.
func (l *ByteLexer) Next() (LexToken, error) {
	l.skipTrivia()
	if l.err != nil {
		return LexToken{}, l.err
	}
	if l.pos >= len(l.src) {
		return LexToken{Kind: TokEOF}, nil
	}

	c := l.src[l.pos]
	switch c {
	case '[', ']', '{', '}', '(', ')', ';', ',', ':', '^', '~', '+', '-', '*', '%', '/':
		// '/' reaches here only as division; comments are consumed in skipTrivia.
		l.pos++
		return LexToken{Kind: int(c)}, nil
	case '?':
		// Adjacent "?:" is the elvis operator; a spaced "? :" stays two tokens.
		l.pos++
		if l.pos < len(l.src) && l.src[l.pos] == ':' {
			l.pos++
			return LexToken{Kind: ELVIS}, nil
		}
		return LexToken{Kind: '?'}, nil
	case '.':
		// ".5" begins a fractional float; otherwise '.' is the selection operator.
		if l.pos+1 < len(l.src) && isASCIIDigit(rune(l.src[l.pos+1])) {
			return l.scanNumber(l.pos)
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
		if c < utf8.RuneSelf {
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v' {
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
		// Non-ASCII: honor unicode.IsSpace for parity with StreamingLexer.
		r, size := utf8.DecodeRuneInString(l.src[l.pos:])
		if unicode.IsSpace(r) {
			l.pos += size
			continue
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
// '.' for a leading-dot fraction) and returns the token. It mirrors
// StreamingLexer.scanNumber, parsing the final text from a source sub-slice.
func (l *ByteLexer) scanNumber(start int) (LexToken, error) {
	i := start
	hasDecimal := l.src[i] == '.'
	hasExponent := false
	i++ // consume the first byte (digit or leading '.')

	for i < len(l.src) {
		c := l.src[i]
		if isASCIIDigit(rune(c)) {
			i++
			continue
		}
		if c == '.' && !hasDecimal && !hasExponent {
			hasDecimal = true
			i++
			// A digit must follow the decimal point ("1." and "1.e5" are errors).
			if i >= len(l.src) || !isASCIIDigit(rune(l.src[i])) {
				l.pos = i
				return LexToken{}, l.fail(start, fmt.Sprintf("expected digit after decimal point in %q", l.src[start:i]))
			}
			continue
		}
		if (c == 'e' || c == 'E') && !hasExponent {
			hasExponent = true
			hasDecimal = true
			i++
			if i < len(l.src) && (l.src[i] == '+' || l.src[i] == '-') {
				i++
			}
			continue
		}
		break
	}

	text := l.src[start:i]
	l.pos = i

	if hasDecimal || hasExponent {
		val, err := strconv.ParseFloat(text, 64)
		// A range error still yields the correctly-rounded value (+/-Inf or 0),
		// matching the reference's strtod; only a syntax error is fatal.
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return LexToken{}, l.fail(start, fmt.Sprintf("invalid real number: %s", text))
		}
		return LexToken{Kind: REAL_LITERAL, Real: val}, nil
	}

	// A leading zero (other than a bare "0") is rejected, not read as octal.
	if len(text) > 1 && text[0] == '0' {
		return LexToken{}, l.fail(start, fmt.Sprintf("leading zero in integer literal: %s", text))
	}

	val, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		// 2^63 is the magnitude of INT64_MIN: emit a dedicated token so the caller
		// can fold '-' 2^63 into INT64_MIN. A bare 2^63 stays a syntax error there.
		if u, uerr := strconv.ParseUint(text, 10, 64); uerr == nil && u == 1<<63 {
			return LexToken{Kind: INT64_MIN_MAGNITUDE}, nil
		}
		return LexToken{}, l.fail(start, fmt.Sprintf("invalid integer: %s", text))
	}
	return LexToken{Kind: INTEGER_LITERAL, Int: val}, nil
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
	// token. Matching StreamingLexer, the '.' is consumed once the prefix matches
	// even if what follows is not an identifier char (a benign quirk preserved for
	// byte-identical behavior).
	word := l.src[start:l.pos]
	if l.pos < len(l.src) && l.src[l.pos] == '.' &&
		(strings.EqualFold(word, "MY") || strings.EqualFold(word, "TARGET") || strings.EqualFold(word, "PARENT")) {
		l.pos++ // consume '.'
		if l.pos < len(l.src) && (isASCIILetter(rune(l.src[l.pos])) || l.src[l.pos] == '_') {
			l.pos++
			for l.pos < len(l.src) {
				c := l.src[l.pos]
				if isASCIILetter(rune(c)) || isASCIIDigit(rune(c)) || c == '_' {
					l.pos++
					continue
				}
				break
			}
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
			// The reference reads the body through bufio.ReadRune, which maps each
			// invalid UTF-8 byte to U+FFFD; normalize to stay byte-identical (valid
			// UTF-8 stays a zero-copy sub-slice).
			if !utf8.ValidString(s) {
				s = normalizeUTF8(s)
			}
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
	writeNormalized(&b, l.src[l.pos+1:i])
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
		// Decode a full rune (invalid byte -> U+FFFD) to match ReadRune/WriteRune.
		r, size := utf8.DecodeRuneInString(l.src[l.pos:])
		b.WriteRune(r)
		l.pos += size
	}
	return LexToken{}, l.fail(start, fmt.Sprintf("unterminated string starting at byte %d", start))
}

// normalizeUTF8 re-encodes s rune by rune, mapping each invalid UTF-8 byte to
// U+FFFD exactly as bufio.ReadRune + WriteRune does in the streaming lexer. Valid
// UTF-8 is returned unchanged (callers avoid it via a utf8.ValidString check).
func normalizeUTF8(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	writeNormalized(&b, s)
	return b.String()
}

// writeNormalized appends s to b, replacing each invalid UTF-8 byte with U+FFFD
// (bufio.ReadRune + WriteRune semantics). Valid UTF-8 is appended verbatim.
func writeNormalized(b *strings.Builder, s string) {
	if utf8.ValidString(s) {
		b.WriteString(s)
		return
	}
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		b.WriteRune(r)
		i += size
	}
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
	b.WriteRune(rune(val))
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
			if !utf8.ValidString(s) {
				s = normalizeUTF8(s)
			}
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
	writeNormalized(&b, l.src[l.pos+1:i])
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
			r, size := utf8.DecodeRuneInString(l.src[l.pos:])
			b.WriteRune(r)
			l.pos += size
		default:
			r, size := utf8.DecodeRuneInString(l.src[l.pos:])
			b.WriteRune(r)
			l.pos += size
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
