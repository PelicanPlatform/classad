package collections

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// BenchmarkIndexedPlanLateIndex measures a recent-window query over an archive whose value
// index on the timestamp was added after most segments sealed (bounded backfill), so the plan
// goes through the index while most segments have no postings for the attribute. Every segment
// is zone-mapped on it; the query matches only the newest records.
func BenchmarkIndexedPlanLateIndex(b *testing.B) {
	if !mmapSupported {
		b.Skip("persistence is unix-only")
	}
	const (
		segSize = 64 << 10
		old     = 40000
		fresh   = 500
	)
	now := time.Now().Unix()
	a, err := CreateArchive(ArchiveOptions{
		Dir: b.TempDir(), SegmentSize: segSize,
		CategoricalAttrs: []string{"Owner"}, ZoneAttrs: []string{"Ts"},
		IndexBackfillBytes: 3 * segSize,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer a.Close()
	add := func(from, to int, ts func(int) int64) {
		for i := from; i < to; i++ {
			ad, err := classad.Parse(fmt.Sprintf(`[ N = %d; Ts = %d; Owner = "u%d"; Cmd = "/bin/job"; Args = %q ]`,
				i, ts(i), i%17, strings.Repeat("a", 120)))
			if err != nil {
				b.Fatal(err)
			}
			if err := a.Append(ad); err != nil {
				b.Fatal(err)
			}
		}
	}
	add(0, old, func(i int) int64 { return now - 10_000_000 + int64(i)*100 })
	a.Reindex()
	a.AddIndex(nil, []string{"Ts"})
	a.Reindex()
	add(old, old+fresh, func(i int) int64 { return now + 1000 + int64(i) })
	a.Reindex()

	for _, bc := range []struct{ name, expr string }{
		{"range", fmt.Sprintf(`Ts >= %d`, now)},
		{"currenttime", `Ts >= CurrentTime - 600`},
	} {
		q, err := vm.Parse(bc.expr)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(bc.name, func(b *testing.B) {
			var st ScanStats
			for i := 0; i < b.N; i++ {
				st = ScanStats{}
				n := 0
				for range a.QueryProjectStats(q, []string{"N"}, &st) {
					n++
				}
				if n != fresh {
					b.Fatalf("%s returned %d rows, want %d", bc.expr, n, fresh)
				}
			}
			b.ReportMetric(float64(st.RecordsVisited), "visited/op")
			b.ReportMetric(float64(st.SegmentsPruned), "pruned/op")
		})
	}
}
