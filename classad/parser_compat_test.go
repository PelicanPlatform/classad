package classad

import "testing"

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
