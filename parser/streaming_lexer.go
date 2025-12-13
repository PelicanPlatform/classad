package parser

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
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
}

// Lex implements the goyacc Lexer interface.
func (l *StreamingLexer) Lex(lval *yySymType) int {
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
		return int('-')
	case '*':
		return int('*')
	case '%':
		return int('%')
	case '.':
		return int('.')
	case '"':
		str := l.scanString()
		lval.str = str
		return STRING_LITERAL
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
				if peek, err := l.peekRune(); err == nil && peek == '=' {
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
				if peek, err := l.peekRune(); err == nil && peek == '=' {
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
				return l.Lex(lval)
			case '*':
				if err := l.discardRune(); err != nil {
					l.err = err
					return 0
				}
				if err := l.skipBlockComment(); err != nil {
					l.err = err
					return 0
				}
				return l.Lex(lval)
			}
		}
		return int('/')
	}

	// Numbers
	if unicode.IsDigit(ch) {
		return l.scanNumber(ch, lval)
	}

	// Identifiers and keywords
	if unicode.IsLetter(ch) || ch == '_' {
		return l.scanIdentifierOrKeyword(ch, lval)
	}

	// Unknown character
	l.Error(fmt.Sprintf("unexpected character: %c", ch))
	return l.Lex(lval)
}

// Error implements the goyacc Lexer interface.
func (l *StreamingLexer) Error(s string) {
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

	ch, size, err := l.r.ReadRune()
	if err != nil {
		return 0, 0, err
	}
	l.recordRune(ch, size)
	return ch, size, nil
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

	ch, size, err := l.r.ReadRune()
	if err != nil {
		return 0, err
	}
	// Do not advance pos/seen yet; stage as pending.
	l.pendingRune = ch
	l.pendingSize = size
	l.hasPending = true
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

		if unicode.IsSpace(ch) {
			continue
		}

		if ch == '/' {
			next, err := l.peekRune()
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
				result.WriteRune(rune(val))
			default:
				l.Error(fmt.Sprintf("invalid escape sequence \\%c at position %d", escaped, l.pos-2))
				result.WriteRune(escaped)
			}
			continue
		}

		result.WriteRune(ch)
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
	var sb strings.Builder
	sb.WriteRune(first)

	hasDecimal := false
	hasExponent := false

	for {
		ch, err := l.peekRune()
		if err != nil {
			break
		}

		if unicode.IsDigit(ch) {
			if err := l.discardRune(); err != nil {
				return 0
			}
			sb.WriteRune(ch)
			continue
		}

		if ch == '.' && !hasDecimal && !hasExponent {
			hasDecimal = true
			if err := l.discardRune(); err != nil {
				return 0
			}
			sb.WriteRune(ch)
			continue
		}

		if (ch == 'e' || ch == 'E') && !hasExponent {
			hasExponent = true
			hasDecimal = true
			if err := l.discardRune(); err != nil {
				return 0
			}
			sb.WriteRune(ch)
			next, err := l.peekRune()
			if err == nil && (next == '+' || next == '-') {
				if err := l.discardRune(); err != nil {
					return 0
				}
				sb.WriteRune(next)
			}
			continue
		}

		break
	}

	text := sb.String()

	if hasDecimal || hasExponent {
		val, err := strconv.ParseFloat(text, 64)
		if err != nil {
			l.Error(fmt.Sprintf("invalid real number: %s", text))
			return 0
		}
		lval.real = val
		return REAL_LITERAL
	}

	val, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		l.Error(fmt.Sprintf("invalid integer: %s", text))
		return 0
	}
	lval.integer = val
	return INTEGER_LITERAL
}

func (l *StreamingLexer) scanIdentifierOrKeyword(first rune, lval *yySymType) int {
	var sb strings.Builder
	sb.WriteRune(first)

	for {
		ch, err := l.peekRune()
		if err != nil {
			break
		}
		if unicode.IsLetter(ch) || unicode.IsDigit(ch) || ch == '_' {
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
			// Consume '.'
			if err := l.discardRune(); err != nil {
				return 0
			}
			peek, err := l.peekRune()
			if err == nil && (unicode.IsLetter(peek) || peek == '_') {
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
					if unicode.IsLetter(nextCh) || unicode.IsDigit(nextCh) || nextCh == '_' {
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
