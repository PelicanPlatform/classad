package collections

import (
	"fmt"
	"os"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// sealActiveSegments closes every shard's active segment, so everything written becomes sealed and
// can be columnarized.
//
// WITHOUT THIS A SCAN BENCHMARK DOES NOT MEASURE THE COLUMNAR PATH. The active segment has no
// columnar block, so its records go to rowEvalWindow and through the whole ClassAd evaluator -- a
// path measured at roughly 88x the per-record cost of the vector one. On this fixture that tail is
// ~600 records out of 20000, 3% of the rows and about three quarters of the CPU, which is enough to
// hide any change to the vector executor entirely and to produce differences that look like
// executor effects and are not. Two measurements were drawn and retracted before this was found.
//
// Production amortizes the tail over far more sealed data; a benchmark with twenty segments does
// not, so it has to be made explicit here.
func sealActiveSegments(c *Collection) {
	for _, sh := range c.shards {
		sh.mu.Lock()
		sh.act = nil
		sh.mu.Unlock()
	}
}

// poolSampleFixture loads the committed real OSPool `condor_status -l` sample into a store with
// sealed segments, which is the ad shape (~8.6 KB each) the production block width is set by.
func poolSampleFixture(tb testing.TB) *Collection {
	tb.Helper()
	sample := loadCorpus(tb)
	// 8 MiB segments over ~8.6 KB ads: ~950 records per segment, so a whole-segment block holds
	// several times the byte budget and the row-count policies are genuinely far apart.
	c := New(Options{Shards: 1, SegmentSize: 1 << 23})
	batch := make([]AdUpdate, 0, 512)
	for i := 0; i < 20000; i++ {
		batch = append(batch, AdUpdate{Key: []byte(fmt.Sprintf("s%d", i)), Ad: sample[i%len(sample)]})
		if len(batch) == 512 {
			if err := c.Update(batch); err != nil {
				tb.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	if err := c.Update(batch); err != nil {
		tb.Fatal(err)
	}
	sealActiveSegments(c)
	if !c.BuildAndEnableSchemaScan(2000, 8) {
		tb.Skip("no schema derived from the pool sample")
	}
	if info := c.SchemaScanInfo(); info.CoveredSegments == 0 {
		tb.Skipf("accelerator covers no sealed segment (%d sealed)", info.SealedSegments)
	}
	assertNoRowWindows(tb, c)
	return c
}

// assertNoRowWindows fails if any window of this fixture still lacks a columnar block.
//
// It is the precondition every scan measurement here rests on, and it is asserted rather than
// assumed because the failure is silent: a fixture with a row window still answers correctly, still
// reports vecBlocks greater than zero, and still prints a plausible ns/op -- while spending most of
// that time in a different executor. Nothing about the output looks wrong.
func assertNoRowWindows(tb testing.TB, c *Collection) {
	tb.Helper()
	q, err := vm.Parse("true")
	if err != nil {
		tb.Fatal(err)
	}
	c.VectorEvalCount(q)
	sp := lastVecSplit.Load()
	if sp == nil {
		tb.Fatal("no scan split recorded; the fixture's path cannot be checked")
	}
	if sp.rowWindows != 0 {
		tb.Fatalf("%d window(s) have no columnar block, so this fixture measures the row evaluator "+
			"as much as the vector path", sp.rowWindows)
	}
}

// widthPolicies sweeps the row group across the whole range the vector executor can be handed, from
// the production width up to the row backstop.
//
// CLASSAD_BENCH_WIDTH_REVERSE reverses the order, and a conclusion from one direction alone is not
// one. The regimes run sequentially against one store, so the arm measured FIRST absorbs the
// warm-up; that has produced a wrong conclusion in this package before (see colgroup_bench_test.go),
// and the production default is not placed first here for that reason.
func widthPolicies() []groupPolicy {
	rows := []int{16, 64, 256, 1024, 4096, 8192}
	if os.Getenv("CLASSAD_BENCH_WIDTH_REVERSE") != "" {
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}
	ps := make([]groupPolicy, 0, len(rows)+1)
	for i, n := range rows {
		if i == len(rows)/2 {
			ps = append(ps, groupPolicy{"default", defaultColGrouping()})
		}
		ps = append(ps, groupPolicy{fmt.Sprintf("rows%04d", n), byRows(n)})
	}
	return ps
}

// BenchmarkVecScanBlockWidth measures the real vector scan over real OSPool ads as the block width
// changes, which is the width the vector executor's batch IS. The cache is warmed first, so the
// decompression the row group also governs is paid up front and what varies between arms is mostly
// the width the executor runs its kernels at.
//
// This is the production-scale counterpart to BenchmarkTileHot in the vm package, which isolates the
// register file. It answers the question the other way round: at the sixteen-record width the byte
// budget actually produces, is anything being LOST by running the executor in such small batches?
// Two expressions, a minimal constraint and a seven-term arithmetic chain, which vary the WORK per
// record by about an order of magnitude. They do not vary register pressure, and that is worth
// stating rather than quietly assuming otherwise: the chain holds THREE live vectors, the same as a
// two-term conjunction, because the lowering is left-associative and each product is consumed by
// the sum above it. Expression size does not buy register pressure here at all. Only script
// bindings do, which is why the gradient lives in BenchmarkTileHot and not here.
//
// It is the noisier of the two by a long way -- decompression, block cache behaviour and ordering
// all move with the row group, and only the last of them is controlled here -- so read it for the
// order of magnitude and take the isolated sweep for the shape.
func BenchmarkVecScanBlockWidth(b *testing.B) {
	store := poolSampleFixture(b)
	for _, e := range []struct{ name, expr string }{
		// Hot columns only: the vector path declines a column that lives in the cold tail, so a
		// constraint over Memory or Cpus on this corpus never reaches the executor at all.
		{"short", "DetectedCpus > 1"},
		{"chain", "DetectedCpus * 2 + UpdatesLost * 3 + OpSysMajorVer * 4 + MonitorSelfResidentSetSize * 5 + " +
			"DetectedCpus * 6 + UpdatesLost * 7 + OpSysMajorVer * 8 > 10"},
	} {
		q, err := vm.Parse(e.expr)
		if err != nil {
			b.Fatal(err)
		}
		for _, p := range widthPolicies() {
			blocks, recs := regroup(b, store, p.g)
			perBlock := float64(recs) / float64(blocks)
			coldCache(b, store)
			store.VectorEvalCount(q) // two warm passes, per widthPolicies' note on ordering
			store.VectorEvalCount(q)
			b.Run(e.name+"/"+p.name, func(b *testing.B) {
				// Assert WHICH PATH RAN before reporting anything. A constraint over a column in
				// the cold tail declines the vector path on every block and falls back to colScope,
				// which answers correctly and ~40x slower -- so without this the benchmark reports a
				// number for a path it never took. It fails rather than warns because a warning in
				// benchmark output is invisible beside the numbers.
				store.VectorEvalCount(q)
				if sp := lastVecSplit.Load(); sp == nil || sp.vecBlocks == 0 {
					b.Fatalf("no block took the vector path: %+v", sp)
				}
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, ok := store.VectorEvalCount(q); !ok {
						b.Fatal("vector path declined mid-benchmark")
					}
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(recs), "ns/elem")
				b.ReportMetric(perBlock, "rec/blk")
			})
		}
	}
}
