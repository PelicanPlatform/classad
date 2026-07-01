package canon

import "testing"

func TestEncodeParseRoundTrip(t *testing.T) {
	cases := []Value{
		{Kind: KUndef},
		{Kind: KError},
		{Kind: KBool, B: true},
		{Kind: KBool, B: false},
		{Kind: KInt, I: 0},
		{Kind: KInt, I: -42},
		{Kind: KInt, I: 9223372036854775807},
		{Kind: KReal, R: 0.5},
		{Kind: KReal, R: -2.25},
		{Kind: KReltime, R: 90},
		{Kind: KAbstime, R: 1325376000, Off: 0},
		{Kind: KString, S: ""},
		{Kind: KString, S: "hi, there; with S5,specials"},
		{Kind: KList, List: []Value{{Kind: KInt, I: 1}, {Kind: KString, S: "x"}}},
		{Kind: KClassad, Map: []Attr{
			{Key: "a", Val: Value{Kind: KInt, I: 1}},
			{Key: "b", Val: Value{Kind: KList, List: []Value{{Kind: KBool, B: true}}}},
		}},
	}
	for _, c := range cases {
		enc := Encode(c)
		got, err := Parse(enc)
		if err != nil {
			t.Fatalf("Parse(%q) error: %v", enc, err)
		}
		if !Equal(got, c, DefaultTolerance) {
			t.Errorf("round trip mismatch: enc=%q\n got=%s\nwant=%s", enc, Describe(got), Describe(c))
		}
	}
}

func TestEqualTypeStrict(t *testing.T) {
	i := Value{Kind: KInt, I: 1}
	r := Value{Kind: KReal, R: 1}
	if Equal(i, r, DefaultTolerance) {
		t.Error("int(1) and real(1.0) must NOT be canonically equal")
	}
}

func TestStringWithDelimiters(t *testing.T) {
	// strings carrying canonical-format metacharacters must survive round trip
	s := Value{Kind: KString, S: "U;,I5R3.0;C2"}
	got, err := Parse(Encode(s))
	if err != nil || got.Kind != KString || got.S != s.S {
		t.Fatalf("delimiter-laden string failed: got=%+v err=%v", got, err)
	}
}
