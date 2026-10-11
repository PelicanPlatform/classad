//go:build libclassad

// This file compares standalone-expression parsing: classad.ParseExprOld and
// classad.ParseExpr against libclassad's ClassAdParser::ParseExpression with a
// full parse, evaluated in an empty ClassAd. With SetOldClassAd(true) that is
// HTCondor's ParseClassAdRvalExpr, which config macro functions ($INT, $REAL,
// $STRING, $EVAL), submit expressions and job_queue.log values go through.
// FuzzDifferential and cafuzz parse whole ads and ignore parse-level
// disagreement, so lexer differences here were out of their scope.
//
// Run:
//
//	go test -tags libclassad ./fuzz -run TestExprParity
//	go test -tags libclassad ./fuzz -run=xxx -fuzz=FuzzParseExprOldDifferential
package fuzz

import (
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/fuzz/canon"
	cppengine "github.com/PelicanPlatform/classad/fuzz/oracle/cgo"
	"github.com/PelicanPlatform/classad/parser"
)

const parseError = "parse error"

// exprParityCases pin libclassad's result for each input (canon.Describe of the
// value, or parseError) and require the Go parser to match. Most come from the
// golang-htcondor config differential fuzzer (CPP_QUIRKS #1, #10, #14-#17).
var exprParityCases = []struct {
	src  string
	old  bool // ParseExprOld (SetOldClassAd(true)) rather than ParseExpr
	want string
}{
	// '//' starts a comment that runs to the end of the line.
	{"0//", true, "int(0)"},
	{"0 // c", true, "int(0)"},
	{"0//", false, "int(0)"},
	{"1 /* c", true, parseError},
	// A '-' where an operand is expected signs the number after it, which then
	// skips old semantics' leading-zero check (#14).
	{"!-00", true, "true"},
	{"-00", true, "int(0)"},
	{"-00", false, "int(0)"},
	{"(-00)", true, "int(0)"},
	{"--00", true, "int(0)"},
	{"1 is -00", true, "false"},
	{"-07", true, "int(-7)"},
	{"-0x10", true, "int(0)"},
	{"!-0x10", true, "true"},
	{"- 00", true, parseError},
	{"1-00", true, parseError},
	{"(1)-00", true, parseError},
	{"00", true, parseError},
	{"07", true, parseError},
	{"0x10", true, parseError},
	{"-08", true, parseError},
	{"08.5", true, "real(8.5)"},
	{"00e5", true, "real(0)"},
	{"0x1.8", true, "real(1.5)"},
	{"-0x1.8", true, "real(-1.5)"},
	{"0x1F.5e+3", true, "real(31.3671875)"},
	{"0x1.", true, parseError},
	// An integer literal out of the int64 range is 0 (#1).
	{"10000000000000000000%1", true, "int(0)"},
	{"10000000000000000000", true, "int(0)"},
	{"10000000000000000000", false, "int(0)"},
	{"9223372036854775808", true, "int(0)"},
	{"-9223372036854775808", true, "int(-9223372036854775808)"},
	{"- 9223372036854775808", true, "int(0)"},
	{"1-9223372036854775808", true, "int(1)"},
	{"-9223372036854775809", true, "int(0)"},
	// String literals are bytes: one that is not valid UTF-8 keeps its bytes,
	// and strings compare bytewise (#15).
	{"\"\x7f\xb4\" > \"\x7f\x84\"", true, "true"},
	{"\"\x7f\xb4\" > \"\x7f\x84\"", false, "true"},
	{"\"\x7f\xb4\" == \"\x7f\x84\"", true, "false"},
	{"\"\xff\"", true, `string("\xff")`},
	{"strcat(\"\xff\", \"\xfe\")", true, `string("\xff\xfe")`},
	// Whitespace is C isspace: no non-ASCII byte separates tokens.
	{"1\xc2\xa0+1", true, parseError},
	{"1\x85+1", true, parseError},
	{"1\v+1", true, "int(2)"},
	// The whole input must be one expression (#16) ...
	{"0;", true, parseError},
	{"0 ;", false, parseError},
	{"(1?2:3) 4", true, parseError},
	{"1 ? : 2 4", true, parseError},
	// ... except that libclassad returns from a top-level conditional before
	// it checks for the end of the input (#16).
	{"1?2:3 4", true, "int(2)"},
	{"1?2:3;", true, "int(2)"},
	{"0?1:2 #", true, "int(2)"},
	{"1?2:3 \"abc", true, "int(2)"},
	{"0?\"\":\"\"\"", true, `string("")`},
	{"1?2:3 08", true, "int(2)"},
	{"1 ? 2 : 3 ? 4 : 5 6", true, "int(2)"},
	{"1?2:3 -08", true, parseError},
	// Old ClassAd strings (#13, #17): no adjacent-literal concatenation, and
	// a \" that ends the input ends the string with a backslash.
	{`"a" "b"`, true, parseError},
	{`"a" "b"`, false, `string("ab")`},
	{`"C:\"`, true, `string("C:\\")`},
	{`"a\"\"`, true, `string("a\"\\")`},
	{`"a\"x"`, true, `string("a\"x")`},
	{`"a\\"x"`, true, `string("a\\\"x")`},
	{`strcat("a";"b")`, true, `string("ab")`},
	{`'\'`, true, "undefined"},
	{`'a\'b'`, true, "undefined"},
	{"'a\nb'", true, "undefined"},
	{`[a = 1; 'a\'b' = 2].'a\'b'`, true, "int(2)"},
	// A doubled leading dot (#10).
	{"..5", true, "real(0)"},
	{"-..5", true, "real(-0)"},
	{"1 + ..5", true, "real(1)"},
	{"..5e", true, parseError},
	{"a..b", true, "undefined"},
	{"[a=1]..a", true, "int(1)"},
	{"..", true, parseError},
	// A scope prefix is one token with the name after its '.', and only then.
	{"mY. ", true, parseError},
	{"MY.x", true, "undefined"},
}

