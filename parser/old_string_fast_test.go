package parser

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

// checkFastPathAgrees requires ParseExprOld's answer for src to be the answer
// the lexer and grammar give, value and error alike, and -- when the fast path
// claims src -- requires that answer to be a string literal holding the value
// the fast path returned.
func checkFastPathAgrees(t *testing.T, src string) {
	t.Helper()
	wantExpr, wantErr := parseExprOldFull(src)
	gotExpr, gotErr := ParseExprOld(src)

	switch {
	case (wantErr == nil) != (gotErr == nil):
		t.Errorf("%q: full path err=%v, ParseExprOld err=%v", src, wantErr, gotErr)
		return
	case wantErr != nil:
		if wantErr.Error() != gotErr.Error() {
			t.Errorf("%q: full path error %q, ParseExprOld error %q", src, wantErr, gotErr)
		}
	case !reflect.DeepEqual(wantExpr, gotExpr):
		t.Errorf("%q: full path %#v, ParseExprOld %#v", src, wantExpr, gotExpr)
		return
	}

	lit, claimed := loneOldStringLiteral(src)
	if !claimed {
		return
	}
	if wantErr != nil {
		t.Errorf("%q: fast path returned %q, full path rejected it: %v", src, lit, wantErr)
		return
	}
	sl, ok := wantExpr.(*ast.StringLiteral)
	if !ok {
		t.Errorf("%q: fast path returned %q, full path parsed %T", src, lit, wantExpr)
		return
	}
	if sl.Value != lit {
		t.Errorf("%q: fast path %q, full path %q", src, lit, sl.Value)
	}
}

