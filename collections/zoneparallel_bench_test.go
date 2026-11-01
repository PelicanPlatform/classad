package collections

import (
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// zoneBenchN is the corpus size for the fan-out zone-pruning benchmark: large enough that the
// live bytes clear the real fan-out gate without the fixture having to lift it.
const zoneBenchN = 60000

var (
	zoneBenchOnce sync.Once
	zoneBenchColl *Collection
)

// zoneBenchStore builds the archive-shaped fixture ONCE and shares it across every arm:
// append-only, a monotonically increasing timestamp in ZoneAttrs, and segments small enough
// that a scan covers many sealed ones. Building per arm would put each arm on its own segment
// layout and compression state, which moves ns/op by more than the effect under test.
func zoneBenchStore(tb testing.TB) *Collection {
	tb.Helper()
	zoneBenchOnce.Do(func() {
		c := New(Options{
			AppendOnly:       true,
			Shards:           4,
			SegmentSize:      1 << 16,
			ZoneAttrs:        []string{"Ts"},
			QueryParallelism: 8,
		})
		const batch = 512
		b := make([]AdUpdate, 0, batch)
		for i := 0; i < zoneBenchN; i++ {
			ad, err := classad.Parse(fmt.Sprintf(
				`[ MyType = "Job"; TargetType = "Machine"; ClusterId = %d; ProcId = 0; `+
					`Owner = "user%d"; Cmd = "/usr/bin/payload"; Cpus = %d; Memory = %d; `+
					`Status = %d; Ts = %d ]`,
				i/10, i%37, 1+i%8, 1024*(1+i%16), 1+i%5, i))
			if err != nil {
				tb.Fatal(err)
			}
			b = append(b, AdUpdate{Key: []byte("ad-" + strconv.Itoa(i)), Ad: ad})
			if len(b) == batch {
				if err := c.Update(b); err != nil {
					tb.Fatal(err)
				}
				b = b[:0]
			}
		}
		if len(b) > 0 {
			if err := c.Update(b); err != nil {
				tb.Fatal(err)
			}
		}
		zoneBenchColl = c
	})
	return zoneBenchColl
}

// BenchmarkZoneParallelQuery measures the cross-segment fan-out full scan over a zoned
// append-only collection, at three degrees of prunability. The "none" arm is the control: its
// predicate names no zone attribute, so no segment can be skipped and the arm measures only
// what asking costs.
//
// The segs-pruned metric comes from a stats-bearing run outside the timer, so each arm reports
// how much of the snapshot it actually skipped alongside its time.
//
//	go test -run '^$' -bench BenchmarkZoneParallelQuery -benchtime 2s -count 6 .
func BenchmarkZoneParallelQuery(b *testing.B) {
	c := zoneBenchStore(b)
	arms := []struct {
		name string
		q    string
	}{
		// Above the whole Ts range: every sealed segment is provably empty of matches.
		{"prune=all", fmt.Sprintf(`Ts >= %d`, zoneBenchN*10)},
		// The newest ~2%: the shape an archive "recent jobs" scan has.
		{"prune=tail", fmt.Sprintf(`Ts >= %d`, zoneBenchN-zoneBenchN/50)},
		// No zone attribute in the predicate: nothing is prunable, so this is the overhead arm.
		{"prune=none", `Cpus == 3 && Status == 2`},
	}
	for _, arm := range arms {
		q, err := vm.Parse(arm.q)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(arm.name, func(b *testing.B) {
			var st ScanStats
			c.runParallelQuery(c.newQueryPlan(q, nil, &st, false), func(*classad.ClassAd) bool { return true }, false)
			b.ReportAllocs()
			b.ResetTimer()
			rows := 0
			for i := 0; i < b.N; i++ {
				rows = 0
				for range c.Query(q) {
					rows++
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(st.SegmentsPruned), "segs-pruned")
			b.ReportMetric(float64(st.SegmentsTotal), "segs-total")
			b.ReportMetric(float64(rows), "rows")
		})
	}
}
