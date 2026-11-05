package classad

import "testing"

// TestParseOldClassAdLiteralEscapes locks in old-ClassAd string semantics matching the C++
// Lexer::tokenizeStringOld: string literals get NO escape interpretation. An unrecognized
// escape (the classic case: OSIssue = "\S" and the other agetty escapes /etc/issue carries)
// must NOT fail the parse -- otherwise a single such attribute drops the whole ad, which is
// how a Go collector silently lost every forwarded startd ad. Recognized C escapes are kept
// literal too (backslash and all), byte-for-byte with the C++ old parser; only \" collapses
// to a literal quote so an embedded quote does not end the string.
func TestParseOldClassAdLiteralEscapes(t *testing.T) {
	cases := []struct {
		name string
		text string
		attr string
		want string
	}{
		{"unknown escape S", `OSIssue = "\S"`, "OSIssue", `\S`},
		{"agetty issue string", `OSIssue = "\S \r (\l)"`, "OSIssue", `\S \r (\l)`},
		{"known escape kept literal (t)", `A = "tab\there"`, "A", `tab\there`},
		{"known escape kept literal (n)", `A = "a\nb"`, "A", `a\nb`},
		{"backslashes literal", `Path = "C:\Users\x"`, "Path", `C:\Users\x`},
		{"double backslash literal", `A = "x\\y"`, "A", `x\\y`},
		{"escaped quote is a literal quote", `A = "a\"b"`, "A", `a"b`},
		{"plain string unchanged", `A = "hello world"`, "A", "hello world"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ad, err := ParseOld(tc.text)
			if err != nil {
				t.Fatalf("ParseOld(%q) errored: %v", tc.text, err)
			}
			got, ok := ad.EvaluateAttrString(tc.attr)
			if !ok {
				t.Fatalf("ParseOld(%q): attribute %s not a string", tc.text, tc.attr)
			}
			if got != tc.want {
				t.Errorf("ParseOld(%q) %s = %q, want %q", tc.text, tc.attr, got, tc.want)
			}
		})
	}
}

// TestParseNewClassAdUndefinedEscape covers the OTHER half of the reference's leniency, which
// this test previously got backwards by asserting new-ClassAd parsing stays strict. libclassad
// is lenient on both paths; what differs is the value. New syntax runs the literal through
// convert_escapes(), whose default arm passes "char after \ unmodified" -- so the escape is
// accepted and the BACKSLASH IS DROPPED ("\S" is "S"). Old syntax never calls convert_escapes
// at all, so the backslash stays ("\S" is "\S", above).
//
// Erroring here is what made htcondordb skip Requirements on every job carrying an OSPool host
// blocklist; silently keeping the backslash instead would have been worse, since the two
// syntaxes genuinely disagree about the value. fuzz.TestEscapeParity pins both against the
// real libclassad.
func TestParseNewClassAdUndefinedEscape(t *testing.T) {
	cases := []struct{ text, attr, want string }{
		{`[ A = "\S" ]`, "A", "S"},
		{`[ A = "warlock21\.beocat\.ksu\.edu" ]`, "A", "warlock21.beocat.ksu.edu"},
		{`[ A = "x\\y" ]`, "A", `x\y`},
		{`[ A = "a\nb" ]`, "A", "a\nb"},
		{`[ A = "a\ab" ]`, "A", "a\ab"},
		{`[ A = "a\vb" ]`, "A", "a\vb"},
		{`[ A = "a\?b" ]`, "A", "a?b"},
	}
	for _, tc := range cases {
		ad, err := Parse(tc.text)
		if err != nil {
			t.Fatalf("Parse(%q) errored: %v", tc.text, err)
		}
		got, ok := ad.EvaluateAttrString(tc.attr)
		if !ok {
			t.Fatalf("Parse(%q): attribute %s not a string", tc.text, tc.attr)
		}
		if got != tc.want {
			t.Errorf("Parse(%q) %s = %q, want %q", tc.text, tc.attr, got, tc.want)
		}
	}
}
