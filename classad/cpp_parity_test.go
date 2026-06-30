package classad

import "testing"

// TestCppParity pins behaviors that were brought into line with the reference
// C++ ClassAd engine (libclassad) after differential fuzzing found Go
// diverging. Each case evaluates `[ x = <expr> ]` and checks the value of x.
//
// The want field is a compact tag:
//
//	U            undefined
//	E            error
//	B:true|false boolean
//	I:<int>      integer
//	R:<float>    real (exact Go %v formatting of the float64)
//	S:<bytes>    string (raw, no quotes)
//
// When adding a fix for a newly found divergence, add the minimal reproducer
// here so it cannot regress.
func TestCppParity(t *testing.T) {
	cases := []struct {
		expr string
		want string
	}{
		// Integer division / modulo: int/int is integer division (toward zero).
		{`1 / 2`, "I:0"},
		{`7 / 2`, "I:3"},
		{`-7 / 2`, "I:-3"},
		{`6 / 3`, "I:2"},
		{`1.0 / 2.0`, "R:0.5"},
		{`7 % 3`, "I:1"},
		{`-7 % 3`, "I:-1"},

		// Case-insensitive string comparison for < <= > >= == != ...
		{`"B" < "a"`, "B:false"},
		{`"a" < "B"`, "B:true"},
		{`"abc" == "ABC"`, "B:true"},
		{`"abc" != "ABC"`, "B:false"},
		// ... but =?= / =!= stay case-sensitive.
		{`"abc" =?= "ABC"`, "B:false"},
		{`"abc" =!= "ABC"`, "B:true"},

		// Booleans are numbers (true=1, false=0) in operators.
		{`true < false`, "B:false"},
		{`true + 1`, "I:2"},
		{`true * 3`, "I:3"},
		{`true == 1`, "B:true"},
		{`2 == true`, "B:false"},
		{`true % 2`, "I:1"},
		{`-true`, "E"}, // ... but unary minus on bool is still an error.

		// Three-valued short-circuit logic.
		{`false && error`, "B:false"},
		{`error && false`, "E"},
		{`true || undefined`, "B:true"},
		{`undefined && false`, "B:false"},
		{`undefined && true`, "U"},
		{`undefined || true`, "B:true"},
		{`!undefined`, "U"},
		{`!1`, "B:false"},
		{`!0`, "B:true"},
		{`1 && true`, "B:true"},
		{`0 && true`, "B:false"},

		// Out-of-range / negative list subscript is an error, not undefined.
		{`{1, 2}[5]`, "E"},
		{`{1, 2}[-1]`, "E"},
		{`{}[0]`, "E"},
		{`{1, 2, 3}[0]`, "I:1"},

		// Unary operators propagate undefined.
		{`-undefined`, "U"},
		{`+undefined`, "U"},

		// Ternary condition coerces a number's truthiness.
		{`1 ? "a" : "b"`, "S:a"},
		{`0 ? "a" : "b"`, "S:b"},
		{`undefined ? 1 : 2`, "U"},
		{`"x" ? 1 : 2`, "E"},

		// Per-function undefined handling: math fns error, string fns propagate.
		{`floor(undefined)`, "E"},
		{`round(undefined)`, "E"},

		// Math functions coerce booleans to numbers (floor/ceiling/round -> int).
		{`round(true)`, "I:1"},
		{`floor(true)`, "I:1"},
		{`ceiling(false)`, "I:0"},
		// round is round-half-to-even (C rint), not half-away-from-zero.
		{`round(2.5)`, "I:2"},
		{`round(3.5)`, "I:4"},
		{`round(0.5)`, "I:0"},
		{`round(-2.5)`, "I:-2"},
		{`round(2.6)`, "I:3"},
		// An integer argument is returned unchanged (no lossy float round-trip).
		{`round(188587117711686808)`, "I:188587117711686808"},
		{`floor(9223372036854775807)`, "I:9223372036854775807"},
		// pow: integer result only for genuine ints with non-negative exponent.
		{`pow(2, 3)`, "I:8"},
		{`pow(2, -1)`, "R:0.5"},
		{`pow(2, true)`, "R:2"},
		{`pow(true, true)`, "R:1"},
		// math builtins parse numeric string arguments (via strtod).
		{`floor("2.5")`, "I:2"},
		{`round("3.5")`, "I:4"},
		{`floor("abc")`, "E"},
		{`pow("2", "3")`, "R:8"},
		{`quantize("10", "3")`, "R:12"},
		{`pow(undefined, 2)`, "E"},
		{`quantize(undefined, 4)`, "E"},
		{`split(undefined)`, "E"},
		{`string(undefined)`, "U"},
		{`bool(undefined)`, "U"},
		{`bool("TRUE")`, "B:true"},
		{`bool("x")`, "U"},
		// case-insensitive >= on strings (regression guard for all 4 orderings)
		{`"Hello" >= "a"`, "B:true"},
		{`"Hello World" >= "a,b,c"`, "B:true"},
		{`strcmp(undefined, "a")`, "U"},

		// toUpper/toLower/strcmp/stricmp coerce non-string scalars to string.
		{`toUpper(5)`, "S:5"},
		{`toUpper(true)`, "S:TRUE"},
		{`toLower(1.5)`, "S:1.500000000000000e+00"},
		{`stricmp(true, "x")`, "I:-1"},

		// length() is not a reference function; it evaluates to error (size()).
		{`length("hello")`, "E"},
		{`length({1, 2})`, "E"},

		// List coercion: string()/strcat()/etc. unparse a list in sink form.
		{`string({})`, "S:{  }"},
		{`string({1, 2})`, "S:{ 1,2 }"},
		{`string({"a", 1})`, `S:{ "a",1 }`},
		{`string({true, undefined})`, "S:{ true,undefined }"},
		{`string({{1}, 2})`, "S:{ { 1 },2 }"},
		{`strcat({1, 2}, "!")`, "S:{ 1,2 }!"},
		{`strcat({1, 2}, x0)`, "U"},
		{`toUpper({1})`, "S:{ 1 }"},
		{`stricmp({}, 1)`, "I:1"},

		// ifThenElse coerces its condition like ?:, and int()/real() coerce bools.
		{`ifThenElse(1, 10, 20)`, "I:10"},
		{`ifThenElse(0, 10, 20)`, "I:20"},
		{`ifThenElse(1.5, 10, 20)`, "I:10"},
		{`ifThenElse(undefined, 1, 2)`, "U"},
		{`ifThenElse("x", 1, 2)`, "E"},
		{`real(true)`, "R:1"},
		{`real(false)`, "R:0"},
		{`int(true)`, "I:1"},

		// int()/real() parse string arguments via strtod.
		{`int("42")`, "I:42"},
		{`int("1.9")`, "I:1"},
		{`int("-3.7")`, "I:-3"},
		{`int(" 5 ")`, "I:5"},
		{`int("1e-10")`, "I:0"},
		{`int("abc")`, "E"},
		{`int("")`, "E"},
		{`real("3.14")`, "R:3.14"},
		{`real("x")`, "E"},

		// quantize: bool coercion, zero base returns the arg unchanged
		// (type preserved), integer ceil-division, and list bases.
		{`quantize(true, false)`, "B:true"},
		{`quantize(7, 0)`, "I:7"},
		{`quantize(5, 3)`, "I:6"},
		{`quantize(8, 3)`, "I:9"},
		{`quantize(true, 2)`, "R:2"},
		{`quantize(5, true)`, "R:5"},
		{`quantize(12, {5, 10, 15, 20})`, "I:15"},
		{`quantize(25, {5, 10, 15, 20})`, "I:40"},

		// substr: an undefined argument dominates over an error one.
		{`substr(error, 1, 2)`, "E"},
		{`substr(error, undefined, 1)`, "U"},
		{`substr(error, error, undefined)`, "U"},
		{`strcmp(error, undefined)`, "U"},
		{`stricmp(undefined, error)`, "U"},
		{`member(error, undefined)`, "U"},

		// member: list/classad target errors; comparison uses == semantics
		// (numeric coercion, case-insensitive) and ignores incomparable items.
		{`member({}, {"x y"})`, "E"},
		{`member(1, {1.0})`, "B:true"},
		{`member("ABC", {"abc"})`, "B:true"},
		{`member(1, {"a", 1})`, "B:true"},
		{`member(5, {"a"})`, "B:false"},

		// Division by zero: integer divisor errors; for a real divisor only a
		// +Inf result is an error, while -Inf and NaN are real values.
		{`1 / 0`, "E"},
		{`1 / 0.0`, "E"},
		{`1.0 / 0.0`, "E"},
		{`-1.0 / 0.0`, "R:-Inf"},
		{`0.0 / 0.0`, "R:NaN"},
		{`5.0 / 2.0`, "R:2.5"},

		// string()/strcat() scalar coercion; reals use %.15E (0 -> "0.0").
		{`string(1.5)`, "S:1.500000000000000E+00"},
		{`string(0.0)`, "S:0.0"},
		{`string(true)`, "S:true"},
		{`string(1)`, "S:1"},
		{`strcat(1, "x")`, "S:1x"},
		{`strcat(true, "x")`, "S:truex"},
		{`strcat(true, undefined)`, "U"},

		// Equality: exact real comparison; mismatched non-numeric types error.
		{`0.1 + 0.2 == 0.3`, "B:false"},
		{`1 == 1.0000000001`, "B:false"},
		{`1.0 == 1`, "B:true"},
		{`undefined == undefined`, "U"},
		{`"a" == 1`, "E"},
		{`{1} == 1`, "E"},
		{`{1, 2} == {1, 2}`, "E"},
		// =?= / =!= cannot compare lists or classads (error), but a type
		// mismatch like list vs int is still a plain boolean.
		{`{1} =?= {1}`, "E"},
		{`{1} =!= {2}`, "E"},
		{`{1} =?= 1`, "B:false"},
	}

	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			ad, err := Parse("[ x = " + tc.expr + " ]")
			if err != nil {
				t.Fatalf("parse error for %q: %v", tc.expr, err)
			}
			got := ad.EvaluateAttr("x")
			if msg := checkValue(got, tc.want); msg != "" {
				t.Errorf("%s => %s", tc.expr, msg)
			}
		})
	}
}

