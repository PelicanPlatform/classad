//go:build libclassad

// String-literal escape decoding, compared against libclassad.
//
// This is its own file because escapes are the one place the two ClassAd syntaxes
// disagree about a VALUE rather than about what parses, and because that disagreement
// has produced the same production bug three times (a Go collector dropping startd ads
// on "\S" from /etc/issue, a schedd TransferInput name with "\,\ ", and htcondordb
// skipping Requirements on OSPool host blocklists of the form regexp("a\.b\.c")).
//
// The two rules, from the reference lexer:
//
//   - New syntax (Lexer::tokenizeString -> convert_escapes, classad/util.cpp) decodes
//     C escapes and octal, and its default arm passes "char after \ unmodified" -- an
//     undefined escape is ACCEPTED with the backslash DROPPED. Only an embedded \0
//     invalidates the literal.
//   - Old syntax (Lexer::tokenizeStringOld) never calls convert_escapes at all: the
//     literal keeps its bytes, backslashes and all. Only \" collapses, and only when
//     more text follows.
//
// Old syntax is what every job_queue.log value, submit expression and config macro
// function goes through (ParseClassAdRvalExpr sets SetOldClassAd(true)), so the two
// rules are not interchangeable: reading old-syntax text with the new parser silently
// deletes backslashes, which rewrites a regex rather than failing loudly.
//
// Run:
//
//	go test -tags libclassad ./fuzz -run TestEscapeParity
//	go test -tags libclassad ./fuzz -run=xxx -fuzz=FuzzStringEscapeDifferential
package fuzz

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/fuzz/canon"
	cppengine "github.com/PelicanPlatform/classad/fuzz/oracle/cgo"
)

// escapeLiteral is a string literal holding body, as source text.
func escapeLiteral(body string) string { return `"` + body + `"` }

// compareLiteral parses and evaluates src in both engines and reports a mismatch.
// It returns the shared description when they agree.
func compareLiteral(t *testing.T, src string, old bool) (string, bool) {
	t.Helper()
	raw, cppParsed := cppengine.EvalExpr(src, old)
	cppDesc := parseError
	var cppVal canon.Value
	if cppParsed {
		v, err := canon.Parse(raw)
		if err != nil {
			t.Fatalf("%q (old=%v): decoding libclassad's result %q: %v", src, old, raw, err)
		}
		cppVal, cppDesc = v, canon.Describe(v)
	}
	goVal, goParsed := goEvalExpr(src, old)
	goDesc := describeResult(goVal, goParsed)
	if goParsed != cppParsed || (goParsed && !canon.Equal(goVal, cppVal, canon.DefaultTolerance)) {
		t.Errorf("%q (old=%v): Go=%s libclassad=%s", src, old, goDesc, cppDesc)
		return cppDesc, false
	}
	return cppDesc, true
}

// goDecodeLiteral returns the string the Go engine decodes src to.
func goDecodeLiteral(src string, old bool) (string, bool) {
	parse := classad.ParseExpr
	if old {
		parse = classad.ParseExprOld
	}
	e, err := parse(src)
	if err != nil {
		return "", false
	}
	s, err := e.Eval(classad.New()).StringValue()
	if err != nil {
		return "", false
	}
	return s, true
}

