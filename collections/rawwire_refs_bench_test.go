package collections

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// BenchmarkWireRefsVsWholeAd measures the three ways a relay can get rows that EVALUATE at
// the far end, which is the choice this closure exists to settle:
//
//	whole-ad   - ask for every attribute and let the consumer sort the references out. Correct,
//	             and what the mirror did before this op existed.
//	projected  - ask for the projection alone. The floor, not an option: a consumer that
//	             evaluates gets undefined for every expression it receives.
//	refs       - the projection closed under its own references, per record.
//
// ns/row and B/row are both reported because they are paid in different places: the bytes
// cross a socket the relay does not own.
func BenchmarkWireRefsVsWholeAd(b *testing.B) {
	c, _ := refFixture(b, 20000, true)
	proj := []string{"ClusterId", "ProcId", "JobStatus", "Owner", "Requirements"}
	q, err := vm.Parse("JobStatus == 1")
	if err != nil {
		b.Fatal(err)
	}

	bench := func(name string, scan func() (int, int)) {
		b.Run(name, func(b *testing.B) {
			if r, _ := scan(); r == 0 { // warm, and prove the arm matches something
				b.Fatal("matched no rows")
			}
			rows, bytes := 0, 0
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r, by := scan()
				rows, bytes = rows+r, bytes+by
			}
			b.StopTimer()
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(rows), "ns/row")
			b.ReportMetric(float64(bytes)/float64(rows), "B/row")
		})
	}
	drain := func(seq func(yield func([]byte) bool)) (int, int) {
		rows, bytes := 0, 0
		for row := range seq {
			rows, bytes = rows+1, bytes+len(row)
		}
		return rows, bytes
	}
	bench("refs", func() (int, int) { return drain(c.QueryRawWireRefs(q, proj, true)) })
	bench("projected", func() (int, int) { return drain(c.QueryRawWire(q, proj, true)) })
	bench("whole-ad", func() (int, int) { return drain(c.QueryRawWire(q, nil, true)) })
}

// BenchmarkWireRefsUncompressed is BenchmarkWireRefsVsWholeAd with per-record compression
// turned OFF, which is the only way to see what the closure itself costs: zstd is ~78% of a
// mutable read and every arm pays it identically, so with it in place it masks the very
// difference being measured. Not a configuration anyone should run -- it is a measuring
// instrument for deciding whether the closure's repeated walk is worth optimizing.
func BenchmarkWireRefsUncompressed(b *testing.B) {
	dir := b.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "basecodec"), []byte("identity"), 0o644); err != nil {
		b.Fatal(err)
	}
	c, err := Open(Options{Dir: dir, Shards: 4, SegmentSize: 1 << 16})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { c.Close() })
	const n = 20000
	put := func(lo, hi int) {
		for i := lo; i < hi; i++ {
			if err := c.Put([]byte(fmt.Sprintf("%d.%d", i, i%10)), mustAd(b, refFixtureAd(i))); err != nil {
				b.Fatal(err)
			}
		}
	}
	put(0, n*3/4)
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		b.Fatal("no columnar schema derived")
	}
	c.ColumnarizeSealed()
	c.Reindex()
	put(n*3/4, n)

	proj := []string{"ClusterId", "ProcId", "JobStatus", "Owner", "Requirements"}
	q, err := vm.Parse("JobStatus == 1")
	if err != nil {
		b.Fatal(err)
	}
	drain := func(seq func(yield func([]byte) bool)) (int, int) {
		rows, bytes := 0, 0
		for row := range seq {
			rows, bytes = rows+1, bytes+len(row)
		}
		return rows, bytes
	}
	bench := func(name string, scan func() (int, int)) {
		b.Run(name, func(b *testing.B) {
			if r, _ := scan(); r == 0 {
				b.Fatal("matched no rows")
			}
			rows, bytes := 0, 0
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r, by := scan()
				rows, bytes = rows+r, bytes+by
			}
			b.StopTimer()
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(rows), "ns/row")
			b.ReportMetric(float64(bytes)/float64(rows), "B/row")
		})
	}
	bench("refs", func() (int, int) { return drain(c.QueryRawWireRefs(q, proj, true)) })
	bench("projected", func() (int, int) { return drain(c.QueryRawWire(q, proj, true)) })
	bench("whole-ad", func() (int, int) { return drain(c.QueryRawWire(q, nil, true)) })
}
