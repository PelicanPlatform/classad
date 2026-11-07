package collections

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// parWireBenchStore builds the store BenchmarkParallelWireQuery reads: persistent, compressed,
// interned and columnarized, which is the state a long-running mirror settles into and the only
// one in which the columnar prefilter each worker carries runs at all.
//
// The ads are wide (nine attributes beyond the projected ones) because a projection that keeps
// almost every attribute has nothing to narrow, and the row assembly the workers parallelize is
// then most of what the serial arm does too -- a fixture that would understate both arms' spread
// and read as a verdict about fan-out.
func parWireBenchStore(tb testing.TB, n, par int) *Collection {
	tb.Helper()
	codec, cerr := NewZSTDCodec(nil)
	if cerr != nil {
		tb.Fatal(cerr)
	}
	c, err := Open(Options{
		Dir:              tb.TempDir(),
		Shards:           4,
		SegmentSize:      1 << 16,
		QueryParallelism: par,
		Codec:            codec,
	})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { c.Close() })
	for i := 0; i < n; i++ {
		src := fmt.Sprintf(
			"MyType = \"Job\"\nTargetType = \"Machine\"\nClusterId = %d\nProcId = %d\nJobStatus = %d\n"+
				"Owner = \"user%d\"\nRequestMemory = %d\nRequestCpus = %d\nCmd = \"/home/user%d/run.sh\"\n"+
				"Iwd = \"/home/user%d/work/%d\"\nArgs = \"--input data-%d.root --events %d\"\n"+
				"Env = \"PATH=/usr/bin:/bin;HOME=/home/user%d\"\nJobUniverse = 5\nQDate = %d\n"+
				"TransferInput = \"in-%d.tar.gz\"\nRequirements = \"OpSys == \\\"LINUX\\\"\"\n",
			i, i%10, 1+i%5, i%64, 1024+(i%32)*512, 1+i%8, i%64, i%64, i, i, 1000+i%997, i%64,
			1700000000+i, i)
		ad, perr := classad.ParseOld(src)
		if perr != nil {
			tb.Fatal(perr)
		}
		if aerr := c.Put([]byte(fmt.Sprintf("%d.%d", i, i%10)), ad); aerr != nil {
			tb.Fatal(aerr)
		}
	}
	if _, rerr := c.RetrainDict(2 * n); rerr != nil {
		tb.Fatal(rerr)
	}
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		tb.Skip("no columnar schema derived: the prefilter the workers carry would not run")
	}
	c.ColumnarizeSealed()
	c.Reindex()
	if columnarSegmentsForTest(c) == 0 {
		tb.Skip("no segment columnarized: the prefilter the workers carry would not run")
	}
	return c
}

// THE CONSTRAINED WIRE READ: SERIAL AGAINST THE STEALING FAN-OUT.
//
// Both arms read ONE store, flipping only its worker cap between them. Two stores would differ in
// their mmap and in how much of it the page cache holds, and that difference lands entirely on
// whichever arm ran first -- so the measurement would be of the fixture, not of the fan-out.
//
// The arms swap places between rounds for the same reason: the first arm of a pair warms the
// allocator and the mapping, and a single before/after pair would report that warming as the win.
//
// The fan-out counter is checked per arm because declining to fan out is SILENT -- the read runs
// serially and returns the same rows -- so without it a broken dispatch reads as a dead heat.
func BenchmarkParallelWireQuery(b *testing.B) {
	// The machine-wide budget is GOMAXPROCS tokens and a fan-out needs two of them, so on a
	// single-core box there is nothing to measure and the arms would be the same path twice.
	if runtime.GOMAXPROCS(0) < 2 {
		b.Skip("GOMAXPROCS < 2: the worker budget cannot seat a fan-out")
	}
	const n = 40000
	c := parWireBenchStore(b, n, 8)
	proj := []string{"ClusterId", "ProcId", "JobStatus", "Owner", "RequestMemory"}
	cases := []struct {
		name string
		expr string
		want int
	}{
		// Nearly every record is decided from the columns and never reassembled: what is left to
		// parallelize is the column reads themselves.
		{"selective", "ClusterId < 500", 500},
		// Most records match, so the workers also assemble most of the rows -- the shape where
		// fan-out has the most to take.
		{"unselective", "ProcId < 8", n * 8 / 10},
	}
	for _, tc := range cases {
		q, qerr := vm.Parse(tc.expr)
		if qerr != nil {
			b.Fatal(qerr)
		}
		// Warm pass: the store is mmap'd, and the first read of it pays the page-in for every
		// segment plus the block cache's first fill. Pinned to one path so each case warms the
		// same way rather than inheriting whatever the previous case's last arm left set.
		c.queryPar = 8
		warm := 0
		for range c.QueryRawWire(q, proj, false) {
			warm++
		}
		if warm != tc.want {
			b.Fatalf("%s: warm pass returned %d rows, want %d", tc.name, warm, tc.want)
		}
		for round := 0; round < 2; round++ {
			arms := []struct {
				name string
				par  int
			}{{"serial", 1}, {"fanout", 8}}
			if round%2 == 1 {
				arms[0], arms[1] = arms[1], arms[0]
			}
			for _, arm := range arms {
				b.Run(fmt.Sprintf("%s/r%d/%s", tc.name, round, arm.name), func(b *testing.B) {
					c.queryPar = arm.par
					b.ReportAllocs()
					before := wireQueryFanouts.Load()
					b.ResetTimer()
					for b.Loop() {
						got := 0
						for range c.QueryRawWire(q, proj, false) {
							got++
						}
						if got != tc.want {
							b.Fatalf("%d rows, want %d", got, tc.want)
						}
					}
					b.StopTimer()
					fanned := wireQueryFanouts.Load() - before
					switch {
					case arm.par == 1 && fanned != 0:
						b.Fatalf("the serial arm fanned out %d times", fanned)
					case arm.par > 1 && fanned == 0:
						// Not a skip: the budget is free (nothing else is reading this store)
						// and the store is far over the work-size gate, so a read that did not
						// fan out means the dispatch is gone -- and skipping would hide exactly
						// that, since the fallback returns the same rows.
						b.Fatal("the parallel arm never fanned out; it measured the serial path twice")
					}
				})
			}
		}
	}
}
