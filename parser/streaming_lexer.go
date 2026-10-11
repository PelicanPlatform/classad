package parser

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/PelicanPlatform/classad/ast"
)

// StreamingLexer tokenizes ClassAds directly from an io.Reader. It stops
// producing tokens after the first complete ClassAd so the caller can parse
// multiple ads from a single stream.
type StreamingLexer struct {
	r                *bufio.Reader
	pos              int
	result           ast.Node
	err              error
	depth            int
	started          bool
	done             bool
	stopAfterClassAd bool
	pendingRune      rune
	pendingSize      int
	hasPending       bool
	seen             []rune
	// lenientEscapes selects old-ClassAd string and quoted-name lexing (scanStringOld,
	// C++ Lexer::tokenizeStringOld), which does no escape processing at all -- so a
	// value like OSIssue = "\S" (agetty escapes from /etc/issue) round-trips instead of
	// failing the whole ad. Set only on the old-ClassAd parse path.
	lenientEscapes bool
	// exprStart makes the next token the grammar's EXPR_START, so the input is
	// parsed as one standalone expression rather than a ClassAd.
	exprStart bool
	// noStringConcat rejects adjacent string literals, which old ClassAd syntax
	// does not concatenate (Lexer::tokenizeStringOld has no such step, unlike
	// tokenizeString). Set only by ParseExprOld.
	noStringConcat bool
	// prevTok is the kind of the last token returned; a '-' after an operand is
	// the binary operator, elsewhere the sign of a following number.
	prevTok int
	// signedNum is set when the NEG just returned signs an integer-looking
	// number, which then lexes by different rules (see number.go).
	signedNum bool
	// tokStart is the input offset at which the last Lex call started, and
	// errAt is tokStart when the first error was recorded: the start of the
	// token that failed the parse.
	tokStart int
	errAt    int
}

// NewStreamingLexer creates a lexer that consumes tokens directly from a reader.
// It wraps non-buffered readers in a bufio.Reader for efficiency.
func NewStreamingLexer(r io.Reader) *StreamingLexer {
	if br, ok := r.(*bufio.Reader); ok {
		return &StreamingLexer{r: br, stopAfterClassAd: true}
	}
	return &StreamingLexer{r: bufio.NewReader(r), stopAfterClassAd: true}
}

// resetForNext prepares the lexer to scan another ClassAd from the same reader.
// It preserves the current reader position but clears parsing state and result.
func (l *StreamingLexer) resetForNext() {
	l.result = nil
	l.err = nil
	l.depth = 0
	l.started = false
	l.done = false
	l.pendingRune = 0
	l.pendingSize = 0
	l.hasPending = false
	l.seen = l.seen[:0]
	l.prevTok = 0
	l.signedNum = false
	l.exprStart = false
	l.noStringConcat = false
}

// isASCIILetter and isASCIIDigit define the character classes for identifiers
// and numeric literals. The reference lexer is ASCII-only, so a Unicode letter
// or digit (e.g. "ǒ") is not a valid identifier/number character even though
// unicode.IsLetter/IsDigit would accept it.
func isASCIILetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isASCIIDigit(r rune) bool {
	return r >= '0' && r <= '9'
}

// Lex implements the goyacc Lexer interface.
func (l *StreamingLexer) Lex(lval *yySymType) int {
	l.tokStart = l.pos
	if l.exprStart {
		l.exprStart = false
		l.prevTok = EXPR_START
		return EXPR_START
	}
	tok := l.lex(lval)
	if tok == STRING_LITERAL && l.prevTok == STRING_LITERAL && l.noStringConcat && l.err == nil {
		l.Error("adjacent string literals")
		tok = 0
	}
	l.prevTok = tok
	return tok
}

