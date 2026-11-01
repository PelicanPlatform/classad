package classad

import "testing"

func benchRe(b *testing.B, src string) {
	ad, err := Parse(`[ s = "user3"; t = "alice,bob" ]`)
	if err != nil {
		b.Fatal(err)
	}
	expr, err := ParseExpr(src)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = expr.Eval(ad)
	}
}

func BenchmarkRegexpSimple(b *testing.B)  { benchRe(b, `regexp("^user", s)`) }
func BenchmarkRegexpFlags(b *testing.B)   { benchRe(b, `regexp("^USER", s, "i")`) }
func BenchmarkRegexpComplex(b *testing.B) { benchRe(b, `regexp("^(user|admin)[0-9]{1,3}$", s)`) }
func BenchmarkRegexpReplace(b *testing.B) { benchRe(b, `replace("([a-z]+)([0-9]+)", s, "\\2-\\1")`) }

// A baseline with no regexp at all, to separate the evaluator's own cost from the compile.
func BenchmarkStrcmpBaseline(b *testing.B) { benchRe(b, `strcmp(s, "user3")`) }
