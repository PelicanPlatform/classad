package vm

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// THE LEXICAL HALF OF THE SCRIPT PARSER: logical lines, and nothing else.
//
// A script is ClassAd expressions with statements around them, and the only thing standing between
// those two is the question of where one statement stops. ClassAd's own lexer has no answer, because
// it has never needed one: a newline is whitespace to it, exactly like a space. That is the fact this
// file trades on. Promoting the newline to a statement terminator takes nothing away from the
// expression grammar, so the expression grammar does not have to be touched, forked, or reimplemented
// -- every expression leaf still goes through parser.ParseExpr, which is the parser the engine, the
// wire format and the differential fuzzers all already agree about.
//
// WHY INDENTATION AND NOT BRACES. `{...}` is a ClassAd list literal and `[...]` is a ClassAd ad
// literal. Both appear inside ordinary expressions, on ordinary lines, constantly. A brace-delimited
// block syntax would overload the one token that already means "list" in every expression in the
// language, and a reader -- never mind a parser -- would have to count depth to know which `}` was
// which. Indentation uses a channel ClassAd does not use at all, so it collides with nothing.
//
// WHAT THIS FILE PRODUCES. One pass over the source yields two things:
//
//   - a `clean` buffer, byte-for-byte the same length as the source, in which every byte that
//     belongs to a comment or to a line-joining backslash has been replaced by a space; and
//   - the logical lines: for each, its indentation and the half-open byte range of its content.
//
// The equal-length rule is the point of the clean buffer. Every offset in it is an offset in the
// original source, so a diagnostic computed from a position in the cleaned text names a real line and
// column in what the user wrote, with no bookkeeping to get wrong. Blanking rather than deleting is
// what buys that, and it costs one copy of the source.
//
// A logical line's content is then handed to parser.ParseExpr as a plain substring. Comments inside
// it are already spaces, and any newline inside it is inside brackets -- which ClassAd's lexer treats
// as whitespace -- so the substring is a valid expression exactly when the user wrote one.

// scriptLine is one logical line: a statement's source, with the indentation that places it in a
// block.
type scriptLine struct {
	indent int // leading spaces; tabs are rejected before a line gets this far
	off    int // byte offset of the first content byte, just past the indentation
	end    int // byte offset just past the last content byte, trailing whitespace trimmed
}

// lineScanner holds the one pass's state. It exists as a type only so the bracket stack and the
// clean buffer do not have to be threaded through half a dozen helpers.
type lineScanner struct {
	src   string
	clean []byte
	pos   *posIndex
	lines []scriptLine
	// opens is the stack of bracket offsets that have not been closed on the current logical line.
	// It holds offsets rather than a count so an unterminated bracket can be blamed on the one that
	// opened it, which is the only position a reader can act on.
	opens []int
}

// scanLines splits src into logical lines, blanking comments and joined line breaks as it goes.
func scanLines(src string, pos *posIndex) ([]scriptLine, []byte, error) {
	s := &lineScanner{src: src, clean: []byte(src), pos: pos}
	if err := s.run(); err != nil {
		return nil, nil, err
	}
	return s.lines, s.clean, nil
}

func (s *lineScanner) run() error {
	i := 0
	for i < len(s.src) {
		indentStart := i
		for i < len(s.src) && (s.src[i] == ' ' || s.src[i] == '\t') {
			i++
		}
		indentEnd := i
		// A comment before the first code on a line is skipped WITHOUT counting towards the
		// indentation, so `/* note */ x = 1` is a statement in column 1 and not one in column 12. A
		// `#` or `//` comment runs to the end of the line and leaves nothing behind it, so only a
		// block comment can reach the second iteration here.
		for i < len(s.src) && commentAt(s.src, i) {
			next, err := s.blankComment(i)
			if err != nil {
				return err
			}
			i = next
			for i < len(s.src) && (s.src[i] == ' ' || s.src[i] == '\t') {
				i++
			}
		}
		// A line with nothing but whitespace, or nothing but a comment, carries no indentation
		// information: Python ignores it for INDENT/DEDENT purposes and so does this, because the
		// alternative is that a stray comment column changes which block the next statement is in.
		if i >= len(s.src) {
			break
		}
		if s.src[i] == '\n' || s.src[i] == '\r' {
			i = s.skipEOL(i)
			continue
		}
		// INDENTATION IS SPACES, and a tab in it is an error rather than a width guess. Python 3
		// rejects only inconsistent mixing; this rejects a tab outright, because "how wide is a tab"
		// has no answer the surrounding ecosystem agrees on, and a misread indent does not produce a
		// wrong value -- it produces a statement in the wrong block, which is a different program.
		if tab := strings.IndexByte(s.src[indentStart:indentEnd], '\t'); tab >= 0 {
			return s.errAt(indentStart+tab,
				"tab in indentation: ClassAd Script indents with spaces only")
		}
		start := i
		end, err := s.consumeLogicalLine(i)
		if err != nil {
			return err
		}
		content := trimRightSpace(s.clean, start, end)
		if content > start {
			s.lines = append(s.lines, scriptLine{indent: indentEnd - indentStart, off: start, end: content})
		}
		i = end
	}
	return nil
}

