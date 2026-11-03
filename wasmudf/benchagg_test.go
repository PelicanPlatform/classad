package wasmudf

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// What a value costs an aggregate, and what the batch buys.
//
// The state crosses the boundary on every fold call, so the per-value cost is the
// guest's own arithmetic plus a share of one state copy and one wasm call. The
// batch is what makes that share small, and these say how small: it is the
// measurement the AggBatch default is set from, and the one that would notice if
// a change started copying per value.

func benchAgg(b *testing.B, batch int) *UDAF {
	b.Helper()
	l, err := NewLoader(b.Context(), Options{AggBatch: batch})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = l.Close(b.Context()) })
	a, err := l.LoadAggregate(b.Context(), "aggsum", mustFixture(b, "aggsum"))
	if err != nil {
		b.Fatal(err)
	}
	return a
}

func mustFixture(b *testing.B, name string) []byte {
	b.Helper()
	w, err := readFixture(name)
	if err != nil {
		b.Fatal(err)
	}
	return w
}

// BenchmarkAggregateAddNum is the columnar rung's hot loop: one numeric column
// value into an accumulator, with no classad.Value in between.
func BenchmarkAggregateAddNum(b *testing.B) {
	for _, batch := range []int{1, 16, 256, 1024, 4096} {
		b.Run(fmt.Sprintf("batch=%d", batch), func(b *testing.B) {
			a := benchAgg(b, batch)
			acc := a.Aggregate().NewAccumulator().(db.ColumnAccumulator)
			v := db.NumValue{I: 7, F: 7, Kind: db.NumInt}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				acc.AddNum(v)
			}
			b.StopTimer()
			acc.Result()
		})
	}
}

// BenchmarkAggregateAdd is the record scan's: one boxed value per row.
func BenchmarkAggregateAdd(b *testing.B) {
	a := benchAgg(b, 1024)
	acc := a.Aggregate().NewAccumulator()
	v := classad.NewIntValue(7)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		acc.Add(v)
	}
	b.StopTimer()
	acc.Result()
}

// BenchmarkAggregateMerge prices the fold the fan-out does once per worker. It is
// two state copies in, one out and one guest call, which is why it can afford to
// be the expensive operation here and the batch cannot.
func BenchmarkAggregateMerge(b *testing.B) {
	a := benchAgg(b, 1024)
	agg := a.Aggregate()
	x := agg.NewAccumulator()
	x.Add(classad.NewIntValue(1))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		y := agg.NewAccumulator()
		y.Add(classad.NewIntValue(1))
		x.Merge(y)
	}
}

// BenchmarkAggregateNewAccumulator prices building one, which the engine does per
// worker and once more to decide whether the aggregate took the column rung at
// all. It is a slice copy: the identity was read from the guest at load.
func BenchmarkAggregateNewAccumulator(b *testing.B) {
	a := benchAgg(b, 1024)
	agg := a.Aggregate()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		sinkAcc = agg.NewAccumulator()
	}
}

var sinkAcc db.Accumulator

// BenchmarkAggregateAddNumTrusted is the same loop against a module the admin
// labelled trusted, which is how much of the per-value cost is wazero's
// interruptibility.
//
// It is worth measuring separately for an aggregate in a way it was not for a
// function. The termination check wazero emits sits at every loop BACK-EDGE, and
// a scalar guest crosses one per call where an aggregate's fold crosses one per
// VALUE -- so the same feature that costs a function a fixed ~340ns per call
// costs an aggregate something per element.
func BenchmarkAggregateAddNumTrusted(b *testing.B) {
	l, err := NewLoader(b.Context(), Options{AggBatch: 1024})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = l.Close(b.Context()) })
	a, err := l.LoadAggregateWith(b.Context(), "aggsum", mustFixture(b, "aggsum"), LoadOptions{Trusted: true})
	if err != nil {
		b.Fatal(err)
	}
	acc := a.Aggregate().NewAccumulator().(db.ColumnAccumulator)
	v := db.NumValue{I: 7, F: 7, Kind: db.NumInt}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		acc.AddNum(v)
	}
	b.StopTimer()
	acc.Result()
}