// TestEscapeParityEveryASCIIEscape sweeps "\<c>" for every printable ASCII c in both
// syntaxes. It is exhaustive rather than a table so that a future libclassad change to
// any single escape shows up here, and it additionally asserts the two RULES above
// hold across the whole range -- a table of hand-picked characters would not have
// caught a default arm that erred on everything it did not name.
func TestEscapeParityEveryASCIIEscape(t *testing.T) {
	// The escapes new syntax defines, and the byte each decodes to.
	defined := map[byte]byte{
		'a': '\a', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t', 'v': '\v',
		'\\': '\\', '"': '"', '\'': '\'', '?': '?',
	}
	for c := byte(0x20); c < 0x7f; c++ {
		if c >= '0' && c <= '7' {
			continue // octal escapes, covered below
		}
		src := escapeLiteral("X\\" + string(c) + "Y")

		// New syntax: always parses; a defined escape decodes, anything else
		// keeps the character and drops the backslash.
		want := "X" + string(c) + "Y"
		if d, ok := defined[c]; ok {
			want = "X" + string(d) + "Y"
		}
		if _, ok := compareLiteral(t, src, false); ok {
			if got, _ := goDecodeLiteral(src, false); got != want {
				t.Errorf(`"\%c" new syntax: both engines decode %q, the rule says %q`, c, got, want)
			}
		}

		// Old syntax: the bytes survive, except \" which ends up a literal quote
		// because the quote does not terminate the string.
		if c == '"' {
			continue // \" inside old syntax is its own case, covered by exprParityCases
		}
		if _, ok := compareLiteral(t, src, true); ok {
			want := "X\\" + string(c) + "Y"
			if got, _ := goDecodeLiteral(src, true); got != want {
				t.Errorf(`"\%c" old syntax: both engines decode %q, the rule says %q`, c, got, want)
			}
		}
	}
}

// TestEscapeParityOctal sweeps every octal escape, including the three-digit forms and
// the \0 that makes a literal invalid.
func TestEscapeParityOctal(t *testing.T) {
	for n := 0; n < 0400; n++ {
		for _, digits := range []string{"%o", "%02o", "%03o"} {
			body := "X\\" + fmt.Sprintf(digits, n) + "Y"
			if n > 077 && digits != "%03o" {
				continue // not representable in fewer digits
			}
			compareLiteral(t, escapeLiteral(body), false)
			compareLiteral(t, escapeLiteral(body), true)
		}
	}
	// A digit that cannot continue the octal escape, and one that can.
	for _, body := range []string{`X\08Y`, `X\018Y`, `X\0008Y`, `X\3777Y`, `X\77Y`, `X\400Y`} {
		compareLiteral(t, escapeLiteral(body), false)
		compareLiteral(t, escapeLiteral(body), true)
	}
}

