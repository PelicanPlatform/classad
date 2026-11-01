package vm

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// benchScriptSource is a block-sized batch of numeric columns, which is what a script's bindings are
// made of in a constraint.
func benchScriptSource(n int) *testSource {
	recs := make([]map[string]string, n)
	for i := range recs {
		recs[i] = map[string]string{
			"Cpus":   fmt.Sprint(1 + i%8),
			"Memory": fmt.Sprint(1024 * (1 + i%16)),
			"Disk":   fmt.Sprint(100 * (1 + i%32)),
		}
	}
	return newTestSource(recs)
}

// BenchmarkVecScript times the same script three ways, because two different questions are being
// asked and only one of them has an interesting answer.
//
// WHAT A BINDING COSTS is the /vector arm against /inlined: the same work written as an expression
// with the bound subexpression substituted back in. A binding is a vector copy per read, so the
// question is whether that copy costs more than it saves -- and where a binding is read twice,
// substituting it means computing it twice, so the script should WIN rather than merely break even.
//
// WHAT VECTORIZING IS WORTH is /vector against /per-record, which runs the same compiled script
// through the register executor, the fastest scalar path the package has. That ratio is a FLOOR
// rather than a measurement: testSource boxes a value per record in LoadColumn, so the fixture's
// column load is most of the vector arm here, where a real block hands over a contiguous column.
// BenchmarkVectorEval in the collections package is the end-to-end number.
//
// The ads are built outside the timer: testSource.ad parses ClassAd source, which would otherwise
// dominate the scalar arm entirely.
func BenchmarkVecScript(b *testing.B) {
	const n = 8192
	src := benchScriptSource(n)
	ads := make([]*classad.ClassAd, n)
	for i := range ads {
		ads[i] = src.ad(i)
	}
	for _, sh := range []struct {
		scriptShape
		inlined string
	}{
		// Read ONCE, so substituting duplicates no work and the script's copy is pure overhead. This
		// is the arm where a binding can only lose.
		{scriptShape{"read-once", [][2]string{{"t", "Cpus * 1024"}}, "Memory > t"},
			"Memory > Cpus * 1024"},
		// Read TWICE, so the inlined form computes the division twice and the binding computes it once.
		{scriptShape{"read-twice", [][2]string{{"t", "Memory / Cpus"}}, "t > 1024 && t < 8192"},
			"(Memory / Cpus) > 1024 && (Memory / Cpus) < 8192"},
		// Two bindings, one of which is read twice.
		{scriptShape{"chained", [][2]string{{"a", "Cpus * 1024"}, {"b", "Memory - a"}}, "b > 0 && Disk > a"},
			"(Memory - Cpus * 1024) > 0 && Disk > Cpus * 1024"},
	} {
		s := NewScript()
		for _, let := range sh.lets {
			e, err := parser.ParseExpr(let[1])
			if err != nil {
				b.Fatal(err)
			}
			s = s.Let(let[0], e)
		}
		re, err := parser.ParseExpr(sh.res)
		if err != nil {
			b.Fatal(err)
		}
		sp, err := s.Result(re).Compile()
		if err != nil {
			b.Fatal(err)
		}
		q, err := Parse(sh.inlined)
		if err != nil {
			b.Fatal(err)
		}
		var scratch VecScratch
		if _, ok := sp.VecEval(src, n, &scratch); !ok {
			b.Fatalf("%s declined", sh.name)
		}
		perElem := func(b *testing.B) {
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*n), "ns/elem")
		}
		b.Run(sh.name+"/vector", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				sp.VecEval(src, n, &scratch)
			}
			perElem(b)
		})
		b.Run(sh.name+"/inlined", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				q.VecEval(src, n, &scratch)
			}
			perElem(b)
		})
		b.Run(sh.name+"/per-record", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				for _, ad := range ads {
					sp.Run(ad)
				}
			}
			perElem(b)
		})
	}
}

// BenchmarkVecPredicate times an If two ways: over a batch, and per record through the register
// executor. The same compiled script in both arms.
//
// Predication does not skip work -- both arms run for every record and the store is what is masked --
// so the question this answers is whether running both arms over a batch still beats running one arm
// per record. The caveat on BenchmarkVecScript's per-record arm applies here too: testSource boxes a
// value per record in LoadColumn, so the ratio is a floor.
func BenchmarkVecPredicate(b *testing.B) {
	const n = 8192
	src := benchScriptSource(n)
	ads := make([]*classad.ClassAd, n)
	for i := range ads {
		ads[i] = src.ad(i)
	}
	build := func(nest bool) *ScriptProgram {
		cond, err := parser.ParseExpr("Cpus > 4")
		if err != nil {
			b.Fatal(err)
		}
		inner, err := parser.ParseExpr("Memory > 8192")
		if err != nil {
			b.Fatal(err)
		}
		hi, err := parser.ParseExpr("Memory * 2")
		if err != nil {
			b.Fatal(err)
		}
		lo, err := parser.ParseExpr("Memory / 2")
		if err != nil {
			b.Fatal(err)
		}
		zero, err := parser.ParseExpr("0")
		if err != nil {
			b.Fatal(err)
		}
		res, err := parser.ParseExpr("x > Disk")
		if err != nil {
			b.Fatal(err)
		}
		s := NewScript().Let("x", zero)
		if nest {
			s = s.If(cond, func(in *Script) {
				in.If(inner, func(t *Script) { t.Let("x", hi) }, func(t *Script) { t.Let("x", lo) })
			}, nil)
		} else {
			s = s.If(cond, func(t *Script) { t.Let("x", hi) }, func(t *Script) { t.Let("x", lo) })
		}
		sp, err := s.Result(res).Compile()
		if err != nil {
			b.Fatal(err)
		}
		return sp
	}
	for _, tc := range []struct {
		name string
		nest bool
	}{{"flat", false}, {"nested", true}} {
		sp := build(tc.nest)
		var scratch VecScratch
		if _, ok := sp.VecEval(src, n, &scratch); !ok {
			b.Fatalf("%s declined", tc.name)
		}
		perElem := func(b *testing.B) {
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*n), "ns/elem")
		}
		b.Run(tc.name+"/vector", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				sp.VecEval(src, n, &scratch)
			}
			perElem(b)
		})
		b.Run(tc.name+"/per-record", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				for _, ad := range ads {
					sp.Run(ad)
				}
			}
			perElem(b)
		})
	}
}