// consumeLogicalLine advances from the first content byte to just past the end of the logical line,
// blanking comments and joined breaks and tracking bracket depth. It returns the offset of the first
// byte of the next physical line (or len(src)).
func (s *lineScanner) consumeLogicalLine(i int) (int, error) {
	s.opens = s.opens[:0]
	for i < len(s.src) {
		c := s.src[i]
		switch {
		case c == '"' || c == '\'':
			next, err := s.scanQuoted(i)
			if err != nil {
				return 0, err
			}
			i = next
		case commentAt(s.src, i):
			next, err := s.blankComment(i)
			if err != nil {
				return 0, err
			}
			i = next
		case c == '\\' && atEOL(s.src, i+1):
			// Explicit line joining. The backslash and the break it hides are blanked together, so
			// the two physical lines become one run of source with no newline in it at all and the
			// offsets of everything after are untouched.
			end := s.skipEOL(i + 1)
			blank(s.clean, i, end)
			i = end
		case c == '(' || c == '[' || c == '{':
			s.opens = append(s.opens, i)
			i++
		case c == ')' || c == ']' || c == '}':
			// An unmatched closer is not diagnosed here. It is a perfectly ordinary expression
			// syntax error, and ParseExpr's message about it is better than anything this scanner
			// could say, because ParseExpr knows what was open.
			if len(s.opens) > 0 {
				s.opens = s.opens[:len(s.opens)-1]
			}
			i++
		case c == '\n' || c == '\r':
			// IMPLICIT LINE JOINING. One depth counter covers `(`, `[` and `{` together, which is
			// necessary rather than merely convenient: ClassAd's `[` opens an ad literal and also
			// opens a subscript, and it is the same token either way, so there is nothing to count
			// separately. This is what lets a multi-line ad literal -- semicolons and all -- be the
			// right-hand side of an assignment.
			if len(s.opens) > 0 {
				i = s.skipEOL(i)
				continue
			}
			return s.skipEOL(i), nil
		default:
			i++
		}
	}
	if len(s.opens) > 0 {
		at := s.opens[len(s.opens)-1]
		return 0, s.errAt(at, fmt.Sprintf("unclosed %q at end of input", s.src[at]))
	}
	return i, nil
}

// scanQuoted advances past a string literal or a quoted attribute name.
//
// A raw line break inside either is rejected. ClassAd's lexer would accept it -- it scans to the
// closing quote and does not care what it crosses -- but in script context a missing close quote
// would then swallow every statement to the end of the file and report the failure somewhere
// unrelated. Ending the literal at the break costs a construct nobody writes and buys a diagnostic
// that points at the line with the mistake on it.
func (s *lineScanner) scanQuoted(i int) (int, error) {
	quote := s.src[i]
	for j := i + 1; j < len(s.src); j++ {
		switch s.src[j] {
		case '\\':
			j++
		case '\n', '\r':
			return 0, s.errAt(i, fmt.Sprintf("unterminated %s", quotedKind(quote)))
		case quote:
			return j + 1, nil
		}
	}
	return 0, s.errAt(i, fmt.Sprintf("unterminated %s", quotedKind(quote)))
}

