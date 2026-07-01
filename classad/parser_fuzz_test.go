package classad

import (
	"testing"
	"unicode/utf8"
)

// parserFuzzSeeds are representative ClassAd sources spanning operators,
// builtins, nested ads/lists, subscripts/selection, and lexical edge cases.
// They seed both parser fuzz targets (and, in non-fuzz `go test` runs, execute
// as ordinary regression inputs).
var parserFuzzSeeds = []string{
	``,
	`[]`,
	`[ x = 1 + 2 * 3 - 4 / 5 % 6 ]`,
	`[ a = "s"; b = {1, 2, 3}; c = [ d = a ] ]`,
	`[ x = f(1, 2.5, true, undefined, error) ]`,
	`[ x = 1 ? 2 : 3; y = a =?= b; z = -x + ~y; w = !p ]`,
	`[ x = 0x1f; y = 1e10; z = .5; w = "a\"b\n"; v = 010 ]`,
	`[ x = a.b.c; y = {[m=1],[m=2]}[0].m; z = (0) ?: {}[3] ]`,
	`[ 'odd name' = 1; x = 10 ?: 20 + 3 ]`,
	`[ x = ((((1)))); y = a && b || c; z = p < q <= r ]`,
	`[ x = 1 << 2 >> 3 >>> 4 & 5 | 6 ^ 7 ]`,
	`[ x = -9223372036854775808; y = 9223372036854775807 ]`,
	`[ x = strcat("a", substr("hello", 1, -1)); y = size({1,2}) ]`,
	// Round-trip regressions found by these targets:
	`[ 'a b' = 1; x = 'a b'; 'true' = 2 ]`, // names needing single quotes
	`[ A = 0 .A; b = 1.5 .foo ]`,           // selection on a numeric literal
	"[ x = \"\x14\x01\x7f\" ]",             // control chars in a string literal
}

// FuzzParse checks parser robustness: on ANY input (including non-UTF-8 bytes)
// the parser must not panic or hang, and a successful parse must produce an ad
// whose unparse both succeeds and re-parses (the unparser is what value
// string-coercion relies on). Requires no C++ oracle, so it runs in ordinary CI.
func FuzzParse(f *testing.F) {
	for _, s := range parserFuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		ad, err := Parse(src)
		if err != nil {
			// Rejecting malformed input is fine; also exercise the bare
			// expression entry point on the same bytes.
			if e, eerr := ParseExpr(src); eerr == nil && e != nil {
				_ = e.String()
			}
			return
		}
		if ad == nil {
			t.Fatalf("Parse returned nil ad and nil error for %q", src)
		}
		out := ad.String()
		// The re-parse invariant holds for UTF-8 text; non-UTF-8 string
		// literals are a known lexer limitation (the lexer decodes UTF-8 while
		// bytes round-trip lossily), out of scope here as in FuzzDifferential.
		if utf8.ValidString(src) {
			if _, err := Parse(out); err != nil {
				t.Fatalf("unparse of a parsed ad does not re-parse:\n  in:  %q\n  out: %q\n  err: %v",
					src, out, err)
			}
		}
	})
}

// FuzzRoundTrip checks that unparse is idempotent: once a source has been
// normalized by parse->unparse, parsing and unparsing it again must reproduce
// the identical text. A mismatch is a parser/unparser inconsistency. Go-only.
func FuzzRoundTrip(f *testing.F) {
	for _, s := range parserFuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		// Non-UTF-8 string literals round-trip lossily (known lexer limitation).
		if !utf8.ValidString(src) {
			return
		}
		ad1, err := Parse(src)
		if err != nil {
			return
		}
		s1 := ad1.String()
		ad2, err := Parse(s1)
		if err != nil {
			t.Fatalf("normalized form does not re-parse:\n  src: %q\n  s1:  %q\n  err: %v", src, s1, err)
		}
		if s2 := ad2.String(); s1 != s2 {
			t.Fatalf("unparse is not idempotent:\n  src: %q\n  s1:  %q\n  s2:  %q", src, s1, s2)
		}
	})
}
