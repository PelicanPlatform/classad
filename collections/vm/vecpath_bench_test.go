package vm

import (
	"fmt"
	"testing"
)

// benchPathSource is a block-sized batch whose A column is either entirely below the threshold the
// benchmark tests or entirely above it.
//
// Entirely, rather than mixed, because the question is what happens when a path is needed by NO
// record -- which is what a selective constraint over clustered or sorted data looks like, and the
// only case where anything can be skipped. The mixed case is covered by the non-selective arm, which
// is there to show the skip costs nothing when it does not fire.
func benchPathSource(n int, above bool) *testSource {
	recs := make([]map[string]string, n)
	for i := range recs {
		a := 1 + i%8
		if above {
			a = 100 + i%8
		}
		recs[i] = map[string]string{
			"A": fmt.Sprint(a), "B": fmt.Sprint(1024 * (1 + i%16)), "C": fmt.Sprint(i % 97),
		}
	}
	return newTestSource(recs)
}

// BenchmarkVecPath times the three constructs that can now skip a path they would otherwise have
// evaluated for everybody.
//
// The /selective arm is the one where nothing takes the path; /mixed is where everything does. Both
// are reported because a skip that paid for itself only when it fired would be a bad trade: the
// second arm is what says it costs nothing when it does not.
func BenchmarkVecPath(b *testing.B) {
	const n = 8192
	for _, expr := range []string{
		`A > 50 && B > 2048 && C < 50`,
		`ifThenElse(A > 50, B + C, B - C)`,
		`A ?: (B + C)`,
	} {
		q, err := Parse(expr)
		if err != nil {
			b.Fatal(err)
		}
		for _, tc := range []struct {
			name  string
			above bool
		}{{"selective", false}, {"mixed", true}} {
			src := benchPathSource(n, tc.above)
			var scratch VecScratch
			if _, ok := q.VecEval(src, n, &scratch); !ok {
				b.Fatalf("%q declined", expr)
			}
			b.Run(expr+"/"+tc.name, func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					q.VecEval(src, n, &scratch)
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*n), "ns/elem")
			})
		}
	}
}
