package parser

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// This file scans numeric literals the way libclassad's Lexer::tokenizeNumber
// does, for both lexers. Its rules, with old ClassAd semantics (which HTCondor
// turns on unless STRICT_CLASSAD_EVALUATION is set, and which the differential
// fuzzer's reference runs with):
//
//   - A '-' where an operand is expected (not after a value, an identifier, '.',
//     or a closing bracket) and directly followed by a digit is part of the
//     literal. Such a signed integer skips the leading-zero check, so "-00" and
//     "-07" are the integers 0 and -7 although "00" and "07" are errors.
//   - "0x" starts a hexadecimal integer, which old semantics reject unless signed;
//     a signed one is read in base 10, so "-0x10" is 0. A hex digit run followed
//     by '.' and a digit is a real that strtod reads as a hex float: 0x1.8 is 1.5.
//   - An integer literal outside the int64 range is 0 ("-9223372036854775808",
//     signed, is INT64_MIN).
//
// The lexers return such a '-' as NEG, and the parser makes NEG and the number
// one primary expression, a unary minus applied to the literal's magnitude,
// which evaluates the same as libclassad's negative literal.

// numCursor feeds scanNumberText one byte at a time from either an in-memory
// source (ByteLexer) or a StreamingLexer.
type numCursor struct {
	src string // in-memory source; unused when sl is set
	pos int
	sl  *StreamingLexer
	buf []byte // the bytes consumed from sl
}

// cur returns the current byte without consuming it, or -1 at end of input or
// on a byte that cannot continue a number.
func (c *numCursor) cur() int {
	if c.sl == nil {
		if c.pos < len(c.src) {
			return int(c.src[c.pos])
		}
		return -1
	}
	r, err := c.sl.peekRune()
	if err != nil || r >= utf8.RuneSelf {
		return -1
	}
	return int(r)
}

// adv consumes the current byte.
func (c *numCursor) adv() {
	if c.sl == nil {
		c.pos++
		return
	}
	r, err := c.sl.peekRune()
	if err != nil {
		return
	}
	if c.sl.discardRune() == nil {
		c.buf = append(c.buf, byte(r))
	}
}

func isHexDigit(c int) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func isDigit(c int) bool { return c >= '0' && c <= '9' }

// scanNumberText consumes the rest of a numeric literal whose first byte (a
// digit, or a '.' followed by a digit) the caller has already consumed. It
// follows Lexer::tokenizeNumber byte for byte and reports whether the literal
// is a real, or a non-empty error message (a format with one verb, for the text
// consumed).
func scanNumberText(first byte, c *numCursor) (isReal bool, errMsg string) {
	const (
		numNone = iota
		numInteger
		numReal
	)
	numberType := numNone
	switch {
	case first == '0':
		if ch := c.cur(); ch == 'x' || ch == 'X' {
			numberType = numInteger
			c.adv()
			if !isHexDigit(c.cur()) {
				return false, "invalid hexadecimal integer: %s"
			}
			for isHexDigit(c.cur()) {
				c.adv()
			}
		} else {
			// Octal or real: an 8 or 9 makes it a real, which then needs a
			// '.' or an exponent.
			numberType = numInteger
			for isDigit(c.cur()) {
				if c.cur() > '7' {
					numberType = numReal
				}
				c.adv()
			}
			if ch := c.cur(); ch == '.' || ch == 'e' || ch == 'E' {
				numberType = numReal
			} else if numberType == numReal {
				return false, "invalid octal integer: %s"
			}
		}
	case isDigit(int(first)):
		for isDigit(c.cur()) {
			c.adv()
		}
		numberType = numInteger
		if ch := c.cur(); ch == '.' || ch == 'e' || ch == 'E' {
			numberType = numReal
		}
	}

	if first == '.' || c.cur() == '.' {
		if c.cur() == '.' {
			c.adv()
		}
		if !isDigit(c.cur()) {
			// Callers start a number on '.' only before a digit, so this is a
			// number with nothing after its '.' ("1.", "0x1.").
			return false, "expected digit after decimal point in %q"
		}
		numberType = numReal
		for isDigit(c.cur()) {
			c.adv()
		}
	}

	if numberType == numReal {
		if ch := c.cur(); ch == 'e' || ch == 'E' {
			c.adv()
			if ch := c.cur(); ch == '+' || ch == '-' {
				c.adv()
			}
			if !isDigit(c.cur()) {
				return false, "invalid real number: %s"
			}
			for isDigit(c.cur()) {
				c.adv()
			}
		}
	}
	return numberType == numReal, ""
}

