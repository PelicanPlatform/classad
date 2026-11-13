package db

import (
	"fmt"
	"testing"
)

// BenchmarkBandJoinAgainstTheScan prices a band against the thing it replaces: the same
// query with the window written as an ON condition, which pairs every row with every row.
//
// The axis is the WIDTH of the window, because that is what separates the two. The scan's
// pairs are the product of the sides whatever the width; the band's are its matches. At a
// width wide enough to admit most of the side the two converge, and that convergence is as
// much of the claim as the speedup is.
//
// Both arm orders are measured. An A/B that always runs one arm first measures the second
// arm's warm caches, and the orders disagreeing is the signal that is happening.
func BenchmarkBandJoinAgainstTheScan(b *testing.B) {
	const jobs, events = 200, 5000
	for _, width := range []int{0, 4, 32, 256} {
		for first := 0; first < 2; first++ {
			cat := intervalFixture(b, jobs, events, width)
			spec := bandSpec(JoinInner, wholeBand())
			scan := JoinSpec{
				Kind:  JoinInner,
				Left:  JoinSide{Table: "jobs", Alias: "j"},
				Right: JoinSide{Table: "ev", Alias: "e"},
				Cond:  []string{"e.Ts >= j.Lo && e.Ts <= j.Hi"},
			}
			for k := 0; k < 2; k++ {
				banded := (k+first)%2 == 0
				name := "scan"
				run := scan
				if banded {
					name, run = "band", spec
				}
				b.Run(fmt.Sprintf("width%d/order%d/%s", width, first, name), func(b *testing.B) {
					var last JoinStats
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						var st JoinStats
						seq, err := cat.Join(run, JoinLimits{}, &st)
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
						last = st
					}
					b.ReportMetric(float64(last.Pairs), "pairs")
					b.ReportMetric(float64(last.Rows), "rows")
				})
			}
		}
	}
}