func (l *StreamingLexer) lex(lval *yySymType) int {
	if l.done {
		return 0
	}

	if err := l.skipTrivia(); err != nil {
		if err == io.EOF {
			l.done = true
			return 0
		}
		l.err = err
		return 0
	}

	ch, _, err := l.readRune()
	if err != nil {
		if err == io.EOF {
			l.done = true
			if l.started && l.depth > 0 {
				l.Error("unexpected EOF while parsing ClassAd")
			}
			return 0
		}
		l.err = err
		return 0
	}

	// Check for operators and punctuation
	switch ch {
	case '[':
		l.started = true
		l.depth++
		return int('[')
	case ']':
		if l.depth > 0 {
			l.depth--
		}
		if l.stopAfterClassAd && l.started && l.depth == 0 {
			// Signal EOF after this token so the parser stops at the first ClassAd.
			l.done = true
		}
		return int(']')
	case '{':
		return int('{')
	case '}':
		return int('}')
	case '(':
		return int('(')
	case ')':
		return int(')')
	case ';':
		return int(';')
	case ',':
		return int(',')
	case '?':
		// A '?' followed by a ':', with only whitespace between them, is the
		// high-precedence elvis operator; libclassad's lexer skips the
		// whitespace (HTCONDOR-3814). A comment between them leaves two tokens.
		for {
			next, err := l.peekRune()
			if err != nil || !isCSpace(next) {
				break
			}
			if err := l.discardRune(); err != nil {
				l.err = err
				return 0
			}
		}
		if next, err := l.peekRune(); err == nil && next == ':' {
			if err := l.discardRune(); err != nil {
				l.err = err
				return 0
			}
			return ELVIS
		}
		return int('?')
	case ':':
		return int(':')
	case '^':
		return int('^')
	case '~':
		return int('~')
	case '+':
		return int('+')
	case '-':
		// Where an operand is expected, a '-' directly before a number is its
		// sign (NEG), which changes how the number lexes (see number.go).
		if !endsOperand(l.prevTok) {
			if next, err := l.peekRune(); err == nil && isASCIIDigit(next) {
				l.signedNum = true
				return NEG
			} else if err == nil && next == '.' {
				if after, perr := l.r.Peek(1); perr == nil && isASCIIDigit(rune(after[0])) {
					return NEG
				}
			}
		}
		return int('-')
	case '*':
		return int('*')
	case '%':
		return int('%')
	case '.':
		// A '.' immediately before a digit begins a fractional float literal
		// (".5"), which the reference accepts; otherwise it is the selection
		// operator. Like libclassad, a second '.' is taken with the first:
		// before a digit the two begin a real that strtod reads as 0.0 ("..5"),
		// and otherwise they are one selection operator ("a..b" is a.b).
		if next, err := l.peekRune(); err == nil && isASCIIDigit(next) {
			return l.scanNumber('.', lval)
		} else if err == nil && next == '.' {
			if after, perr := l.r.Peek(1); perr == nil && isASCIIDigit(rune(after[0])) {
				return l.scanNumber('.', lval)
			}
			if err := l.discardRune(); err != nil {
				l.err = err
				return 0
			}
		}
		return int('.')
	case '"':
		str := l.scanString()
		lval.str = str
		return STRING_LITERAL
	case '\'':
		if l.lenientEscapes {
			lval.str = l.scanStringOld('\'')
			if l.err != nil {
				return 0
			}
			return IDENTIFIER
		}
		return l.scanQuotedIdentifier(lval)
	case '=':
		if next, err := l.peekRune(); err == nil {
			switch next {
			case '=':
				if err := l.discardRune(); err != nil {
					l.err = err
					return 0
				}
				return EQ
			case '?':
				if err := l.discardRune(); err != nil {
					l.err = err
					return 0
				}
				// peekRuneRaw (not peekRune): a non-'=' here must leave nothing
				// staged as pending, or the following unreadRune('?') would
				// clobber it and silently drop the peeked character.
				if peek, err := l.peekRuneRaw(); err == nil && peek == '=' {
					if err := l.discardRune(); err != nil {
						l.err = err
						return 0
					}
					return IS
				}
				// Put back the '?' by unread one rune
				l.unreadRune(utf8.RuneLen('?'))
				return int('=')
			case '!':
				if err := l.discardRune(); err != nil {
					l.err = err
					return 0
				}
				// peekRuneRaw (not peekRune): a non-'=' here must leave nothing
				// staged as pending, or the following unreadRune('!') would
				// clobber it and silently drop the peeked character (so e.g.
				// "x=!10" lexed as "x = !0", evaluating !10 to true).
				if peek, err := l.peekRuneRaw(); err == nil && peek == '=' {
					if err := l.discardRune(); err != nil {
						l.err = err
						return 0
					}
					return ISNT
				}
				l.unreadRune(utf8.RuneLen('!'))
				return int('=')
			}
		}
		return int('=')
	case '!':
		if next, err := l.peekRune(); err == nil && next == '=' {
			if err := l.discardRune(); err != nil {
				l.err = err
				return 0
			}
			return NE
		}
		return int('!')
	case '<':
		if next, err := l.peekRune(); err == nil {
			switch next {
			case '=':
				if err := l.discardRune(); err != nil {
					l.err = err
					return 0
				}
				return LE
			case '<':
				if err := l.discardRune(); err != nil {
					l.err = err
					return 0
				}
				return LSHIFT
			}
		}
		return int('<')
	case '>':
		if next, err := l.peekRune(); err == nil {
			switch next {
			case '=':
				if err := l.discardRune(); err != nil {
					l.err = err
					return 0
				}
				return GE
			case '>':
				if err := l.discardRune(); err != nil {
					l.err = err
					return 0
				}
				if peek, err := l.peekRune(); err == nil && peek == '>' {
					if err := l.discardRune(); err != nil {
						l.err = err
						return 0
					}
					return URSHIFT
				}
				return RSHIFT
			}
		}
		return int('>')
	case '&':
		if next, err := l.peekRune(); err == nil && next == '&' {
			if err := l.discardRune(); err != nil {
				l.err = err
				return 0
			}
			return AND
		}
		return int('&')
	case '|':
		if next, err := l.peekRune(); err == nil && next == '|' {
			if err := l.discardRune(); err != nil {
				l.err = err
				return 0
			}
			return OR
		}
		return int('|')
	case '/':
		if next, err := l.peekRune(); err == nil {
			switch next {
			case '/':
				if err := l.discardRune(); err != nil {
					l.err = err
					return 0
				}
				if err := l.skipLineComment(); err != nil {
					l.err = err
					return 0
				}
				return l.lex(lval)
			case '*':
				if err := l.discardRune(); err != nil {
					l.err = err
					return 0
				}
				if err := l.skipBlockComment(); err != nil {
					l.err = err
					return 0
				}
				return l.lex(lval)
			}
		}
		return int('/')
	}

	// Numbers
	if isASCIIDigit(ch) {
		return l.scanNumber(ch, lval)
	}

	// Identifiers and keywords
	if isASCIILetter(ch) || ch == '_' {
		return l.scanIdentifierOrKeyword(ch, lval)
	}

	// Unknown character: report the error and stop (return EOF) rather than
	// silently skipping it and lexing on, which would accept malformed input
	// like "[#]" that the reference parser rejects.
	l.Error(fmt.Sprintf("unexpected character: %c", ch))
	return 0
}