// checkValue returns "" if v matches the want tag, else a description of the
// mismatch.
func checkValue(v Value, want string) string {
	describe := func() string { return v.Type().describe() + "(" + v.String() + ")" }
	switch {
	case want == "U":
		if !v.IsUndefined() {
			return "want undefined, got " + describe()
		}
	case want == "E":
		if !v.IsError() {
			return "want error, got " + describe()
		}
	case want == "B:true":
		if b, _ := v.BoolValue(); !v.IsBool() || !b {
			return "want true, got " + describe()
		}
	case want == "B:false":
		if b, _ := v.BoolValue(); !v.IsBool() || b {
			return "want false, got " + describe()
		}
	case len(want) > 2 && want[:2] == "I:":
		if !v.IsInteger() {
			return "want integer, got " + describe()
		}
		if v.String() != want[2:] {
			return "want " + want[2:] + ", got " + v.String()
		}
	case len(want) > 2 && want[:2] == "R:":
		if !v.IsReal() {
			return "want real, got " + describe()
		}
		if v.String() != want[2:] {
			return "want " + want[2:] + ", got " + v.String()
		}
	case len(want) > 2 && want[:2] == "S:":
		s, _ := v.StringValue()
		if !v.IsString() {
			return "want string, got " + describe()
		}
		if s != want[2:] {
			return "want string " + want[2:] + ", got " + s
		}
	default:
		return "bad want tag: " + want
	}
	return ""
}

func (t ValueType) describe() string {
	switch t {
	case UndefinedValue:
		return "undefined"
	case ErrorValue:
		return "error"
	case BooleanValue:
		return "bool"
	case IntegerValue:
		return "int"
	case RealValue:
		return "real"
	case StringValue:
		return "string"
	case ListValue:
		return "list"
	case ClassAdValue:
		return "classad"
	default:
		return "unknown"
	}
}
