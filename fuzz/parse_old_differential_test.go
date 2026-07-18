//go:build libclassad

// This file hosts FuzzParseOldClassAdDifferential, the differential *old-ClassAd* parser
// target. The sibling FuzzParseDifferential only ever exercises the NEW-ClassAd parser
// (classad.Parse vs libclassad's ClassAdParser) -- so the OLD-ClassAd wire format that
// daemons actually advertise, whose string literals keep escapes literal
// (Lexer::tokenizeStringOld), was never differentially tested. That blind spot is how the
// OSIssue = "\S" divergence shipped: C++ accepted it, Go's ParseOld rejected it, and the
// error dropped the whole ad. This target parses each input with BOTH old-ClassAd parsers
// (classad.ParseOld vs libclassad SetOldClassAd(true)) and fails on disagreement.
//
// Run:
//
//	go test ./fuzz -run=xxx -fuzz=FuzzParseOldClassAdDifferential -tags libclassad
package fuzz

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/PelicanPlatform/classad/fuzz/differ"
)

// oldStringSeeds are raw string-literal CONTENTS (the bytes between the quotes) that stress
// old-ClassAd string lexing (tokenizeStringOld). Each is wrapped as `A = "<seed>"` before
// parsing, so the target isolates the string tokenizer -- the escape/quote handling where the
// OSIssue = "\S" divergence lived -- rather than the shared expression grammar or the
// top-level old-vs-new structure (whose recovery deltas are the sibling target's concern).
var oldStringSeeds = []string{
	`\S`,              // the field regression: agetty escape, unknown to C
	`\S \r (\l) \m`,   // a realistic /etc/issue string, several escapes
	`slot1@host`,      // plain content
	`C:\Users\condor`, // literal backslashes
	`a\"b`,            // escaped quote -> literal quote, string continues
	`x\\y`,            // double backslash in the middle
	`tab\there`,       // a recognized C escape, kept literal in old mode
	``,                // empty string
	`\`,               // a lone trailing backslash (escapes the closing quote in C++)
	`\\`,              // backslash-backslash before the quote
}

// knownOldParseDelta reports whether an old-ClassAd parse-level disagreement is an
// intentional grammar difference rather than a Go bug. It reuses the new-parser deltas
// (integer overflow, "..", unbalanced brackets, dangling operator before ':') since old and
// new ClassAds share the expression grammar and number lexer; only the string tokenizer
// differs, and that is exactly what this target is meant to flag rather than excuse.
func knownOldParseDelta(src string, r differ.Result) bool {
	return knownParseDelta(src, r)
}

// hasUnescapedQuote reports whether s contains a double quote that would terminate the
// wrapper string literal. In old-ClassAd lexing a quote is escaped iff the IMMEDIATELY
// preceding character is a backslash (tokenizeStringOld checks the one prior char, not an
// even/odd run), so a "bare" quote is one at the start or not directly preceded by '\'.
func hasUnescapedQuote(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '"' && (i == 0 || s[i-1] != '\\') {
			return true
		}
	}
	return false
}

func FuzzParseOldClassAdDifferential(f *testing.F) {
	for _, s := range oldStringSeeds {
		f.Add(s)
	}
	opts := differ.DefaultOptions()
	opts.OldClassAd = true

	f.Fuzz(func(t *testing.T, s string) {
		// Same lexer-scope exclusions as the new-parser target: non-UTF-8 and embedded NUL
		// are known byte-vs-rune / C-string-terminator deltas, out of scope here.
		if !utf8.ValidString(s) || strings.IndexByte(s, 0) >= 0 {
			t.Skip()
		}
		// A bare (unescaped) double quote in the content terminates the wrapper string and
		// turns the input into malformed multi-token structure, whose parser-recovery deltas
		// are the sibling target's concern, not this one. The escape divergence class is
		// entirely about BACKSLASH handling (quotes terminate strings identically in both
		// engines), so skipping bare-quote content loses no relevant coverage.
		if hasUnescapedQuote(s) {
			t.Skip()
		}
		// Old-ClassAd is a line-oriented wire format -- a newline ends the attribute, so a
		// string value is single-line and cannot contain a raw newline/CR. Such content isn't
		// representable on the wire and only exercises the line-based old->new conversion's
		// structural recovery, not string-escape lexing; skip it.
		if strings.ContainsAny(s, "\n\r") {
			t.Skip()
		}
		// A block-comment marker in the content trips the line-based conversion's comment
		// handling (both engines' convertOldToNewFormat), again a structural artifact rather
		// than string lexing.
		if strings.Contains(s, "/*") || strings.Contains(s, "*/") {
			t.Skip()
		}
		// Wrap the fuzzer bytes as one old-ClassAd string-valued attribute, so what varies is
		// the string literal's content -- driving tokenizeStringOld vs the Go lenient scanner.
		src := `A = "` + s + `"`
		r := differ.Compare(src, opts)
		switch r.Category {
		case differ.GoPanic:
			t.Fatalf("Go engine panicked (old ClassAd)\n  content: %q\n  %s", s, r.Detail)
		case differ.ParseDivergence:
			if knownOldParseDelta(src, r) {
				return
			}
			t.Fatalf("old-ClassAd string-lexer disagreement (%s)\n  content: %q\n  go-err: %v",
				r.Detail, s, r.GoErr)
		}
	})
}
