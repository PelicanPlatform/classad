package classad

import "testing"

func TestUnparseExprMatchesReference(t *testing.T) {
	// want values are what C++ `string({ <expr> })` yields, minus the "{ }".
	cases := map[string]string{
		`(a + 1)`:                     `(a + 1)`,
		`1 + 1`:                       `1 + 1`,
		`(-5)`:                        `(-5)`,
		`(-a)`:                        `( -a)`,
		`(!a)`:                        `( !a)`,
		`(a =?= b)`:                   `(a is b)`,
		`(a =!= b)`:                   `(a isnt b)`,
		`f(1, 2)`:                     `f(1,2)`,
		`a.b`:                         `a.b`,
		`a[2]`:                        `a[2]`,
		`(a + b * c)`:                 `(a + b * c)`,
		`((a + b) * c)`:               `((a + b) * c)`,
		`(undefined ? error : false)`: `(undefined ? error : false)`,
		`-2.25`:                       `-2.250000000000000E+00`,
		`{1, 1+1}`:                    `{ 1,1 + 1 }`,
		`"a b"`:                       `"a b"`,
	}
	for src, want := range cases {
		e, err := ParseExpr(src)
		if err != nil {
			t.Errorf("parse %q: %v", src, err)
			continue
		}
		got := unparseExprString(e.internal())
		if got != want {
			t.Errorf("unparse %q = %q, want %q", src, got, want)
		}
	}
}