// Error implements the goyacc Lexer interface.
func (l *StreamingLexer) Error(s string) {
	if l.err == nil {
		l.errAt = l.tokStart
	}
	l.err = errors.New(l.formatError(s))
}

// Result returns the parsed result and any error.
func (l *StreamingLexer) Result() (ast.Node, error) {
	return l.result, l.err
}

// SetResult sets the parse result.
func (l *StreamingLexer) SetResult(node ast.Node) {
	l.result = node
}

func (l *StreamingLexer) readRune() (rune, int, error) {
	if l.hasPending {
		ch := l.pendingRune
		size := l.pendingSize
		l.hasPending = false
		l.recordRune(ch, size)
		return ch, size, nil
	}

	ch, size, err := l.readRawRune()
	if err != nil {
		return 0, 0, err
	}
	l.recordRune(ch, size)
	return ch, size, nil
}

// readRawRune reads the next rune from the reader. A byte that is not part of
// valid UTF-8 comes back as rawByteBase+byte rather than U+FFFD, so a string
// literal keeps it.
func (l *StreamingLexer) readRawRune() (rune, int, error) {
	ch, size, err := l.r.ReadRune()
	if err == nil && ch == utf8.RuneError && size == 1 {
		if uerr := l.r.UnreadRune(); uerr != nil {
			return 0, 0, uerr
		}
		b, berr := l.r.ReadByte()
		if berr != nil {
			return 0, 0, berr
		}
		return rawByteBase + rune(b), 1, nil
	}
	return ch, size, err
}

