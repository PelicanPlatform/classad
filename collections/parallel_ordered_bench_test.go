package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// What the ordered fan-out is worth to the full-decode read on a newest-first collection.
//
// Both arms read the SAME collection, built once: a store per arm would give each its own page
// cache and dictionary state, and the comparison would carry that difference as if it were the
// one being measured. Parallelism is switched per read through the collection's own knob rather
// than by rebuilding, so nothing but the fan-out differs between the arms.
//
// The LIMIT arm is here because it is the shape an archive actually serves -- condor_history asks
// for the newest few -- and the shape a fan-out most easily makes WORSE. A serial newest-first
// read of 5 ads never leaves the newest segment; a fan-out that started workers behind it would
// throw that work away. The prelude exists to keep that case at parity, so a regression here
// matters as much as a win on the full scan.
func BenchmarkOrderedQuery(b *testing.B) {
	const n = 100000
	c := orderedBenchArchive(b, n, 8)

	read := func(b *testing.B, constraint string, par, limit int) {
		q, err := vm.Parse(constraint)
		if err != nil {
			b.Fatal(err)
		}
		c.queryPar = par
		// A warm pass before the timer: the segments are mmap'd and the first read pays page-in
		// no later read pays.
		got := 0
		for range c.Query(q) {
			got++
			if limit > 0 && got == limit {
				break
			}
		}
		if limit == 0 && got == 0 {
			b.Fatal("the constraint matched nothing; this would time an empty scan")
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			seen := 0
			for range c.Query(q) {
				seen++
				if limit > 0 && seen == limit {
					break
				}
			}
			if seen != got {
				b.Fatalf("%d ads, want %d", seen, got)
			}
		}
		b.StopTimer()
		b.ReportMetric(float64(got), "ads")
	}

	for _, shape := range []struct {
		name       string
		constraint string
		limit      int
	}{
		{"fullscan", "JobStatus >= 0", 0},
		{"selective", "ProcId == 7", 0},
		{"limit5", "JobStatus >= 0", 5},
	} {
		for _, arm := range []struct {
			name string
			par  int
		}{{"serial", 1}, {"ordered", 8}} {
			b.Run(fmt.Sprintf("%s/%s", shape.name, arm.name), func(b *testing.B) {
				read(b, shape.constraint, arm.par, shape.limit)
			})
		}
	}
}