// blankComment replaces a comment with spaces and returns the offset just past it.
func (s *lineScanner) blankComment(i int) (int, error) {
	if s.src[i] == '#' || s.src[i+1] == '/' {
		end := i
		for end < len(s.src) && s.src[end] != '\n' && s.src[end] != '\r' {
			end++
		}
		blank(s.clean, i, end)
		return end, nil
	}
	// A BLOCK COMMENT MAY NOT CROSS A LINE BREAK. ClassAd allows it, and in an expression that is
	// harmless; here it would blank a NEWLINE and silently glue two statements into one, which is
	// the one way a comment can change what a program does. Refusing it is cheap and the message
	// says exactly what to do.
	for end := i + 2; end+1 < len(s.src); end++ {
		switch {
		case s.src[end] == '\n' || s.src[end] == '\r':
			return 0, s.errAt(i, "a /* */ comment may not span a line break in a script; use # or // instead")
		case s.src[end] == '*' && s.src[end+1] == '/':
			blank(s.clean, i, end+2)
			return end + 2, nil
		}
	}
	return 0, s.errAt(i, "unterminated /* comment")
}

// skipEOL advances past one line terminator, treating \r\n as one.
func (s *lineScanner) skipEOL(i int) int {
	if i < len(s.src) && s.src[i] == '\r' {
		i++
		if i < len(s.src) && s.src[i] == '\n' {
			i++
		}
		return i
	}
	if i < len(s.src) && s.src[i] == '\n' {
		return i + 1
	}
	return i
}

func (s *lineScanner) errAt(off int, msg string) error {
	return s.pos.errorAt(off, msg)
}

// commentAt reports whether a comment begins at i. `#` is the script's own; `//` and `/* */` are
// ClassAd's, kept because an expression pasted out of an existing ad may carry one.
func commentAt(src string, i int) bool {
	if src[i] == '#' {
		return true
	}
	return src[i] == '/' && i+1 < len(src) && (src[i+1] == '/' || src[i+1] == '*')
}

func atEOL(src string, i int) bool {
	return i < len(src) && (src[i] == '\n' || src[i] == '\r')
}

func quotedKind(quote byte) string {
	if quote == '\'' {
		return "quoted attribute name"
	}
	return "string literal"
}

func blank(buf []byte, from, to int) {
	for i := from; i < to && i < len(buf); i++ {
		if buf[i] != '\n' && buf[i] != '\r' {
			buf[i] = ' '
		}
	}
}

func trimRightSpace(buf []byte, from, to int) int {
	for to > from {
		switch buf[to-1] {
		case ' ', '\t', '\n', '\r':
			to--
		default:
			return to
		}
	}
	return from
}

// posIndex turns a byte offset into the line and column a human would count.
//
// Columns are counted in RUNES, not bytes, because that is what the ClassAd parser's own diagnostics
// count and a script that mixes the two in its error messages is worse than one that picks either.
type posIndex struct {
	src    string
	starts []int // byte offset of the first byte of each line, lines[0] == 0
}

func newPosIndex(src string) *posIndex {
	p := &posIndex{src: src, starts: []int{0}}
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			p.starts = append(p.starts, i+1)
		}
	}
	return p
}

// lineCol returns the 1-based line and column of off.
func (p *posIndex) lineCol(off int) (int, int) {
	if off > len(p.src) {
		off = len(p.src)
	}
	lo, hi := 0, len(p.starts)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if p.starts[mid] <= off {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	col := 1
	for i := p.starts[lo]; i < off; {
		_, size := utf8.DecodeRuneInString(p.src[i:])
		i += size
		col++
	}
	return lo + 1, col
}

// sourceLine returns the text of the line containing off, without its terminator.
func (p *posIndex) sourceLine(line int) string {
	if line < 1 || line > len(p.starts) {
		return ""
	}
	start := p.starts[line-1]
	end := len(p.src)
	if line < len(p.starts) {
		end = p.starts[line]
	}
	return strings.TrimRight(p.src[start:end], "\r\n")
}

func (p *posIndex) errorAt(off int, msg string) *ScriptSyntaxError {
	line, col := p.lineCol(off)
	return &ScriptSyntaxError{Line: line, Column: col, Msg: msg, Source: p.sourceLine(line)}
}
