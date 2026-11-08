package vm

import (
	"strconv"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// The spine benchmarks measure a machine-generated membership constraint --
// "X == 1 || X == 2 || ..." -- which is a left-deep || spine with one term per
// candidate key. The ad matches the last term, so every term is evaluated.

func benchSpine(b *testing.B, n int) (*Program, *classad.ClassAd) {
	b.Helper()
	expr, err := parser.ParseExpr(spineSource(n))
	if err != nil {
		b.Fatal(err)
	}
	ad, err := classad.Parse(`[X = ` + strconv.Itoa(n) + `]`)
	if err != nil {
		b.Fatal(err)
	}
	return CompileProgram(expr), ad
}

func BenchmarkSpineEval64(b *testing.B)   { benchSpineEval(b, 64) }
func BenchmarkSpineEval1000(b *testing.B) { benchSpineEval(b, 1000) }
func BenchmarkSpineEval5000(b *testing.B) { benchSpineEval(b, 5000) }

func benchSpineEval(b *testing.B, n int) {
	p, ad := benchSpine(b, n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if v := Run(p, ad); !v.IsBool() {
			b.Fatalf("got %s, want a boolean", describe(v))
		}
	}
}

func BenchmarkSpineCompile1000(b *testing.B) {
	expr, err := parser.ParseExpr(spineSource(1000))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = CompileProgram(expr)
	}
}
