package db

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// The record-scan reduce path: many values per group, reduced by SUM/AVG/MIN/MAX. This is
// where aggAcc accumulates every value and the library aggregate folds the whole slice, so
// it is where computing the answer twice would show.
func benchReduce(b *testing.B, groups, perGroup int, aggs []AggSpec) {
	rows := make([][]classad.Value, 0, groups*perGroup)
	for g := 0; g < groups; g++ {
		owner := classad.NewStringValue(fmt.Sprintf("owner%d", g))
		for i := 0; i < perGroup; i++ {
			rows = append(rows, []classad.Value{owner, classad.NewIntValue(int64(i))})
		}
	}
	groupCols := []GroupCol{{Attr: "Owner"}}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		out, err := aggregateValues(valueSeq(rows), []string{"Owner", "v"},
			groupCols, aggs, []int{0}, aggArgCols(aggs), nil, 1<<20)
		if err != nil {
			b.Fatal(err)
		}
		if len(out) != groups {
			b.Fatalf("got %d groups, want %d", len(out), groups)
		}
	}
}

// aggArgCols points every non-COUNT(*) aggregate at column 1.
func aggArgCols(aggs []AggSpec) []int {
	out := make([]int, len(aggs))
	for i, a := range aggs {
		if a.Arg == "*" {
			out[i] = -1
			continue
		}
		out[i] = 1
	}
	return out
}

func BenchmarkReduceSumWideGroups(b *testing.B) {
	benchReduce(b, 10, 20000, []AggSpec{{Func: AggSum, Arg: "v"}})
}

func BenchmarkReduceFourValueAggs(b *testing.B) {
	benchReduce(b, 10, 20000, []AggSpec{
		{Func: AggSum, Arg: "v"}, {Func: AggAvg, Arg: "v"},
		{Func: AggMin, Arg: "v"}, {Func: AggMax, Arg: "v"},
	})
}

func BenchmarkReduceCountManyGroups(b *testing.B) {
	benchReduce(b, 20000, 10, []AggSpec{{Func: AggCount, Arg: "*"}})
}