// numberToken converts the text of a numeric literal scanned by
// scanNumberText into a token, as tokenizeNumber's conversion does. signed
// reports a '-' directly before the literal where an operand is expected.
func numberToken(text string, isReal, signed bool) (kind int, i int64, r float64, err error) {
	if isReal {
		if len(text) > 1 && text[0] == '0' && (text[1] == 'x' || text[1] == 'X') {
			// strtod reads the longest hex-float prefix: hex digits around
			// one '.', with no 'p' exponent (the lexer never takes a 'p').
			end := 2
			dot := false
			for end < len(text) && (isHexDigit(int(text[end])) || (text[end] == '.' && !dot)) {
				dot = dot || text[end] == '.'
				end++
			}
			r, err = strconv.ParseFloat(text[:end]+"p0", 64)
		} else if strings.HasPrefix(text, "..") {
			// "..5": strtod reads no number at all and returns 0.
			r = 0
		} else {
			r, err = strconv.ParseFloat(text, 64)
		}
		// A range error still yields the correctly rounded value (+/-Inf or 0),
		// as strtod does; only a syntax error is fatal.
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return 0, 0, 0, fmt.Errorf("invalid real number: %s", text)
		}
		return REAL_LITERAL, 0, r, nil
	}

	// Old ClassAd semantics have no octal or hex integers: a leading zero
	// (other than a bare "0") is an error, unless the literal is signed.
	if !signed && len(text) > 1 && text[0] == '0' {
		return 0, 0, 0, fmt.Errorf("leading zero in integer literal: %s", text)
	}
	// from_chars in base 10 reads the leading decimal digits ("0x1F" is 0)
	// and leaves the value 0 when they are out of range.
	digits := text
	for n := range len(text) {
		if !isDigit(int(text[n])) {
			digits = text[:n]
			break
		}
	}
	v, perr := strconv.ParseInt(digits, 10, 64)
	if perr != nil {
		if u, uerr := strconv.ParseUint(digits, 10, 64); signed && uerr == nil && u == 1<<63 {
			return INT64_MIN_MAGNITUDE, 0, 0, nil
		}
		v = 0
	}
	return INTEGER_LITERAL, v, 0, nil
}

// endsOperand reports whether a token of this kind ends an operand, after which
// a '-' is the binary operator rather than the sign of a number (the token
// kinds Lexer::LoadToken checks before calling tokenizeNumber).
func endsOperand(kind int) bool {
	switch kind {
	case INTEGER_LITERAL, REAL_LITERAL, BOOLEAN_LITERAL, STRING_LITERAL, UNDEFINED, ERROR,
		IDENTIFIER, INT64_MIN_MAGNITUDE, '.', ']', ')', '}':
		return true
	}
	return false
}

// isCSpace reports whether c is whitespace to libclassad's lexer: C isspace in
// the C locale. No other byte, ASCII or not, separates tokens.
func isCSpace(c rune) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}

// rawByteBase offsets a byte that is not part of valid UTF-8 into a value that
// is not a rune, so the StreamingLexer can carry it through its rune-based
// reader and write the original byte back: libclassad strings are bytes, and
// replacing such a byte with U+FFFD changes the string's value.
const rawByteBase rune = utf8.MaxRune + 1

// writeLexRune appends a rune read by the StreamingLexer, restoring a raw byte.
func writeLexRune(b *strings.Builder, r rune) {
	if r >= rawByteBase {
		b.WriteByte(byte(r - rawByteBase))
		return
	}
	b.WriteRune(r)
}
