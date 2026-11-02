package db

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
)

// Serial against fanned-out, over the same data, for the built-in and for a user-defined aggregate
// that takes column values. Both arms report fanouts/op, so a run where the fan-out silently did not
// happen reads as 0 rather than as a surprisingly small speed-up.

func parBenchFixture(tb testing.TB, n, par int) *DB {
	tb.Helper()
	d, err := OpenConfig(Config{SegmentSize: 1 << 16, QueryParallelism: par})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { d.Close() })
	tx := d.Begin()
	for i := 0; i < n; i++ {
		ad := classad.New()
		ad.InsertAttr("JobStatus", int64(i%5))
		ad.InsertAttr("Cpus", int64(i%64))
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
		if i%20000 == 19999 {
			if err := tx.Commit(); err != nil {
				tb.Fatal(err)
			}
			tx = d.Begin()
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	if !d.EnableSchemaScan(4000, 8) {
		tb.Skip("no sealed segments")
	}
	return d
}

func BenchmarkAggParallel(b *testing.B) {
	name := "pb_sum_col"
	if _, ok := LookupAggregate(name); !ok {
		if err := RegisterAggregate(name, benchSumCol{}); err != nil {
			b.Fatal(err)
		}
	}
	for _, n := range []int{25000, 50000, 100000, 200000, 400000, 800000} {
		// The arm order flips with each size so a systematic bias from running one arm first --
		// cache warmth, a drifting machine -- shows up as a sign change rather than as a result.
		order := [][]int{{1, 0}, {0, 1}}[n/25000%2]
		for _, par := range order {
			d := parBenchFixture(b, n, par)
			lbl := "serial"
			if par == 0 {
				lbl = "parallel"
			}
			for _, c := range []struct {
				what string
				aggs []AggSpec
			}{
				{"builtin-sum", []AggSpec{{Func: AggSum, Arg: "Cpus"}}},
				{"user-sum", []AggSpec{{Func: AggUser, UserFunc: name, Arg: "Cpus"}}},
			} {
				b.Run(fmt.Sprintf("n=%d/%s/%s", n, c.what, lbl), func(b *testing.B) {
					before := collections.AggFanouts()
					for i := 0; i < b.N; i++ {
						if _, err := d.AggregateColsStats("true", nil, c.aggs, nil, nil); err != nil {
							b.Fatal(err)
						}
					}
					b.ReportMetric(float64(collections.AggFanouts()-before)/float64(b.N), "fanouts/op")
				})
			}
		}
	}
}