// goEvalExpr parses src with ParseExprOld (old) or ParseExpr and evaluates it in
// an empty ad, as golang-htcondor's config functions do.
func goEvalExpr(src string, old bool) (canon.Value, bool) {
	parse := classad.ParseExpr
	if old {
		parse = classad.ParseExprOld
	}
	e, err := parse(src)
	if err != nil {
		return canon.Value{}, false
	}
	return canon.FromGoValue(e.Eval(classad.New())), true
}

func describeResult(v canon.Value, parsed bool) string {
	if !parsed {
		return parseError
	}
	return canon.Describe(v)
}

func cppEvalExpr(t *testing.T, src string, old bool) (canon.Value, bool) {
	t.Helper()
	raw, parsed := cppengine.EvalExpr(src, old)
	if !parsed {
		return canon.Value{}, false
	}
	v, err := canon.Parse(raw)
	if err != nil {
		t.Fatalf("%q: decoding libclassad's result %q: %v", src, raw, err)
	}
	return v, true
}

func TestExprParity(t *testing.T) {
	for _, tc := range exprParityCases {
		cppVal, cppParsed := cppEvalExpr(t, tc.src, tc.old)
		if got := describeResult(cppVal, cppParsed); got != tc.want {
			t.Errorf("%q (old=%v): libclassad now returns %s, recorded %s -- "+
				"revisit the Go mirror and fuzz/CPP_QUIRKS.md", tc.src, tc.old, got, tc.want)
		}
		goVal, goParsed := goEvalExpr(tc.src, tc.old)
		if goParsed != cppParsed || (goParsed && !canon.Equal(goVal, cppVal, canon.DefaultTolerance)) {
			t.Errorf("%q (old=%v): Go=%s libclassad=%s", tc.src, tc.old,
				describeResult(goVal, goParsed), describeResult(cppVal, cppParsed))
		}
	}
}

// exprFuzzSeeds are config-function arguments and the cases above.
var exprFuzzSeeds = []string{
	`1 + 2`, `(10 * 3) / 4`, `"a" < "b"`, `strcat("x", "y")`, `size("abc")`,
	`ifThenElse(1 > 0, 1.5, 2)`, `int(3.7)`, `real("1e3")`, `{1, 2}[1]`,
	`[a = 1; b = a + 1].b`, `true && !false`, `1 ? 2 : 3`, `toUpper("ab")`,
}

// valueComparable reports whether the fuzz target compares src's value, not
// just whether it parses: only when it has no identifier, so its value comes
// from literals and operators alone. Attribute references and function calls
// bring in evaluation semantics (scoping, cyclic references, the function
// library) that FuzzDifferential covers with whole ads, and on which the Go
// engine knowingly differs in places (e.g. RE2 vs PCRE2, Unicode case mapping).
func valueComparable(src string) bool {
	lex := parser.NewByteLexer(src)
	for {
		tok, err := lex.Next()
		if err != nil {
			return false
		}
		switch tok.Kind {
		case parser.TokEOF:
			return true
		case parser.IDENTIFIER:
			return false
		}
	}
}

// FuzzParseExprOldDifferential compares ParseExprOld + evaluation in an empty ad
// with libclassad's ParseClassAdRvalExpr recipe on arbitrary bytes. It fails on
// a parse-level or value disagreement and on a Go panic.
func FuzzParseExprOldDifferential(f *testing.F) {
	for _, s := range exprFuzzSeeds {
		f.Add(s)
	}
	for _, tc := range exprParityCases {
		if tc.old {
			f.Add(tc.src)
		}
	}
	f.Fuzz(func(t *testing.T, src string) {
		// The C side takes a C string. A long input is skipped: libclassad's
		// parser refuses nesting past a depth limit the Go parser does not
		// have, and lexing is exercised by short inputs.
		if strings.IndexByte(src, 0) >= 0 || len(src) > 256 {
			t.Skip()
		}
		goVal, goParsed := goEvalExpr(src, true)
		raw, cppParsed, timedOut := cppengine.EvalExprTimeout(src, true, 5*time.Second)
		if timedOut {
			t.Skip() // a libclassad evaluation hang (CPP_QUIRKS #2)
		}
		if goParsed != cppParsed {
			t.Fatalf("parse disagreement: %q: Go parsed=%v, libclassad parsed=%v", src, goParsed, cppParsed)
		}
		if !goParsed {
			return
		}
		if !valueComparable(src) {
			return
		}
		cppVal, err := canon.Parse(raw)
		if err != nil {
			t.Fatalf("%q: decoding libclassad's result %q: %v", src, raw, err)
		}
		// =?= / =!= on lists (CPP_QUIRKS #9): error in Go, a boolean in libclassad.
		if goVal.Kind == canon.KError && cppVal.Kind == canon.KBool && strings.IndexByte(src, '{') >= 0 {
			return
		}
		if !canon.Equal(goVal, cppVal, canon.DefaultTolerance) {
			t.Fatalf("value disagreement: %q: Go=%s libclassad=%s", src, canon.Describe(goVal), canon.Describe(cppVal))
		}
	})
}
