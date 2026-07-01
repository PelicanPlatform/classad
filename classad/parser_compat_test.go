package classad

import (
	"strings"
	"testing"
)

// TestParserCompat pins parser accept/reject decisions to libclassad's, for
// grammar/lexer differences found by the differential parser fuzzer
// (fuzz.FuzzParseDifferential). Each input is parsed and only its accept/reject
// outcome is checked.
func TestParserCompat(t *testing.T) {
	cases := []struct {
		src   string
		parse bool // true = should parse, false = should be rejected
	}{
		// An unexpected character is a hard error, not silently skipped
		// (previously "[#]" lexed as "[]").
		{`[#]`, false},
		{`[a = 1 @ 2]`, false},
		{`[a = 1]`, true},

		// Integer literals may not have a leading zero (only a bare 0); the
		// reference does not read "010" as octal or decimal.
		{`[a = 0]`, true},
		{`[a = 00]`, false},
		{`[a = 010]`, false},
		{`[a = 007]`, false},
		{`[a = 0.5]`, true},
		{`[a = 0e5]`, true},

		// A fractional float with no integer part (".5") is accepted; "." on
		// its own is still the selection operator.
		{`[a = .5]`, true},
		{`[a = .0]`, true},
		{`[a = .5e2]`, true},
		{`[n = [x=1]; y = n.x]`, true},
		// A decimal point (and exponent) must be followed by a digit; a
		// trailing dot is rejected (the reference rejects "1." and "1.e5").
		{`[a = 1.]`, false},
		{`[a = 5.]`, false},
		{`[a = 0.]`, false},
		{`[a = 1.e5]`, false},
		{`[a = 1.5e3]`, true},
		{`[a = 00.5]`, true},

		// ';' separates assignments but empty statements are allowed anywhere;
		// two assignments with no ';' between them is still an error.
		{`[;a=1]`, true},
		{`[a=1;]`, true},
		{`[a=1;;b=2]`, true},
		{`[;;]`, true},
		{`[ ; ]`, true},
		{`[]`, true},
		{`[;a=1;;b=2;]`, true},
		{`[a=1 b=2]`, false},

		// A leading-dot reference (".A") is accepted (it resolves like "A").
		{`[A=1; B=.A]`, true},
		{`[x=.A]`, true},
		{`[x=.foo.bar]`, true},
		{`[x=.e5]`, true}, // a reference to e5, not a malformed float
		{`[e5=7; x=.e5]`, true},

		// Adjacent string literals concatenate (C-style): "a" "b" is "ab".
		{`[a = "x""y"]`, true},
		{`[a = "x" "y"]`, true},
		{`[a = "x"  "y"  "z"]`, true},
		{`[a = """"]`, true},

		// Identifiers and numbers are ASCII-only; a Unicode letter is not a
		// valid identifier character even though unicode.IsLetter accepts it.
		{`[A=((ǒ))]`, false},
		{`[ǒ=1]`, false},
		{`[café=1]`, false},
		{`[a1=1]`, true},
		{`[_x=1]`, true},

		// Trailing content after a complete top-level ClassAd is rejected --
		// including trailing input that triggers a lexer error, which used to
		// be silently accepted (the Lexer wrapper masked the streaming lexer's
		// error once a complete prefix had been parsed).
		{`[a=1]`, true},
		{`[a=1] `, true},
		{"[a=1]\n", true},
		{`[a=1]x`, false},
		{`[a=1]#`, false},
		{`[a=1]00`, false},
		{`[]00`, false},

		// INT64_MIN parses; a bare 2^63 (positive overflow) does not.
		{`[a = -9223372036854775808]`, true},
		{`[a = 9223372036854775808]`, false},
	}
	for _, tc := range cases {
		_, err := Parse(tc.src)
		if got := err == nil; got != tc.parse {
			t.Errorf("Parse(%q): parsed=%v, want %v (err=%v)", tc.src, got, tc.parse, err)
		}
	}
}

// TestCyclicEvalNoCrash guards against unbounded evaluation recursion: a value
// that references its own attribute through a lazy list element (A = {A[0]})
// once escaped the per-attribute cycle guard and overflowed the goroutine stack
// (an unrecoverable crash). Such cycles must resolve to error, like the
// reference engine, not crash.
func TestCyclicEvalNoCrash(t *testing.T) {
	cases := []struct {
		src, attr, want string
	}{
		{`[A={A[0]}]`, "A", "list[E]"}, // {error}
		{`[A=A]`, "A", "E"},
		{`[A=B; B=A]`, "A", "E"},
		{`[a={1,2}; b=a[0]]`, "b", "I:1"}, // non-cyclic subscript still works
	}
	for _, tc := range cases {
		ad, err := Parse(tc.src)
		if err != nil {
			t.Errorf("%s: parse error: %v", tc.src, err)
			continue
		}
		v := ad.EvaluateAttr(tc.attr)
		if tc.want == "list[E]" {
			elems, lerr := v.ListValue()
			if lerr != nil || len(elems) != 1 || !elems[0].IsError() {
				t.Errorf("%s: %s = %v, want a one-element list of error", tc.src, tc.attr, v)
			}
			continue
		}
		if msg := checkValue(v, tc.want); msg != "" {
			t.Errorf("%s: %s => %s", tc.src, tc.attr, msg)
		}
	}
}

// TestCyclicListStringNoCrash guards the materialization paths that run OUTSIDE
// the recover-protected evaluator entry points -- Value.String() and
// Value.ListValue() -- against a list value that is cyclic only through
// materialization (A = {A}). Each level used to spin up a fresh evaluator at
// depth 0, so the per-evaluator depth guard never tripped and printing/listing
// the value overflowed the goroutine stack. The list's creation depth is now
// carried on the value, so materialization keeps accounting for depth and a
// self-referential list resolves to a finite nesting ending in error -- exactly
// what the reference engine produces (a ~64-deep list[...[error]]).
func TestCyclicListStringNoCrash(t *testing.T) {
	for _, src := range []string{`[A={A}]`, `[A={{A}}]`, `[A={A}; B=A]`} {
		ad, err := Parse(src)
		if err != nil {
			t.Errorf("%s: parse error: %v", src, err)
			continue
		}
		v := ad.EvaluateAttr("A")
		// Must terminate (no stack overflow) and be finite.
		s := v.String()
		if s == "" {
			t.Errorf("%s: empty String()", src)
		}
		elems, lerr := v.ListValue()
		if lerr != nil || len(elems) != 1 {
			t.Errorf("%s: ListValue()=%v,%v want a one-element list", src, elems, lerr)
			continue
		}
		// The self-reference bottoms out in an error once the depth bound is hit.
		if !strings.Contains(s, "error") {
			t.Errorf("%s: String()=%q, want it to bottom out in error", src, s)
		}
	}
}