func (l *StreamingLexer) unreadRune(size int) {
	if len(l.seen) == 0 {
		return
	}
	last := l.seen[len(l.seen)-1]
	l.seen = l.seen[:len(l.seen)-1]
	l.pos -= size
	l.pendingRune = last
	l.pendingSize = size
	l.hasPending = true
}

func (l *StreamingLexer) peekRune() (rune, error) {
	if l.hasPending {
		return l.pendingRune, nil
	}

	ch, size, err := l.readRawRune()
	if err != nil {
		return 0, err
	}
	// Do not advance pos/seen yet; stage as pending.
	l.pendingRune = ch
	l.pendingSize = size
	l.hasPending = true
	return ch, nil
}

// peekRuneRaw returns the next rune without consuming it and without using the
// single-slot pending buffer. It is needed where a rune already needs to be
// pushed back into the pending slot but we still want to look one further
// ahead (e.g. distinguishing the '/' operator from a "//" or "/*" comment in
// skipTrivia); using the pending-based peekRune there would clobber the rune
// being pushed back and silently drop a character such as the divisor in
// "1/2".
func (l *StreamingLexer) peekRuneRaw() (rune, error) {
	if l.hasPending {
		return l.pendingRune, nil
	}
	ch, _, err := l.readRawRune()
	if err != nil {
		return 0, err
	}
	unread := l.r.UnreadRune
	if ch >= rawByteBase {
		unread = l.r.UnreadByte
	}
	if uerr := unread(); uerr != nil {
		return 0, uerr
	}
	return ch, nil
}

// discardRune consumes a rune and returns any read error.
func (l *StreamingLexer) discardRune() error {
	_, _, err := l.readRune()
	return err
}

func (l *StreamingLexer) skipTrivia() error {
	for {
		ch, size, err := l.readRune()
		if err != nil {
			return err
		}

		if isCSpace(ch) {
			continue
		}

		if ch == '/' {
			next, err := l.peekRuneRaw()
			if err == nil {
				switch next {
				case '/':
					// Consume next '/'
					if err := l.discardRune(); err != nil {
						return err
					}
					if err := l.skipLineComment(); err != nil {
						return err
					}
					continue
				case '*':
					if err := l.discardRune(); err != nil {
						return err
					}
					if err := l.skipBlockComment(); err != nil {
						return err
					}
					continue
				}
			}
		}

		// Non-trivia rune; stage it for the lexer without consuming it.
		l.pendingRune = ch
		l.pendingSize = size
		l.hasPending = true
		// We recorded this rune in readRune, so roll back the position and seen to
		// reflect that it is not yet consumed by the parser.
		l.pos -= size
		if len(l.seen) > 0 {
			l.seen = l.seen[:len(l.seen)-1]
		}
		return nil
	}
}

func (l *StreamingLexer) skipLineComment() error {
	for {
		ch, _, err := l.readRune()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if ch == '\n' {
			return nil
		}
	}
}

func (l *StreamingLexer) skipBlockComment() error {
	for {
		ch, _, err := l.readRune()
		if err != nil {
			if err == io.EOF {
				l.Error("unterminated block comment")
			}
			return err
		}
		if ch == '*' {
			next, err := l.peekRune()
			if err == nil && next == '/' {
				if err := l.discardRune(); err != nil {
					return err
				}
				return nil
			}
		}
	}
}

