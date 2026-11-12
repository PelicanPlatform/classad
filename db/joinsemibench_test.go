package db

import (
	"fmt"
	"testing"
)

// BenchmarkAntiJoinAgainstItsEmulation is the comparison the kind exists for: a native anti
// join against the `LEFT JOIN ... WHERE <key> =?= undefined` that callers write today.
//
// fan is the axis. The emulation forms a pair per history row of every matching cluster and
// drops each one in the residual WHERE; the native kind forms one per job and stops. At fan
// 1 the two do the same work, and the gap should open linearly from there -- so an arm that
// wins at fan 1 by more than noise is measuring something other than this.
//
// Both arms run under the same fixture and BOTH ORDERS, because an arm that always runs
// second measures a warmed page cache as though it were an algorithm.
func BenchmarkAntiJoinAgainstItsEmulation(b *testing.B) {
	for _, fan := range []int{1, 2, 8, 32} {
		// A benchmark fixture, not the correctness one: semiFixture seals every 512 rows,
		// and at fan 32 that is 85 segments whose per-open cost swamps the pair loop this
		// is comparing. It showed as the same arm measuring 101ms in one order and 59ms in
		// the other.
		cat := semiFixtureSized(b, 2000, fan, 65536)

		native := semiSpec(JoinAnti)
		emul := semiSpec(JoinLeft)
		emul.Post = []string{"h.ClusterId =?= undefined"}

		arms := []struct {
			name string
			spec JoinSpec
		}{{"native", native}, {"emulation", emul}}
		for _, first := range []int{0, 1} {
			for k := range arms {
				a := arms[(k+first)%2]
				b.Run(fmt.Sprintf("fan%d/%s/order%d", fan, a.name, first), func(b *testing.B) {
					var pairs, rows int
					for b.Loop() {
						var st JoinStats
						seq, err := cat.Join(a.spec, JoinLimits{}, &st)
						if err != nil {
							b.Fatal(err)
						}
						n := 0
						for range seq {
							n++
						}
						if st.Err != nil {
							b.Fatal(st.Err)
						}
						pairs, rows = st.Pairs, n
					}
					b.ReportMetric(float64(pairs), "pairs")
					b.ReportMetric(float64(rows), "rows")
				})
			}
		}
	}
}
