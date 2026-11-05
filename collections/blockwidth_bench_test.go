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
//
// rowGroupBytes is Options.RowGroupBytes, 0 taking the production default. It is a parameter rather
// than a later adjustment because it is the only way to change the width of these blocks: a sealed
// segment here carries its own columnar payload, whose row groups were decided when the segment was
// columnarized, so a width sweep builds a store per budget instead of regrouping one.
func poolSampleFixture(tb testing.TB, rowGroupBytes int) *Collection {
	tb.Helper()
	sample := loadCorpus(tb)
	// 8 MiB segments over ~8.6 KB ads: ~950 records per segment, so a whole-segment block holds
	// several times the byte budget and the budgets below are genuinely far apart.
	c := New(Options{Shards: 1, SegmentSize: 1 << 23, RowGroupBytes: rowGroupBytes})
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

func BenchmarkVecScanBlockWidth(b *testing.B) {
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
		want := -1
		for _, bud := range widthBudgets() {
			store := poolSampleFixture(b, bud.bytes)
			blocks, recs := blockStats(b, store)
			coldCache(b, store)
			store.VectorEvalCount(q) // two warm passes, per widthBudgets' note on ordering
			store.VectorEvalCount(q)
			b.Run(e.name+"/"+bud.name, func(b *testing.B) {
				// Assert WHAT WAS COUNTED, not just which path ran. A fixture that answers nothing
				// still reports vecBlocks greater than zero and still prints a plausible ns/op, so
				// checking the path alone cannot tell a scan of real columns from a scan of empty
				// ones -- and the count must not depend on the row group at all.
				got, ok := store.VectorEvalCount(q)
				if !ok {
					b.Fatal("vector path declined")
				}
				if want < 0 {
					want = got
				}
				if got != want {
					b.Fatalf("counted %d at %s but %d at the first budget: the row group changed the "+
						"ANSWER, so these timings are not of the same work", got, bud.name, want)
				}
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
				b.ReportMetric(float64(recs)/float64(blocks), "rec/blk")
			})
		}
	}
}

type widthBudget struct {
	name  string
	bytes int
}

// widthBudgets sweeps Options.RowGroupBytes around the production default.
//
// CLASSAD_BENCH_WIDTH_REVERSE reverses the order, and a conclusion from one direction alone is not
// one: each arm builds its own store, so the arm measured FIRST absorbs the warm-up.
func widthBudgets() []widthBudget {
	out := []widthBudget{
		{"b032k", 32 << 10},
		{"b128k-default", 128 << 10},
		{"b512k", 512 << 10},
		{"b002m", 2 << 20},
		{"b008m", 8 << 20},
	}
	if os.Getenv("CLASSAD_BENCH_WIDTH_REVERSE") != "" {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

// blockStats reports the columnar blocks and records the store's sealed segments actually hold, so
// an arm that did not move the width cannot be mistaken for one that did.
func blockStats(tb testing.TB, c *Collection) (blocks, recs int) {
	tb.Helper()
	for _, sh := range c.shards {
		sh.mu.RLock()
		act := sh.act
		segs := append([]*segment(nil), sh.segs...)
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil || seg == act || seg.used == 0 {
				continue
			}
			cs := seg.colblk.Load()
			if cs == nil {
				continue
			}
			blocks += len(cs.blocks)
			for _, bl := range cs.blocks {
				recs += bl.n
			}
		}
	}
	if blocks == 0 {
		tb.Fatal("no sealed segment carries a columnar block: there is no columnar path to measure")
	}
	return blocks, recs
}