func (l *StreamingLexer) scanString() string {
	if l.lenientEscapes {
		return l.scanStringOld('"')
	}
	var result strings.Builder
	startPos := l.pos - utf8.RuneLen('"')

	for {
		ch, _, err := l.readRune()
		if err != nil {
			if err == io.EOF {
				l.Error(fmt.Sprintf("unterminated string starting at byte %d", startPos))
			}
			return result.String()
		}

		if ch == '"' {
			return result.String()
		}

		if ch == '\\' {
			escaped, _, err := l.readRune()
			if err != nil {
				l.Error(fmt.Sprintf("unterminated escape sequence in string starting at position %d", startPos))
				return result.String()
			}
			switch escaped {
			case 'b':
				result.WriteRune('\b')
			case 't':
				result.WriteRune('\t')
			case 'n':
				result.WriteRune('\n')
			case 'f':
				result.WriteRune('\f')
			case 'r':
				result.WriteRune('\r')
			case '\\':
				result.WriteRune('\\')
			case '"':
				result.WriteRune('"')
			case '\'':
				result.WriteRune('\'')
			case '0', '1', '2', '3', '4', '5', '6', '7':
				var octalStr strings.Builder
				octalStr.WriteRune(escaped)

				maxDigits := 2
				if escaped >= '0' && escaped <= '3' {
					maxDigits = 3
				}

				for i := 1; i < maxDigits; i++ {
					next, err := l.peekRune()
					if err != nil {
						break
					}
					if next >= '0' && next <= '7' {
						if err := l.discardRune(); err != nil {
							return result.String()
						}
						octalStr.WriteRune(next)
					} else {
						break
					}
				}

				val, err := strconv.ParseInt(octalStr.String(), 8, 64)
				if err != nil {
					l.Error(fmt.Sprintf("invalid octal escape %s at position %d", octalStr.String(), l.pos))
					return result.String()
				}
				if val == 0 {
					l.Error(fmt.Sprintf("null character (\\%s) not allowed in string at position %d", octalStr.String(), l.pos))
					return result.String()
				}
				result.WriteByte(byte(val)) // one byte, as convert_escapes writes it
			default:
				l.Error(fmt.Sprintf("invalid escape sequence \\%c at position %d", escaped, l.pos-2))
				writeLexRune(&result, escaped)
			}
			continue
		}

		writeLexRune(&result, ch)
	}
}

// scanStringOld scans the rest of a string literal (delim is a double quote)
// or a quoted attribute name (a single quote) in old ClassAd syntax, as C++
// Lexer::tokenizeStringOld does. No escape sequence is interpreted (\S \n \t
// stay literal, backslash and all), except that a backslash immediately before
// the delimiter escapes it: the delimiter is kept and the literal continues --
// unless that delimiter is the last thing on the line or in the input, where
// the literal ends there, with the backslash as its last byte. One backslash
// is processed at a time (peek, don't consume the next rune), so \\" reads as
// \ then an escaped ".
func (l *StreamingLexer) scanStringOld(delim rune) string {
	var result strings.Builder
	startPos := l.pos - 1
	for {
		ch, _, err := l.readRune()
		if err != nil {
			if err == io.EOF {
				l.Error(fmt.Sprintf("unterminated string starting at byte %d", startPos))
			}
			return result.String()
		}
		switch ch {
		case delim:
			return result.String()
		case '\\':
			if next, perr := l.peekRune(); perr == nil && next == delim {
				if err := l.discardRune(); err != nil {
					return result.String()
				}
				if after, aerr := l.peekRune(); aerr != nil || after == '\n' {
					if aerr == nil {
						if err := l.discardRune(); err != nil { // the newline
							return result.String()
						}
					}
					result.WriteByte('\\')
					return result.String()
				}
				result.WriteRune(delim)
			} else {
				result.WriteByte('\\')
			}
		default:
			writeLexRune(&result, ch)
		}
	}
}

