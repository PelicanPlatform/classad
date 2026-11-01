package vm

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// BenchmarkVecLoop times a counting loop over a batch against the same script run per record.
//
// The spread of trip counts is the variable that matters, because it decides how fast the live set
// collapses. "uniform" is every record going round the same number of times, which is the best case:
// the batch stays full until it empties all at once. "spread" is trips varying across the batch, so
// most trips run with a mostly-empty live set and a full-width pass each time -- the shape the work
// guard exists for, and the one where a batch is worth the least.
func BenchmarkVecLoop(b *testing.B) {
	const n = 8192
	for _, tc := range []struct {
		name  string
		trips func(i int) int
	}{
		{"uniform-8", func(int) int { return 8 }},
		{"spread-1to16", func(i int) int { return 1 + i%16 }},
		{"spread-1to256", func(i int) int { return 1 + i%256 }},
	} {
		recs := make([]map[string]string, n)
		for i := range recs {
			recs[i] = map[string]string{"A": fmt.Sprint(tc.trips(i))}
		}
		src := newTestSource(recs)
		ads := make([]*classad.ClassAd, n)
		for i := range ads {
			ads[i] = src.ad(i)
		}
		sp, err := NewScript().Let("x", mustExprB(b, "0")).
			While(mustExprB(b, "x < A"), func(s *Script) { s.Let("x", mustExprB(b, "x + 1")) }).
			Result(mustExprB(b, "x")).Compile()
		if err != nil {
			b.Fatal(err)
		}
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
