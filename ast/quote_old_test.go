package ast

import "testing"

func TestAppendQuoteStringOld(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{``, `""`},
		{`plain`, `"plain"`},
		{`X86_64`, `"X86_64"`},
		// The agetty escape from /etc/issue: two bytes, backslash and S. Old-ClassAd
		// keeps the backslash literal -- it must NOT double to \\S.
		{"\\S", `"\S"`},
		{"C:\\Users\\me", `"C:\Users\me"`},   // literal backslashes preserved
		{"a\\r\\l\\m", `"a\r\l\m"`},          // more agetty escapes, all literal
		{`he said "hi"`, `"he said \"hi\""`}, // embedded quote escaped as \"
		{"back\\slash and \"q\"", `"back\slash and \"q\""`},
	}
	for _, c := range cases {
		if got := string(AppendQuoteStringOld(nil, c.in)); got != c.want {
			t.Errorf("AppendQuoteStringOld(%q) = %s, want %s", c.in, got, c.want)
		}
		if got := string(AppendQuoteStringOldBytes(nil, []byte(c.in))); got != c.want {
			t.Errorf("AppendQuoteStringOldBytes(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}
