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
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/fuzz/differ"
)

// knownParseDelta reports whether a parse-level disagreement is one of the
// intentional, documented differences between the Go grammar/lexer and
// libclassad's -- i.e. not a Go parser bug.
func knownParseDelta(src string, r differ.Result) bool {
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

func FuzzParseDifferential(f *testing.F) {
	for _, s := range seeds {
		addIfMatch(f, s)
	}
	loadCorpus(f)

	opts := differ.DefaultOptions() // IgnoreParseDivergence = false

	f.Fuzz(func(t *testing.T, src string) {
		// libclassad's C-string lexer treats an embedded NUL as a terminator
		// (so it rejects a string containing one) while the Go lexer keeps it.
		if strings.IndexByte(src, 0) >= 0 {
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
