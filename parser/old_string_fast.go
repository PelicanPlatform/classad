package parser

import "strings"

// loneOldStringLiteral reports whether input is a single old-ClassAd string
// literal with nothing but whitespace around it, and returns the literal's
// value. It is the allocation-light path for the shape most job_queue.log
// values and most values on a CEDAR wire have.
//
// It applies the same rules as scanStringOld (C++ Lexer::tokenizeStringOld):
// no escape is interpreted, so a backslash is a literal byte, except that a
// backslash immediately before the closing quote keeps that quote inside the
// literal -- unless the quote is the last thing on the line or in the input,
// where the literal ends with the backslash as its last byte.
//
// Anything it cannot decide -- a comment, an unterminated literal, a quote
// that opens an expression rather than the whole input -- is declined, and the
// full lexer and grammar answer instead.
func loneOldStringLiteral(input string) (string, bool) {
	i := 0
	for i < len(input) && isCSpaceByte(input[i]) {
		i++
	}
	if i >= len(input) || input[i] != '"' {
		return "", false
	}
	i++
	body := i
	// A literal holding no backslash is returned as a sub-slice of the input
	// rather than a copy, so the common case allocates nothing. A quote or a
	// backslash is a single byte in UTF-8 and never a continuation byte, so
	// scanning bytes stops in exactly the places the rune-based lexer does.
	for ; i < len(input); i++ {
		switch input[i] {
		case '"':
			if !onlyCSpace(input[i+1:]) {
				return "", false
			}
			return input[body:i], true
		case '\\':
			return escapedOldStringLiteral(input, body, i)
		}
	}
	return "", false // unterminated
}

// escapedOldStringLiteral finds the end of a literal whose body starts at body
// and whose first backslash is at i, building the value a byte at a time.
func escapedOldStringLiteral(input string, body, i int) (string, bool) {
	var sb strings.Builder
	sb.Grow(len(input) - body)
	sb.WriteString(input[body:i])
	for i < len(input) {
		switch c := input[i]; c {
		case '"':
			if !onlyCSpace(input[i+1:]) {
				return "", false
			}
			return sb.String(), true
		case '\\':
			if i+1 < len(input) && input[i+1] == '"' {
				switch {
				case i+2 >= len(input):
					sb.WriteByte('\\')
					return sb.String(), true
				case input[i+2] == '\n':
					sb.WriteByte('\\')
					if !onlyCSpace(input[i+3:]) {
						return "", false
					}
					return sb.String(), true
				}
				sb.WriteByte('"')
				i += 2
				continue
			}
			sb.WriteByte('\\')
			i++
		default:
			sb.WriteByte(c)
			i++
		}
	}
	return "", false // unterminated
}

// isCSpaceByte is isCSpace for a byte. Every rune isCSpace accepts is ASCII.
func isCSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}

// onlyCSpace reports whether s is entirely whitespace, which the lexer skips to
// reach the end of the input. A comment is also trivia, but is left to the
// lexer.
func onlyCSpace(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isCSpaceByte(s[i]) {
			return false
		}
	}
	return true
}
