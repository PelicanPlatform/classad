package collections

import (
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

func populateInto(tb testing.TB, c *Collection, sample []*classad.ClassAd, n int) {
	tb.Helper()
	const batch = 512
	b := make([]AdUpdate, 0, batch)
	flush := func() {
		if len(b) > 0 {
			if err := c.Update(b); err != nil {
				tb.Fatal(err)
			}
			b = b[:0]
		}
	}
	for i := 0; i < n; i++ {
		b = append(b, AdUpdate{Key: []byte("ad-" + strconv.Itoa(i)), Ad: sample[i%len(sample)]})
		if len(b) == batch {
			flush()
		}
	}
	flush()
}

// BenchmarkParallelQuery measures single-query latency of a full scan at increasing
// worker counts (par=1 is the serial baseline), across collection size and query
// selectivity. Read the speedup down each par column; the small-size rows show where
// fan-out overhead outweighs the work (why the real path is threshold-gated).
//
//	go test -run '^$' -bench BenchmarkParallelQuery -benchtime 300ms .
func BenchmarkParallelQuery(b *testing.B) {
	sample := loadCorpus(b)
	sizes := []struct {
		name string
		n    int
	}{
		{"tiny", 200}, // below the real threshold; shows fan-out overhead when forced
		{"small", 2000},
		{"large", 40000},
	}
	sels := []struct {
		name string
		q    string
	}{
		{"low", `Memory >= 0`},          // matches ~all -> decode-heavy
		{"high", `Memory == 987654321`}, // matches ~none -> match/reject-only
	}
	pars := []int{1, 2, 4, 8}

	for _, sz := range sizes {
		for _, par := range pars {
			c := New(Options{Shards: 8, SegmentSize: 1 << 14, QueryParallelism: par})
			if par >= 2 {
				c.parallelMinBytes = 0 // force fan-out so small-size overhead is visible
			}
			populateInto(b, c, sample, sz.n)
			for _, sel := range sels {
				q := mustQuery(b, sel.q)
				name := fmt.Sprintf("size=%s/sel=%s/par=%d", sz.name, sel.name, par)
				b.Run(name, func(b *testing.B) {
					b.ReportAllocs()
					b.ResetTimer()
					var matches int
					for i := 0; i < b.N; i++ {
						matches = 0
						for range c.Query(q) {
							matches++
						}
					}
					b.StopTimer()
					b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(sz.n), "ns/ad")
					_ = matches
				})
			}
		}
	}
}

// BenchmarkQueryContention measures aggregate throughput when the number of
// concurrent queries equals the core count -- the regime where per-query fan-out
// competes with inter-query concurrency. It contrasts the real machine-wide worker
// budget ("budget=on") with an unbounded one ("budget=off", each query gets its full
// worker count) so the effect of oversubscription is visible: does throughput
// plateau, degrade gracefully, or collapse as workers-per-query grows?
//
//	go test -run '^$' -bench BenchmarkQueryContention -benchtime 2s .
func BenchmarkQueryContention(b *testing.B) {
	sample := loadCorpus(b)
	q := mustQuery(b, `Cpus >= 8`) // ~half match: real per-query decode work (CPU-bound)
	clients := runtime.GOMAXPROCS(0)

	mkColl := func(par int, budgeted bool) *Collection {
		c := New(Options{Shards: 8, SegmentSize: 1 << 14, QueryParallelism: par})
		if par >= 2 {
			c.parallelMinBytes = 0 // force fan-out
			if !budgeted {
				c.qsem = make(chan struct{}, clients*par) // non-binding: full fan-out per query
			}
		}
		populateInto(b, c, sample, 20000)
		return c
	}
	run := func(name string, c *Collection) {
		b.Run(name, func(b *testing.B) {
			b.ResetTimer()
			var next int64
			var wg sync.WaitGroup
			for w := 0; w < clients; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for atomic.AddInt64(&next, 1) <= int64(b.N) {
						for range c.Query(q) {
						}
					}
				}()
			}
			wg.Wait()
			b.StopTimer()
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "queries/s")
		})
	}

	// Baseline: every one of the `clients` queries runs serial (one core each) -- the
	// cores are already fully utilized, so this is the throughput to beat.
	run(fmt.Sprintf("clients=%d/par=1", clients), mkColl(1, true))
	for _, par := range []int{2, 4, 8} {
		run(fmt.Sprintf("clients=%d/budget=on/par=%d", clients, par), mkColl(par, true))
		run(fmt.Sprintf("clients=%d/budget=off/par=%d", clients, par), mkColl(par, false))
	}
}
