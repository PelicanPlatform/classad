package classad

import (
	"strings"
	"testing"
	"testing/quick"
	"unicode/utf8"
)

// TestRawBytesRoundTrip guards that a string that is not valid UTF-8 keeps its
// bytes, as libclassad's do: from a literal, through evaluation, and through
// unparse and MarshalOld back to the same value.
func TestRawBytesRoundTrip(t *testing.T) {
	for _, src := range []string{
		"[A = \"\xff\"]",
		"[A = \"a\xec\xecb\"]",
		"[A = \"q\\\"\xfe\"; B = A]",
		"['n\xffm' = \"\xc3\"]",
	} {
		ad, err := Parse(src)
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		want := attrStrings(t, ad)
		for _, again := range []func() (*ClassAd, error){
			func() (*ClassAd, error) { return Parse(ad.String()) },
			func() (*ClassAd, error) { return ParseOld(ad.MarshalOld()) },
		} {
			ad2, err := again()
			if err != nil {
				t.Fatalf("%q: re-parse: %v", src, err)
			}
			if got := attrStrings(t, ad2); len(got) != len(want) {
				t.Fatalf("%q: attributes %q, want %q", src, got, want)
			} else {
				for k, v := range want {
					if got[k] != v {
						t.Errorf("%q: %q = %q after a round trip, want %q", src, k, got[k], v)
					}
				}
			}
		}
	}

	// An octal escape is one byte, as in libclassad's convert_escapes.
	if v, ok := mustParse(t, `[A = "\351\303\251"]`).EvaluateAttrString("A"); !ok || v != "\xe9é" {
		t.Errorf(`"\351\303\251" = %q, want "\xe9é"`, v)
	}
}

func attrStrings(t *testing.T, ad *ClassAd) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range ad.GetAttributes() {
		v, ok := ad.EvaluateAttrString(name)
		if !ok {
			t.Fatalf("%q is not a string", name)
		}
		out[name] = v
	}
	return out
}

// TestStringFunctionsKeepBytes guards that the case-mapping and case-folding
// functions keep a byte that is not valid UTF-8 (rather than turning it into
// U+FFFD), and compare it only with the same byte, as libclassad does. Valid
// UTF-8 is mapped as before.
func TestStringFunctionsKeepBytes(t *testing.T) {
	cases := []struct {
		expr string
		want string
	}{
		{"toUpper(\"a\xffb\")", "S:A\xffB"},
		{"toLower(\"A\xe9B\")", "S:a\xe9b"},
		{"toUpper(\"é\xff\")", "S:É\xff"},
		{"toUpper(\"é\")", "S:É"},
		{"stricmp(\"\xe9\", \"\xc9\")", "I:1"},
		{"stricmp(\"a\xff\", \"A\xff\")", "I:0"},
		{"stringListIMember(\"\xc9\", \"\xe9\")", "B:false"},
		{"stringListIMember(\"A\xff\", \"x,a\xff\")", "B:true"},
		{"stringListIMember(\"É\", \"é\")", "B:true"},
	}
	for _, tc := range cases {
		ad := mustParse(t, "[ x = "+tc.expr+" ]")
		if msg := checkValue(ad.EvaluateAttr("x"), tc.want); msg != "" {
			t.Errorf("%q => %s", tc.expr, msg)
		}
	}
}

// TestValidUTF8Unchanged guards that keeping invalid bytes changed nothing for
// valid UTF-8: the byte-preserving case functions equal the standard ones on
// it, and string comparison (bytewise, case-folding ASCII only, as before) is
// pinned on non-ASCII input.
func TestValidUTF8Unchanged(t *testing.T) {
	samples := []string{"", "abc", "ABC", "é", "É", "straße", "ǆ", "ẞ", "Kelvin K", "日本語", "ſ", "ΣΑΣ", "a b", "\U0001F600x"}
	for _, a := range samples {
		if got, want := toLowerKeepBytes(a), strings.ToLower(a); got != want {
			t.Errorf("toLowerKeepBytes(%q) = %q, want %q", a, got, want)
		}
		if got, want := toUpperKeepBytes(a), strings.ToUpper(a); got != want {
			t.Errorf("toUpperKeepBytes(%q) = %q, want %q", a, got, want)
		}
		for _, b := range samples {
			if got, want := equalFoldKeepBytes(a, b), strings.EqualFold(a, b); got != want {
				t.Errorf("equalFoldKeepBytes(%q, %q) = %v, want %v", a, b, got, want)
			}
		}
	}
	f := func(a, b string) bool {
		if !utf8.ValidString(a) || !utf8.ValidString(b) {
			return true
		}
		return toLowerKeepBytes(a) == strings.ToLower(a) && toUpperKeepBytes(a) == strings.ToUpper(a) &&
			equalFoldKeepBytes(a, b) == strings.EqualFold(a, b)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 20000}); err != nil {
		t.Error(err)
	}

	for _, tc := range []struct{ expr, want string }{
		{`"é" < "z"`, "B:false"}, // bytewise: 0xc3 > 'z'
		{`"é" == "É"`, "B:false"},
		{`"a" == "A"`, "B:true"},
		{`"éa" == "éA"`, "B:true"},
		{`strcmp("é", "É")`, "I:1"},
		{`stricmp("é", "É")`, "I:0"},
		{`toUpper("é")`, "S:É"},
		{`stringListIMember("É", "é")`, "B:true"},
	} {
		ad := mustParse(t, "[ x = "+tc.expr+" ]")
		if msg := checkValue(ad.EvaluateAttr("x"), tc.want); msg != "" {
			t.Errorf("%q => %s", tc.expr, msg)
		}
	}
}
