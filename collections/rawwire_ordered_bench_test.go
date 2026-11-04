package collections

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// orderedBenchArchive is db's archiveCountFixture, built here so the benchmark can see the
// fan-out counter: declining to fan out is silent, and a benchmark that compared the serial path
// with itself would report a dead heat and look like a verdict.
func orderedBenchArchive(tb testing.TB, n, par int) *Collection {
	tb.Helper()
	codec, cerr := NewZSTDCodec(nil)
	if cerr != nil {
		tb.Fatal(cerr)
	}
	c, err := Open(Options{
		Dir:              tb.TempDir(),
		AppendOnly:       true,
		ReverseScan:      true,
		SegmentSize:      1 << 16,
		QueryParallelism: par,
		Codec:            codec,
		ValueAttrs:       []string{"ClusterId"},
	})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { c.Close() })
	for i := 0; i < n; i++ {
		ad, perr := classad.ParseOld(fmt.Sprintf(
			"ClusterId = %d\nProcId = %d\nJobStatus = %d\nRequestMemory = %d\nRequestCpus = %d\n"+
				"Owner = \"user%d\"\nCmd = \"/home/user%d/run.sh\"\nCompletionDate = %d",
			i, i%10, 1+i%5, 1024+(i%32)*512, 1+i%8, i%512, i%512, 1700000000+i))
		if perr != nil {
			tb.Fatal(perr)
		}
		if aerr := c.Put([]byte(fmt.Sprintf("%d", i)), ad); aerr != nil {
			tb.Fatal(aerr)
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 8) {
		tb.Skip("no sealed segments to accelerate")
	}
	return c
}

// BenchmarkOrderedWireQueryRefs is BenchmarkOrderedWireQuery over the REFS spelling of the read:
// the projection closed under the projected expressions' own references, which is what db's
// QueryRawWireRefs (and so the history mirror) actually calls.
func BenchmarkOrderedWireQueryRefs(b *testing.B) {
	q, err := vm.Parse("JobStatus >= 0")
	if err != nil {
		b.Fatal(err)
	}
	proj := []string{"ClusterId", "Owner", "JobStatus"}
	for _, n := range []int{20000, 100000} {
		ser := orderedBenchArchive(b, n, 1)
		par := orderedBenchArchive(b, n, 0)
		for round := 0; round < 2; round++ {
			arms := []struct {
				name string
				c    *Collection
			}{{"serial", ser}, {"ordered-fanout", par}}
			if round%2 == 1 {
				arms[0], arms[1] = arms[1], arms[0]
			}
			for _, arm := range arms {
				b.Run(fmt.Sprintf("n=%d/r%d/%s", n, round, arm.name), func(b *testing.B) {
					b.ReportAllocs()
					before := wireOrderedFanouts.Load()
					for b.Loop() {
						got := 0
						for range arm.c.QueryRawWireRefs(q, proj, false) {
							got++
						}
						if got != n {
							b.Fatalf("%d rows, want %d", got, n)
						}
					}
					fanned := wireOrderedFanouts.Load() - before
					if arm.name == "ordered-fanout" && fanned == 0 && runtime.GOMAXPROCS(0) > 1 {
						b.Fatal("the parallel arm never fanned out")
					}
					if arm.name == "serial" && fanned != 0 {
						b.Fatalf("the serial arm fanned out %d times", fanned)
					}
				})
			}
		}
	}
}

// THE CONSTRAINED WIRE READ OVER A NEWEST-FIRST ARCHIVE: SERIAL AGAINST THE ORDERED FAN-OUT.
//
// Same rows, same predicate, same projection; the only difference is QueryParallelism, which on
// this path is the difference between the serial reverse walk and runOrderedWireQuery. The arms
// flip between rounds because the first arm warms the allocator.
func BenchmarkOrderedWireQuery(b *testing.B) {
	q, err := vm.Parse("JobStatus >= 0")
	if err != nil {
		b.Fatal(err)
	}
	proj := []string{"ClusterId", "Owner", "JobStatus"}
	for _, n := range []int{20000, 100000} {
		ser := orderedBenchArchive(b, n, 1)
		par := orderedBenchArchive(b, n, 0) // 0 = the default worker cap
		for round := 0; round < 2; round++ {
			arms := []struct {
				name string
				c    *Collection
			}{{"serial", ser}, {"ordered-fanout", par}}
			if round%2 == 1 {
				arms[0], arms[1] = arms[1], arms[0]
			}
			for _, arm := range arms {
				b.Run(fmt.Sprintf("n=%d/r%d/%s", n, round, arm.name), func(b *testing.B) {
					b.ReportAllocs()
					before := wireOrderedFanouts.Load()
					for b.Loop() {
						got := 0
						for range arm.c.QueryRawWire(q, proj, false) {
							got++
						}
						if got != n {
							b.Fatalf("%d rows, want %d", got, n)
						}
					}
					fanned := wireOrderedFanouts.Load() - before
					// At -cpu=1 the auto worker cap IS 1, so the parallel arm is legitimately
					// serial and the two arms coincide; that run measures the serial path on both
					// trees, which is the regression check.
					if arm.name == "ordered-fanout" && fanned == 0 && runtime.GOMAXPROCS(0) > 1 {
						b.Fatal("the parallel arm never fanned out: this compared the serial path with itself")
					}
					if arm.name == "serial" && fanned != 0 {
						b.Fatalf("the serial arm fanned out %d times", fanned)
					}
				})
			}
		}
	}
}

// What an early stop costs: a newest-first LIMIT K. The serial walk frames one segment and reads
// K records; the ordered fan-out has W-1 other workers started on the segments behind it.
func BenchmarkOrderedWireQueryLimit(b *testing.B) {
	q, err := vm.Parse("JobStatus >= 0")
	if err != nil {
		b.Fatal(err)
	}
	proj := []string{"ClusterId", "Owner", "JobStatus"}
	const n = 100000
	ser := orderedBenchArchive(b, n, 1)
	par := orderedBenchArchive(b, n, 0)
	for _, k := range []int{5, 1000, 5000, 20000} {
		for round := 0; round < 2; round++ {
			arms := []struct {
				name string
				c    *Collection
			}{{"serial", ser}, {"ordered-fanout", par}}
			if round%2 == 1 {
				arms[0], arms[1] = arms[1], arms[0]
			}
			for _, arm := range arms {
				b.Run(fmt.Sprintf("k=%d/r%d/%s", k, round, arm.name), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						got := 0
						for range arm.c.QueryRawWire(q, proj, false) {
							got++
							if got == k {
								break
							}
						}
						if got != k {
							b.Fatalf("%d rows, want %d", got, k)
						}
					}
				})
			}
		}
	}
}
