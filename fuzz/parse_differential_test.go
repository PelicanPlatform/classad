//go:build libclassad

// This file hosts FuzzParseDifferential, the coverage-guided *parser* fuzz
// target: Go's native fuzzing mutates ClassAd source text, and each input is
// PARSED by both the Go parser and libclassad. Unlike FuzzDifferential (which
// focuses on evaluation and ignores parse-level disagreement), this target
// fails when the two parsers disagree on whether the input is well-formed --
// except for the intentional grammar deltas catalogued in knownParseDelta.
//
// Run:
//
//	go test ./fuzz -run=xxx -fuzz=FuzzParseDifferential -tags libclassad
package fuzz

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/PelicanPlatform/classad/fuzz/differ"
)

// knownParseDelta reports whether a parse-level disagreement is one of the
// intentional, documented differences between the Go grammar/lexer and
// libclassad's -- i.e. not a Go parser bug.
func knownParseDelta(src string, r differ.Result) bool {
	// Integer-literal overflow: Go rejects a decimal integer that does not fit
	// int64, while libclassad silently wraps it (CPP_QUIRKS #1). Deliberately
	// not mirrored. (The lexer's "invalid integer" error is reported by yacc as
	// a generic "syntax error", so detect the overflowing literal in the source
	// instead.)
	if !r.GoParsed && r.CppParsed && containsOverflowingInt(src) {
		return true
	}
	// libclassad's number lexer is strtod-lenient and accepts a doubled leading
	// dot ("..5" -> 0.0) that the Go lexer rejects. CPP_QUIRKS #10; not
	// mirrored. (".e5" is a leading-dot reference to e5, handled by the parser,
	// not a float quirk.)
	if !r.GoParsed && r.CppParsed && strings.Contains(src, "..") {
		return true
	}
	// libclassad's parser leniently recovers from unbalanced or mismatched
	// brackets (e.g. "{(0?[0:0)}" is accepted as "{0}"); the Go parser rejects
	// them. CPP_QUIRKS #11; not mirrored.
	if !r.GoParsed && r.CppParsed && !bracketsBalanced(src) {
		return true
	}
	// Inside a conditional, libclassad's error recovery accepts a then-branch
	// that ends in a dangling binary operator with no right operand before the
	// ':' (e.g. "a ? b % : c"); the Go parser rejects it. CPP_QUIRKS #12; not
	// mirrored. A well-formed ternary's then-expression never ends in a binary
	// operator, so an operator immediately before ':' is the signature.
	if !r.GoParsed && r.CppParsed && danglingOpBeforeColon(src) {
		return true
	}
	return false
}

// danglingOpBeforeColon reports whether a ':' in src is immediately preceded
// (ignoring whitespace) by a binary-operator character -- i.e. an expression
// with a missing right operand sits just before the ternary ':'. Brackets and
// operators inside "..." string literals and '...' quoted attribute names are
// ignored.
func danglingOpBeforeColon(src string) bool {
	// Characters that terminate a ClassAd binary operator (+ - * / %, & | ^,
	// < > << >> <= >=, == != =?= =!= =). A valid operand never ends in one.
	isOpEnd := func(c byte) bool {
		switch c {
		case '+', '-', '*', '/', '%', '&', '|', '^', '<', '>', '=', '!':
			return true
		}
		return false
	}
	inDQ, inSQ := false, false
	var prev byte // last non-space significant char, 0 if none
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inDQ:
			if c == '\\' {
				i++
			} else if c == '"' {
				inDQ = false
			}
			continue
		case inSQ:
			if c == '\\' {
				i++
			} else if c == '\'' {
				inSQ = false
			}
			continue
		case c == '"':
			inDQ = true
		case c == '\'':
			inSQ = true
		case c == ':':
			if isOpEnd(prev) {
				return true
			}
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			continue // do not update prev across whitespace
		}
		prev = c
	}
	return false
}

// bracketsBalanced reports whether (), [], and {} nest properly in src, ignoring
// brackets inside "..." string literals and '...' quoted attribute names.
func bracketsBalanced(src string) bool {
	var stack []byte
	inDQ, inSQ := false, false
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inDQ:
			if c == '\\' {
				i++
			} else if c == '"' {
				inDQ = false
			}
		case inSQ:
			if c == '\\' {
				i++
			} else if c == '\'' {
				inSQ = false
			}
		case c == '"':
			inDQ = true
		case c == '\'':
			inSQ = true
		case c == '[':
			stack = append(stack, ']')
		case c == '(':
			stack = append(stack, ')')
		case c == '{':
			stack = append(stack, '}')
		case c == ']' || c == ')' || c == '}':
			if len(stack) == 0 || stack[len(stack)-1] != c {
				return false
			}
			stack = stack[:len(stack)-1]
		}
	}
	return len(stack) == 0
}

// containsOverflowingInt reports whether src contains a bare decimal integer
// literal whose magnitude does not fit in int64 (so the Go lexer rejects it).
// It skips hex, float mantissa/exponent digits, and digits inside identifiers.
func containsOverflowingInt(src string) bool {
	b := []byte(src)
	isIdent := func(c byte) bool {
		return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
	}
	for i := 0; i < len(b); i++ {
		if b[i] < '0' || b[i] > '9' {
			continue
		}
		// Not the start of a bare decimal literal if the previous byte could
		// make this hex, a float part, a continuation, or part of an identifier.
		if i > 0 {
			p := b[i-1]
			if p == '.' || p == 'x' || p == 'X' || isIdent(p) ||
				(p >= '0' && p <= '9') {
				for i < len(b) && b[i] >= '0' && b[i] <= '9' {
					i++
				}
				continue
			}
		}
		j := i
		for j < len(b) && b[j] >= '0' && b[j] <= '9' {
			j++
		}
		// A trailing '.', exponent, hex, or hex-float marker means it is not a
		// plain decimal integer literal.
		if j < len(b) {
			switch b[j] {
			case '.', 'e', 'E', 'x', 'X', 'p', 'P':
				i = j
				continue
			}
		}
		if _, err := strconv.ParseInt(string(b[i:j]), 10, 64); err != nil {
			return true // all-digit run that does not fit int64
		}
		i = j
	}
	return false
}

func FuzzParseDifferential(f *testing.F) {
	for _, s := range seeds {
		addIfMatch(f, s)
	}
	loadCorpus(f)

	opts := differ.DefaultOptions() // IgnoreParseDivergence = false

	f.Fuzz(func(t *testing.T, src string) {
		// Non-UTF-8 string-literal scanning is a known byte-vs-rune lexer delta
		// (see README); out of scope for parser-agreement fuzzing. An embedded
		// NUL is similar: libclassad's C-string lexer treats it as a terminator
		// (so it rejects a string containing one) while the Go lexer keeps it.
		if !utf8.ValidString(src) || strings.IndexByte(src, 0) >= 0 {
			t.Skip()
		}
		r := differ.Compare(src, opts)
		switch r.Category {
		case differ.GoPanic:
			t.Fatalf("Go engine panicked\n  input: %q\n  %s", src, r.Detail)
		case differ.ParseDivergence:
			if knownParseDelta(src, r) {
				return
			}
			t.Fatalf("parser disagreement (%s)\n  input: %q\n  go-err: %v",
				r.Detail, src, r.GoErr)
		}
	})
}
