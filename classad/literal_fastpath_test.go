package classad

import "testing"

// TestLiteralAttrFastPath documents the evalAttrExpr fast path: a literal
// attribute value skips cyclic-detection bookkeeping. Correctness hinges on the
// fact that a literal references nothing, so it can neither be part of a cycle nor
// depend on scope. These cases exercise both the fast path (literal values) and
// that genuine cycles -- which necessarily run through non-literal references --
// are still detected.
func TestLiteralAttrFastPath(t *testing.T) {
	cases := []struct {
		src  string
		attr string
		want string // "I:n", "S:s", "B:t/f", "U", "E"
	}{
		// Literal values: fast path.
		{`[a = 5]`, "a", "I:5"},
		{`[a = "hi"]`, "a", "S:hi"},
		{`[a = true]`, "a", "B:true"},
		{`[a = undefined]`, "a", "U"},
		{`[a = error]`, "a", "E"},
		// A reference chain ending in a literal: intermediate refs take the slow
		// path, the literal takes the fast path; no false cycle.
		{`[a = 7; b = a; c = b]`, "c", "I:7"},
		// A literal referenced from two attributes: fast path both times, no
		// leftover marker from the first evaluation.
		{`[a = 9; b = a + a]`, "b", "I:18"},
		// Genuine cycles run through non-literal references and must still error.
		{`[a = a]`, "a", "E"},
		{`[a = b; b = a]`, "a", "E"},
		{`[a = a + 1]`, "a", "E"},
	}
	for _, tc := range cases {
		ad, err := Parse(tc.src)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.src, err)
		}
		v := ad.EvaluateAttr(tc.attr)
		got := shortValue(v)
		if got != tc.want {
			t.Errorf("%s .%s = %s, want %s", tc.src, tc.attr, got, tc.want)
		}
	}
}

func shortValue(v Value) string {
	switch {
	case v.IsUndefined():
		return "U"
	case v.IsError():
		return "E"
	case v.IsBool():
		b, _ := v.BoolValue()
		if b {
			return "B:true"
		}
		return "B:false"
	case v.IsInteger():
		i, _ := v.IntValue()
		return "I:" + itoa(i)
	case v.IsString():
		s, _ := v.StringValue()
		return "S:" + s
	}
	return "?"
}

func itoa(i int64) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}