// TestEscapeParityRealWorld pins the production strings that found this, whole.
func TestEscapeParityRealWorld(t *testing.T) {
	for _, body := range []string{
		`warlock21\.beocat\.ksu\.edu|wsu\-lg02\.osris\.org|UA\-LR\-ITS\-EP\.a87b5e635699`,
		`osdf:///chtc/staging/c/ckoch5/tests/space-comma/testing\,\ more`,
		`\S \r (\l)`, // agetty /etc/issue, via OSIssue
		`C:\Users\x`,
		`a\\b`,
		`\`,
		`a\`,
	} {
		compareLiteral(t, escapeLiteral(body), false)
		compareLiteral(t, escapeLiteral(body), true)
	}
	// The whole Requirements expression from the htcondordb report, in the syntax the
	// schedd writes it in. Its regex must keep its backslashes: dropping them leaves a
	// pattern whose '.' matches any character, which quietly matches more hosts.
	const req = `!regexp("warlock21\.beocat\.ksu\.edu|wsu\-lg02\.osris\.org", TARGET.Machine)`
	e, err := classad.ParseExprOld(req)
	if err != nil {
		t.Fatalf("ParseExprOld rejected a real job_queue.log Requirements: %v", err)
	}
	if got := e.String(); !strings.Contains(got, `\.beocat`) {
		t.Errorf("old-syntax parse lost the regex's backslashes: %s", got)
	}
}

// bodyIsOneLiteral reports whether "body" is a single string literal -- that is,
// whether body holds a quote that would close it early. It applies the reference
// scanner's own rule (Lexer::tokenizeString): a quote terminates unless the run of
// backslashes immediately before it has odd length. Deliberately independent of
// either Go lexer, since the inputs this screens out are ones a Go lexer rejects.
func bodyIsOneLiteral(body string) bool {
	odd := false
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '\\':
			odd = !odd
		case '"':
			if !odd {
				return false
			}
			odd = false
		default:
			odd = false
		}
	}
	// A trailing backslash escapes the closing quote, so the literal is unterminated
	// rather than single -- both engines reject it, which is worth fuzzing.
	return true
}

// TestEscapeConcatSplice pins the reference's values for CPP_QUIRKS #18: adjacent
// literals are spliced before convert_escapes runs, so an octal escape straddles the
// join. The Go engine decodes each literal separately and so differs here; the test
// records libclassad's answer rather than asserting agreement, which makes a future
// libclassad change -- or a decision to mirror this -- visible rather than silent.
func TestEscapeConcatSplice(t *testing.T) {
	for _, tc := range []struct{ src, cpp string }{
		{`"\1" "2"`, `string("\n")`},
		{`"\3" "77"`, `string("\xff")`},
		{`"\0""1"`, `string("\x01")`},
		{`"\0"`, parseError},
	} {
		raw, parsed := cppengine.EvalExpr(tc.src, false)
		got := parseError
		if parsed {
			v, err := canon.Parse(raw)
			if err != nil {
				t.Fatalf("%q: decoding libclassad's result %q: %v", tc.src, raw, err)
			}
			got = canon.Describe(v)
		}
		if got != tc.cpp {
			t.Errorf("%q: libclassad now returns %s, recorded %s -- revisit CPP_QUIRKS.md #18",
				tc.src, got, tc.cpp)
		}
	}
}

// escapeFuzzSeeds are string-literal bodies: the real-world cases plus the shapes that
// exercise each arm of both lexers.
var escapeFuzzSeeds = []string{
	`a\.b`, `\S`, `\S \r (\l)`, `C:\Users\x`, `x\\y`, `a\"b`, `\`, `a\`, `\\`,
	`\a\b\f\n\r\t\v\?\'\"`, `\0`, `\00`, `\000`, `\101`, `\08`, `\377`, `\400`,
	`testing\,\ more`, `warlock21\.beocat\.ksu\.edu`, `\x41`, `\u0041`, `a\
b`,
}

// FuzzStringEscapeDifferential mutates the BODY of a string literal and requires both
// engines to agree, in both syntaxes. Scoping the fuzzer to the inside of a literal is
// what makes it productive: FuzzParseExprOldDifferential mutates whole expressions, so
// almost every input it tries dies in the grammar long before it reaches the escape
// decoder, and FuzzDifferential/cafuzz ignore parse-level disagreement entirely -- which
// is how an escape bug stayed invisible to all of them.
func FuzzStringEscapeDifferential(f *testing.F) {
	for _, s := range escapeFuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, body string) {
		// The C side takes a C string, and a literal is lexed, not nested, so
		// length buys no coverage.
		if strings.IndexByte(body, 0) >= 0 || len(body) > 128 {
			t.Skip()
		}
		// Both engines keep a literal's bytes (CPP_QUIRKS #15), but the Go lexers
		// re-encode through runes, mapping an invalid byte to U+FFFD. That is a
		// known, separate difference; keep this target on the escape rules.
		if !utf8.ValidString(body) {
			t.Skip()
		}
		src := escapeLiteral(body)
		// A body holding an unescaped quote splits src into ADJACENT literals, which
		// the reference splices before decoding escapes, so an octal escape can
		// straddle the join (CPP_QUIRKS #18). That is a separate, unmirrored
		// difference; this target is about decoding one literal.
		if !bodyIsOneLiteral(body) {
			t.Skip()
		}
		for _, old := range []bool{false, true} {
			raw, cppParsed := cppengine.EvalExpr(src, old)
			goVal, goParsed := goEvalExpr(src, old)
			if goParsed != cppParsed {
				t.Fatalf("parse disagreement: %q (old=%v): Go parsed=%v, libclassad parsed=%v",
					src, old, goParsed, cppParsed)
			}
			if !goParsed {
				continue
			}
			cppVal, err := canon.Parse(raw)
			if err != nil {
				t.Fatalf("%q (old=%v): decoding libclassad's result %q: %v", src, old, raw, err)
			}
			if !canon.Equal(goVal, cppVal, canon.DefaultTolerance) {
				t.Fatalf("value disagreement: %q (old=%v): Go=%s libclassad=%s",
					src, old, canon.Describe(goVal), canon.Describe(cppVal))
			}
		}
	})
}