// scanQuotedIdentifier scans a single-quoted attribute name (the opening quote
// has already been consumed) and returns it as an IDENTIFIER token. Unlike a
// bare identifier, a quoted name may contain spaces and reserved words
// ('true', 'a b') and is never reinterpreted as a keyword, matching the
// reference engine. A backslash escapes the following character (so 'a\'b' is
// the name a'b); a newline or end of input before the closing quote is an
// error.
func (l *StreamingLexer) scanQuotedIdentifier(lval *yySymType) int {
	startPos := l.pos - utf8.RuneLen('\'')
	var sb strings.Builder
	for {
		ch, _, err := l.readRune()
		if err != nil {
			l.Error(fmt.Sprintf("unterminated quoted attribute name starting at byte %d", startPos))
			return 0
		}
		switch ch {
		case '\'':
			lval.str = sb.String()
			return IDENTIFIER
		case '\n':
			l.Error(fmt.Sprintf("newline in quoted attribute name starting at byte %d", startPos))
			return 0
		case '\\':
			escaped, _, eerr := l.readRune()
			if eerr != nil {
				l.Error(fmt.Sprintf("unterminated quoted attribute name starting at byte %d", startPos))
				return 0
			}
			writeLexRune(&sb, escaped)
		default:
			writeLexRune(&sb, ch)
		}
	}
}

func (l *StreamingLexer) recordRune(ch rune, size int) {
	l.pos += size
	l.seen = append(l.seen, ch)
}

func (l *StreamingLexer) formatError(msg string) string {
	line, col := 1, 0
	for _, r := range l.seen {
		if r == '\n' {
			line++
			col = 0
		} else {
			col++
		}
	}

	runes := l.seen
	lastNL := -1
	for i := len(runes) - 1; i >= 0; i-- {
		if runes[i] == '\n' {
			lastNL = i
			break
		}
	}
	start := lastNL + 1
	lineText := string(runes[start:])
	caret := strings.Repeat(" ", max(col-1, 0)) + "^"

	return fmt.Sprintf("parse error at line %d, col %d: %s\n%s\n%s", line, col, msg, lineText, caret)
}

func (l *StreamingLexer) scanNumber(first rune, lval *yySymType) int {
	signed := l.signedNum
	l.signedNum = false
	c := numCursor{sl: l, buf: append(make([]byte, 0, 24), byte(first))}
	isReal, msg := scanNumberText(byte(first), &c)
	text := string(c.buf)
	if msg != "" {
		l.Error(fmt.Sprintf(msg, text))
		return 0
	}
	kind, i, r, err := numberToken(text, isReal, signed)
	if err != nil {
		l.Error(err.Error())
		return 0
	}
	lval.integer, lval.real = i, r
	return kind
}

func (l *StreamingLexer) scanIdentifierOrKeyword(first rune, lval *yySymType) int {
	var sb strings.Builder
	sb.WriteRune(first)

	for {
		ch, err := l.peekRune()
		if err != nil {
			break
		}
		if isASCIILetter(ch) || isASCIIDigit(ch) || ch == '_' {
			if err := l.discardRune(); err != nil {
				return 0
			}
			sb.WriteRune(ch)
			continue
		}
		break
	}

	text := sb.String()
	textUpper := strings.ToUpper(text)

	if next, err := l.peekRune(); err == nil && next == '.' {
		switch textUpper {
		case "MY", "TARGET", "PARENT":
			// Fold "MY.name" into one token only when a name follows the '.';
			// otherwise the '.' stays a separate token, as in libclassad.
			after, aerr := l.r.Peek(1)
			if aerr == nil && (isASCIILetter(rune(after[0])) || after[0] == '_') {
				if err := l.discardRune(); err != nil { // the '.'
					return 0
				}
				peek, err := l.peekRune()
				if err != nil {
					return 0
				}
				if err := l.discardRune(); err != nil {
					return 0
				}
				sb.WriteRune('.')
				sb.WriteRune(peek)
				for {
					nextCh, err := l.peekRune()
					if err != nil {
						break
					}
					if isASCIILetter(nextCh) || isASCIIDigit(nextCh) || nextCh == '_' {
						if err := l.discardRune(); err != nil {
							return 0
						}
						sb.WriteRune(nextCh)
					} else {
						break
					}
				}
			}
		}
	}

	scoped := sb.String()
	switch strings.ToLower(scoped) {
	case "true":
		lval.boolean = true
		return BOOLEAN_LITERAL
	case "false":
		lval.boolean = false
		return BOOLEAN_LITERAL
	case "undefined":
		return UNDEFINED
	case "error":
		return ERROR
	case "is":
		return IS
	case "isnt":
		return ISNT
	}

	lval.str = scoped
	return IDENTIFIER
}
