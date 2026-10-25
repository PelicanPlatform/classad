package db

import (
	"testing"
)

// groupColsBenchArchive is buildAggArchive with the columnar accelerator ENABLED, which the index
// tier's own benchmark does not need and this one cannot run without. Built once per arm and shared
// by both arms of a comparison, so the two differ in strategy and not in corpus.
func groupColsBenchArchive(tb testing.TB, n, ndv int) *ArchiveTable {
	tb.Helper()
	hist := buildAggArchive(tb, n, ndv, 1<<20)
	if !hist.BuildAndEnableSchemaScan(8000, 16) {
		tb.Skip("no sealed segments to accelerate")
	}
	return hist
}

// The "per owner per day" aggregate, measured against the record scan it replaces.
//
// Both arms read ONE shared archive and answer the SAME query, so the difference between them is
// the strategy and not the corpus. The scan arm calls the dispatcher's own fall-through directly
// (the same pieces, in the same order, that AggregateColsStats reaches when every fast rung
// declines), rather than a second implementation of grouping that could be slower for its own
// reasons.
//
// The columnar arm asserts it was SERVED before it is timed: GroupedFromColumns declining would
// otherwise be measured as an instant zero and read as an enormous speedup.
func BenchmarkArchiveGroupPerOwnerPerDay(b *testing.B) {
	const day = 86400
	groups := []GroupCol{{Attr: "Owner"}, {Attr: "CompletionDate", BucketWidth: day}}
	for _, c := range []struct {
		name string
		n    int
		ndv  int
		aggs []AggSpec
	}{
		{"count/n=200k/ndv=8", 200_000, 8, []AggSpec{{Func: AggCount, Arg: "*"}}},
		{"count/n=200k/ndv=200", 200_000, 200, []AggSpec{{Func: AggCount, Arg: "*"}}},
		{"stats/n=200k/ndv=200", 200_000, 200, []AggSpec{{Func: AggCount, Arg: "*"},
			{Func: AggMin, Arg: "RequestMemory"}, {Func: AggMax, Arg: "RequestMemory"},
			{Func: AggAvg, Arg: "RequestMemory"}}},
	} {
		hist := groupColsBenchArchive(b, c.n, c.ndv)
		// Correctness guard: a benchmark that measures a wrong answer, or that measures a decline,
		// is worthless.
		cols, served := GroupedFromColumns(hist, "true", groups, c.aggs)
		if !served {
			b.Fatalf("%s: the columnar path declined", c.name)
		}
		scan := scanAggregate(b, hist, "true", groups, c.aggs)
		if len(cols) != len(scan) {
			b.Fatalf("%s: columnar found %d groups, scan %d", c.name, len(cols), len(scan))
		}
		b.Run(c.name+"/columnar", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, ok := GroupedFromColumns(hist, "true", groups, c.aggs); !ok {
					b.Fatal("declined")
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(c.n)*float64(b.N)/b.Elapsed().Seconds()/1e6, "Mrec/s")
		})
		b.Run(c.name+"/scan", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				scanAggregate(b, hist, "true", groups, c.aggs)
			}
			b.StopTimer()
			b.ReportMetric(float64(c.n)*float64(b.N)/b.Elapsed().Seconds()/1e6, "Mrec/s")
		})
	}
}

// BenchmarkArchiveGroupOneColumn is the single-column shape through the same fixture and the same
// entry point, so a change to the packed key shows up here against the number it had before rather
// than only against the scan.
func BenchmarkArchiveGroupOneColumn(b *testing.B) {
	const day = 86400
	aggs := []AggSpec{{Func: AggCount, Arg: "*"}}
	for _, c := range []struct {
		name   string
		groups []GroupCol
	}{
		{"owner", []GroupCol{{Attr: "Owner"}}},
		{"clusterid", []GroupCol{{Attr: "ClusterId"}}},
		{"completiondate_day", []GroupCol{{Attr: "CompletionDate", BucketWidth: day}}},
	} {
		hist := groupColsBenchArchive(b, 200_000, 200)
		if _, served := GroupedFromColumns(hist, "true", c.groups, aggs); !served {
			b.Fatalf("%s: the columnar path declined", c.name)
		}
		b.Run(c.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, ok := GroupedFromColumns(hist, "true", c.groups, aggs); !ok {
					b.Fatal("declined")
				}
			}
			b.StopTimer()
			b.ReportMetric(200_000*float64(b.N)/b.Elapsed().Seconds()/1e6, "Mrec/s")
		})
	}
}
