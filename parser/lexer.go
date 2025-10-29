// Package parser implements the ClassAd lexer and parser.
package parser

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bbockelm/golang-classads/ast"
)

// Token represents a lexical token.
type Token struct {
	Type int
	Text string
	Pos  int
}

// Lexer represents a lexical scanner for ClassAd expressions.
type Lexer struct {
	input  string
	pos    int
	result ast.Node
	err    error
}

// NewLexer creates a new lexer for the given input.
func NewLexer(input string) *Lexer {
	return &Lexer{
		input: input,
		pos:   0,
	}
}

// Lex implements the goyacc Lexer interface.
func (l *Lexer) Lex(lval *yySymType) int {
	l.skipWhitespace()

	if l.pos >= len(l.input) {
		return 0 // EOF
	}

	// Check for operators and punctuation
	switch l.peek() {
	case '[':
		l.advance()
		return int('[')
	case ']':
		l.advance()
		return int(']')
	case '{':
		l.advance()
		return int('{')
	case '}':
		l.advance()
		return int('}')
	case '(':
		l.advance()
		return int('(')
	case ')':
		l.advance()
		return int(')')
	case ';':
		l.advance()
		return int(';')
	case ',':
		l.advance()
		return int(',')
	case '?':
		l.advance()
		return int('?')
	case ':':
		l.advance()
		return int(':')
	case '^':
		l.advance()
		return int('^')
	case '~':
		l.advance()
		return int('~')
	case '+':
		l.advance()
		return int('+')
	case '-':
		l.advance()
		return int('-')
	case '*':
		l.advance()
		return int('*')
	case '%':
		l.advance()
		return int('%')
	case '.':
		l.advance()
		return int('.')
	case '"':
		str := l.scanString()
		lval.str = str
		return STRING_LITERAL
	case '=':
		l.advance()
		if l.peek() == '=' {
			l.advance()
			return EQ
		} else if l.peek() == '?' {
			l.advance()
			if l.peek() == '=' {
				l.advance()
				return IS // =?= is an alias for 'is'
			}
			// Put back the '?'
			l.pos--
			return int('=')
		} else if l.peek() == '!' {
			l.advance()
			if l.peek() == '=' {
				l.advance()
				return ISNT // =!= is an alias for 'isnt'
			}
			// Put back the '!'
			l.pos--
			return int('=')
		}
		return int('=')
	case '!':
		l.advance()
		if l.peek() == '=' {
			l.advance()
			return NE
		}
		return int('!')
	case '<':
		l.advance()
		if l.peek() == '=' {
			l.advance()
			return LE
		} else if l.peek() == '<' {
			l.advance()
			return LSHIFT
		}
		return int('<')
	case '>':
		l.advance()
		if l.peek() == '=' {
			l.advance()
			return GE
		} else if l.peek() == '>' {
			l.advance()
			if l.peek() == '>' {
				l.advance()
				return URSHIFT
			}
			return RSHIFT
		}
		return int('>')
	case '&':
		l.advance()
		if l.peek() == '&' {
			l.advance()
			return AND
		}
		return int('&')
	case '|':
		l.advance()
		if l.peek() == '|' {
			l.advance()
			return OR
		}
		return int('|')
	case '/':
		l.advance()
		if l.peek() == '/' {
			// Line comment
			l.skipLineComment()
			return l.Lex(lval)
		} else if l.peek() == '*' {
			// Block comment
			l.skipBlockComment()
			return l.Lex(lval)
		}
		return int('/')
	}

	// Check for numbers
	ch := l.peek()
	if unicode.IsDigit(ch) {
		return l.scanNumber(lval)
	}

	// Check for identifiers and keywords
	if unicode.IsLetter(ch) || ch == '_' {
		return l.scanIdentifierOrKeyword(lval)
	}

	// Unknown character
	l.Error(fmt.Sprintf("unexpected character: %c", ch))
	l.advance()
	return l.Lex(lval)
}

// Error implements the goyacc Lexer interface.
func (l *Lexer) Error(s string) {
	l.err = fmt.Errorf("parse error at position %d: %s", l.pos, s)
}

// Result returns the parsed result and any error.
func (l *Lexer) Result() (ast.Node, error) {
	return l.result, l.err
}

// SetResult sets the parse result.
func (l *Lexer) SetResult(node ast.Node) {
	l.result = node
}

func (l *Lexer) peek() rune {
	if l.pos >= len(l.input) {
		return 0
	}
	ch, _ := utf8.DecodeRuneInString(l.input[l.pos:])
	return ch
}

func (l *Lexer) advance() {
	if l.pos < len(l.input) {
		_, size := utf8.DecodeRuneInString(l.input[l.pos:])
		l.pos += size
	}
}

func (l *Lexer) skipWhitespace() {
	for l.pos < len(l.input) && unicode.IsSpace(l.peek()) {
		l.advance()
	}
}

