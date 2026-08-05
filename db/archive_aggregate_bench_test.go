package db

import (
	"fmt"
	"testing"
)

// buildAggArchive creates an archive of n history-shaped records whose Owner is drawn from
// ndv distinct values, indexed categorically. segSize is deliberately small so the archive
// has many sealed segments, which is the shape a real history has and the shape that
// decides whether an aggregate can be answered from the per-segment indexes.
func buildAggArchive(tb testing.TB, n, ndv, segSize int) *ArchiveTable {
	tb.Helper()
	cat, err := OpenCatalog(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { cat.Close() })
	hist, err := cat.CreateArchiveTable("history", ArchiveConfig{
		SegmentSize:      segSize,
		CategoricalAttrs: []string{"Owner"},
		ValueAttrs:       []string{"ClusterId"},
		ZoneAttrs:        []string{"CompletionDate"},
	})
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < n; i++ {
		err := hist.AppendOld(fmt.Sprintf(
			"ClusterId = %d\nCompletionDate = %d\nOwner = %q\nRequestMemory = %d\nJobStatus = 4",
			i, 1700000000+i, fmt.Sprintf("user%04d", i%ndv), 1024+(i%8)*512))
		if err != nil {
			tb.Fatal(err)
		}
	}
	return hist
}

// BenchmarkArchiveGroupCount measures GROUP BY Owner + COUNT(*) over an archive. This is
// the "job count per group" shape, and today it is a scan: Aggregate projects the group
// attribute out of every record in every segment the zone maps cannot prune, which means
// decompressing the whole archive.
//
// ndv is varied because it decides whether the answer is derivable from per-segment
// summaries alone: each sealed segment's index keeps exact counts for its top heavy
// hitters, so a low-cardinality grouping attribute is fully described by resident metadata
// while a high-cardinality one is not.
func BenchmarkArchiveGroupCount(b *testing.B) {
	cases := []struct {
		name string
		n    int
		ndv  int
	}{
		{"n=50k/ndv=8", 50_000, 8},
		{"n=50k/ndv=200", 50_000, 200},
		{"n=200k/ndv=8", 200_000, 8},
		{"n=200k/ndv=200", 200_000, 200},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			hist := buildAggArchive(b, c.n, c.ndv, 1<<20)
			// Correctness guard: a benchmark that measures a wrong answer is worthless.
			rows, err := hist.Aggregate("true", []string{"Owner"}, []AggSpec{{Func: AggCount, Arg: "*"}})
			if err != nil {
				b.Fatal(err)
			}
			if len(rows) != c.ndv {
				b.Fatalf("groups = %d, want %d", len(rows), c.ndv)
			}
			b.ReportMetric(float64(hist.Stats().Segments), "segments")
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := hist.Aggregate("true", []string{"Owner"}, []AggSpec{{Func: AggCount, Arg: "*"}}); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(c.n)*float64(b.N)/b.Elapsed().Seconds()/1e6, "Mrec/s")
		})
	}
}