// oldStringBodies are the insides of a string literal: every escape shape, the
// backslash runs that decide where a literal ends, and bytes that are not text.
func oldStringBodies() []string {
	bodies := []string{
		"", " ", "  ", "a", "abc", "a b c", "hello world",
		`\`, `a\`, `\a`, `\\`, `a\\b`, `\\\`, `\\\\`, `a\\\b`,
		`\"`, `a\"b`, `\"\"`, `a\"`, `\"a`, `\\"`, `a\\"b`, `\\\"`,
		`"`, `a"b`, `a""b`,
		`C:\tmp`, `C:\\tmp`, `C:\Users\x`, `a\.b\.c`, `\S \r (\l)`,
		`testing\,\ more`,
		"a\nb", "a\tb", "\n", "\t", "\r\n", "\v", "\f",
		"a\x00b", "\x00",
		"naïve", "日本語", "\xff", "a\xffb", "\xc3", "\xed\xa0\x80", "\ufffd",
		"a\\\nb", "a\\\r\nb",
		strings.Repeat("x", 300),
	}
	// Every ASCII escape, bare and embedded.
	for c := byte(0x20); c < 0x7f; c++ {
		bodies = append(bodies, `\`+string(c), `X\`+string(c)+`Y`)
	}
	// Every octal form, in each digit width, plus the ones that stop short.
	for n := 0; n < 0o400; n++ {
		for _, digits := range []string{"%o", "%02o", "%03o"} {
			bodies = append(bodies, "X\\"+fmt.Sprintf(digits, n)+"Y")
		}
	}
	bodies = append(bodies, `X\08Y`, `X\018Y`, `X\0008Y`, `X\3777Y`, `X\77Y`, `X\400Y`)
	// Runs of backslashes before the closing quote, where the run's length
	// decides whether the quote closes the literal.
	for n := 1; n <= 6; n++ {
		bodies = append(bodies, "a"+strings.Repeat(`\`, n), "a"+strings.Repeat(`\`, n)+`"b`)
	}
	return bodies
}

// oldStringWrappers turn a literal body into whole inputs: the literal alone,
// padded, truncated, and as part of a larger expression.
func oldStringWrappers(body string) []string {
	lit := `"` + body + `"`
	return []string{
		lit,
		" " + lit, lit + " ", " " + lit + " ", "\t\n" + lit + "\r\n ",
		"\v" + lit + "\f",
		`"` + body, // unterminated
		// An escaped quote ends the literal when it is the last thing on the
		// line or in the input, and keeps it going anywhere else.
		`"` + body + `\"`,
		`"` + body + `\"` + "\n",
		`"` + body + `\"` + "\n   ",
		`"` + body + `\"` + "\r\n",
		`"` + body + `\"` + "\x00",
		`"` + body + `\"` + "\nx",
		`"` + body + `\"` + "\n" + lit,
		`"` + body + `\"x"`,
		lit + `"`,
		lit + lit,
		lit + " " + lit,
		lit + " + " + lit,
		lit + " == " + lit,
		lit + " =?= " + lit,
		"strcat(" + lit + ", " + lit + ")",
		"(" + lit + ")",
		"{" + lit + "}",
		"[a = " + lit + "]",
		lit + ";",
		lit + " ? 1 : 2",
		"X ? " + lit + " : " + lit,
		lit + " /* c */",
		"/* c */ " + lit,
		lit + " // c",
		"// c\n" + lit,
		"#" + "\n" + lit,
		lit + ".foo",
		lit + "[0]",
		"-" + lit,
		"!" + lit,
	}
}

// TestParseExprOldFastPathDifferential compares ParseExprOld against the lexer
// and grammar over every body and wrapper combination. The fast path is only
// ever a shortcut, so disagreement anywhere is a bug in it.
func TestParseExprOldFastPathDifferential(t *testing.T) {
	for _, body := range oldStringBodies() {
		for _, src := range oldStringWrappers(body) {
			checkFastPathAgrees(t, src)
		}
	}
}

// TestParseExprOldFastPathNonStrings runs the differential over inputs that are
// not string literals at all, which the fast path must decline without
// disturbing them -- including the top-level conditional whose trailing input
// ParseExprOld ignores.
func TestParseExprOldFastPathNonStrings(t *testing.T) {
	for _, src := range []string{
		"", " ", "\n", "0", "1", "-1", "42", "3.14", "true", "false", "undefined",
		"error", "A", "MY.A", "TARGET.Machine", ".A0", "A + 1", "{1, 2, 3}",
		"1 ? 2 : 3", "1 ? 2 : 3 4", `1 ? "a" : "b" junk`, `"a" ? "b" : "c" junk`,
		"regexp(\"a\\.b\", Machine)",
		`!regexp("warlock21\.beocat\.ksu\.edu|wsu\-lg02\.osris\.org", TARGET.Machine)`,
		"(TARGET.RequestCpus <= Cpus) && (TARGET.RequestMemory <= Memory)",
		`ifThenElse(State =?= "Claimed", RemoteOwner, "none")`,
		"$(FOO)", "$INT(x)", "'quoted name'", `'a\'b'`,
		"[", "]", "(", ")", "'", "\\", "+", "?", ":",
	} {
		checkFastPathAgrees(t, src)
	}
}

// TestLoneOldStringLiteralValues pins the values and the decisions, so a change
// that makes the fast path and the full path agree on something wrong still
// fails here.
func TestLoneOldStringLiteralValues(t *testing.T) {
	for _, tc := range []struct {
		src    string
		want   string
		claims bool
	}{
		{`""`, "", true},
		{`"a"`, "a", true},
		{`  "a"  `, "a", true},
		{`"a\nb"`, `a\nb`, true},       // no escape conversion: four bytes
		{`"\S"`, `\S`, true},           // agetty escape from /etc/issue
		{`"C:\\tmp"`, `C:\\tmp`, true}, // both backslashes survive
		{`"a\"b"`, `a"b`, true},        // the one escape there is
		{`"a\"`, `a\`, true},           // an escaped quote at the end of the input ends it
		{"\"a\\\"\n", `a\`, true},      // and at the end of the line
		{`"a" + "b"`, "", false},
		{`"a" == "b"`, "", false},
		{`"a" "b"`, "", false},
		{`strcat("a","b")`, "", false},
		{`"a" ? "b" : "c"`, "", false},
		{"\"a", "", false}, // unterminated
		{`"`, "", false},
		{`"a" // c`, "", false},
		{"1", "", false},
	} {
		got, ok := loneOldStringLiteral(tc.src)
		if ok != tc.claims {
			t.Errorf("loneOldStringLiteral(%q) claimed=%v, want %v", tc.src, ok, tc.claims)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("loneOldStringLiteral(%q) = %q, want %q", tc.src, got, tc.want)
		}
		checkFastPathAgrees(t, tc.src)
	}
}

// FuzzParseExprOldFastPath mutates whole inputs and requires the fast path and
// the full path to stay indistinguishable. It needs no oracle: the full path is
// the oracle.
func FuzzParseExprOldFastPath(f *testing.F) {
	for _, body := range []string{"", "a", `\`, `a\"b`, `\S`, `C:\\tmp`, `a"b`, "a\nb", "\xff"} {
		for _, src := range oldStringWrappers(body) {
			f.Add(src)
		}
	}
	for _, src := range []string{"1", "A + 1", "1 ? 2 : 3 4", `ifThenElse(a, "b", "c")`} {
		f.Add(src)
	}
	f.Fuzz(func(t *testing.T, src string) {
		if len(src) > 256 {
			t.Skip()
		}
		checkFastPathAgrees(t, src)
	})
}