func (l *Lexer) skipLineComment() {
	// Skip //
	l.advance()
	for l.pos < len(l.input) && l.peek() != '\n' {
		l.advance()
	}
}

func (l *Lexer) skipBlockComment() {
	// Skip /*
	l.advance()
	for l.pos < len(l.input) {
		if l.peek() == '*' {
			l.advance()
			if l.peek() == '/' {
				l.advance()
				return
			}
		} else {
			l.advance()
		}
	}
}

func (l *Lexer) scanString() string {
	// Skip opening quote
	l.advance()
	start := l.pos
	var result strings.Builder

	for l.pos < len(l.input) {
		ch := l.peek()
		if ch == '"' {
			l.advance()
			return result.String()
		} else if ch == '\\' {
			l.advance()
			if l.pos < len(l.input) {
				escaped := l.peek()
				l.advance()
				switch escaped {
				case 'b':
					result.WriteRune('\b') // Backspace (8)
				case 't':
					result.WriteRune('\t') // Tab (9)
				case 'n':
					result.WriteRune('\n') // Newline (10)
				case 'f':
					result.WriteRune('\f') // Formfeed (12)
				case 'r':
					result.WriteRune('\r') // Carriage return (13)
				case '\\':
					result.WriteRune('\\') // Backslash (92)
				case '"':
					result.WriteRune('"') // Quote (34)
				case '\'':
					result.WriteRune('\'') // Apostrophe (39)
				case '0', '1', '2', '3', '4', '5', '6', '7':
					// Octal escape sequence
					// If first digit is 0-3, read up to 3 digits
					// If first digit is 4-7, read up to 2 digits
					var octalStr strings.Builder
					octalStr.WriteRune(escaped)

					maxDigits := 2
					if escaped >= '0' && escaped <= '3' {
						maxDigits = 3
					}

					// Read additional octal digits
					for i := 1; i < maxDigits && l.pos < len(l.input); i++ {
						nextCh := l.peek()
						if nextCh >= '0' && nextCh <= '7' {
							octalStr.WriteRune(nextCh)
							l.advance()
						} else {
							break
						}
					}

					// Convert octal string to integer
					octalValue := int64(0)
					for _, digit := range octalStr.String() {
						octalValue = octalValue*8 + int64(digit-'0')
					}

					// Check for null (value 0) which is not allowed
					if octalValue == 0 {
						l.Error(fmt.Sprintf("null character (\\%s) not allowed in string at position %d", octalStr.String(), l.pos-len(octalStr.String())-2))
						return result.String()
					}

					result.WriteRune(rune(octalValue))
				default:
					// Unknown escape sequence - this is an error according to spec
					l.Error(fmt.Sprintf("invalid escape sequence \\%c at position %d", escaped, l.pos-2))
					result.WriteRune(escaped)
				}
			}
		} else {
			result.WriteRune(ch)
			l.advance()
		}
	}

	l.Error(fmt.Sprintf("unterminated string starting at position %d", start-1))
	return result.String()
}

func (l *Lexer) scanNumber(lval *yySymType) int {
	start := l.pos
	hasDecimal := false
	hasExponent := false

	for l.pos < len(l.input) {
		ch := l.peek()
		if unicode.IsDigit(ch) {
			l.advance()
		} else if ch == '.' && !hasDecimal && !hasExponent {
			hasDecimal = true
			l.advance()
		} else if (ch == 'e' || ch == 'E') && !hasExponent {
			hasExponent = true
			hasDecimal = true // Exponent implies floating point
			l.advance()
			if l.peek() == '+' || l.peek() == '-' {
				l.advance()
			}
		} else {
			break
		}
	}

	text := l.input[start:l.pos]
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

func (l *Lexer) scanIdentifierOrKeyword(lval *yySymType) int {
	start := l.pos
	l.advance()

	for l.pos < len(l.input) {
		ch := l.peek()
		if unicode.IsLetter(ch) || unicode.IsDigit(ch) || ch == '_' {
			l.advance()
		} else {
			break
		}
	}

	text := l.input[start:l.pos]

	// Check for scoped attribute references (MY., TARGET., PARENT.)
	textUpper := strings.ToUpper(text)
	if l.peek() == '.' {
		switch textUpper {
		case "MY", "TARGET", "PARENT":
			// Consume the dot
			l.advance()
			// Now scan the attribute name
			if unicode.IsLetter(l.peek()) || l.peek() == '_' {
				l.advance()
				for l.pos < len(l.input) {
					ch := l.peek()
					if unicode.IsLetter(ch) || unicode.IsDigit(ch) || ch == '_' {
						l.advance()
					} else {
						break
					}
				}
				// Return the full scoped reference (e.g., "MY.Cpus")
				scopedName := l.input[start:l.pos]
				lval.str = scopedName
				return IDENTIFIER
			}
			// If no valid identifier follows, put the dot back and continue
			l.pos--
		}
	}

	// Check for keywords
	switch strings.ToLower(text) {
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

	lval.str = text
	return IDENTIFIER
}
