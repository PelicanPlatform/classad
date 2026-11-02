package vm

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/parser"
)

// WHAT THIS FILE MEASURES, AND THE ARGUMENT IT REFUTES.
//
// THE ARGUMENT WAS: peak live bytes in the vector executor is roughly
// `live registers x batch width x bytes per element`. Split the batch into cache-sized tiles and
// fuse each straight-line region over a tile, and peak live bytes falls by eight to sixteen times --
// at which point MORE live registers become affordable rather than fewer. Tiling and register
// allocation would then be substitutes for one problem, and tiling the cheaper of the two.
//
// IT DOES NOT HOLD, for two independent reasons, and the second is the one that generalizes.
//
// FIRST, the width it was costed at is not the width the executor runs at. The batch is a columnar
// block, and a block seals at a BYTE budget; on real pool ads that is about sixteen records, so a
// data register is a few hundred bytes. TestProductionBlockWidthIsSetByBytesNotRows in the
// collections package pins that. A tile cannot be smaller than a batch that is already sixteen wide,
// and in any case must not straddle a block, because the Raw and Dict forms alias one block's bytes.
//
// SECOND -- and this is what the benchmarks below are for -- narrowing the batch does not help even
// where the register file IS large. BenchmarkTileHot sweeps the width from 16 to 8192 over a corpus
// held in cache, so the only thing growing with the width is the register file: at 32 live bindings
// and width 8192 that file is about 6.9 MiB allocated and 2.4 MiB actually touched. Throughput per
// element does not improve at ANY narrower width, at ANY register pressure. It gets WORSE below
// about 512 -- and the relative penalty at width 16 is LARGEST where the working set is SMALLEST,
// falling monotonically from 3.02x in BenchmarkTileControl, whose two vectors are 404 KiB, through
// 1.53x, 1.45x, 1.40x, 1.35x and 1.31x to 1.28x at thirty-four vectors and 6.9 MiB. That inverts
// the predicted signature, which was no effect at low pressure growing as pressure rises. It is
// per-call fixed cost amortized over more per-element work, and a working-set effect cannot be
// strongest in the arm that has none.
//
// The reason is that nothing here is waiting on memory. A register is written by one instruction and
// streamed by the next, which is the access pattern a prefetcher serves perfectly, and the
// interpretive cost per element per value operation is fifteen to nineteen cycles -- more than an L2
// hit, and paid whether or not the data was in L1. BenchmarkTileControl's single specialized
// comparison runs at about six, so the gap is dispatch and generality, not memory. Shrinking the
// working set of something that is not stalling on memory buys nothing. That is the general form,
// and it applies to any later pass justified by peak live bytes rather than by a measured stall.
//
// WHAT THIS DOES NOT TEST, and must not be read as refuting: kernel fusion. Narrowing the batch and
// fusing instructions over a tile are two mechanisms, and only the first is what a width sweep
// varies. Fusion would keep an intermediate out of memory entirely; the sweep over an unfused
// executor says nothing about it. What the sweep does say is that fusion's payoff cannot be the
// memory traffic it saves, since there is no memory stall here to recover -- it would have to be
// the per-element interpretive work removed, which is a different and smaller claim, and one that
// needs a fused kernel to measure rather than a sweep.
//
// Kept as benchmarks rather than deleted because the argument is a natural one to make again, and
// because the sweep is the cheap way to re-answer it on different hardware or after the kernels get
// faster. If the per-element interpretive cost ever falls far enough for memory to become the limit,
// this sweep is what will show it: the curve would develop a minimum at a width below 8192, and that
// minimum would move with register pressure.

// tileSource serves pre-built contiguous int64 columns, the shape a real block hands over, and
// honours dst.n so a caller can evaluate a window of the corpus at any width.
//
// Columns are keyed under both the given spelling and the folded one, so a lookup is an exact hit.
// ClassAd attribute names are case-insensitive and folding at lookup time allocates for any name not
// already lower-case -- once per LoadColumn call, which at a sixteen-record width is once per
// sixteen elements and would swamp the measurement this fixture exists to take.
type tileSource struct {
	cols map[string][]int64
	off  int
}

func newTileSource(total int, names []string) *tileSource {
	s := &tileSource{cols: make(map[string][]int64, 2*len(names))}
	for k, nm := range names {
		c := make([]int64, total)
		for i := range c {
			c[i] = int64(1 + (i*(k+1))%64)
		}
		s.cols[nm] = c
		s.cols[strings.ToLower(nm)] = c
	}
	return s
}

func (s *tileSource) LoadColumn(name string, scope ast.AttributeScope, dst *Vec) bool {
	if scope != ast.NoScope && scope != ast.MyScope {
		return false
	}
	dst.Dict, dst.Raw, dst.Mask, dst.Const = nil, nil, false, false
	c, ok := s.cols[name]
	if !ok {
		return false
	}
	copy(dst.I[:dst.n], c[s.off:s.off+dst.n])
	st := dst.St[:dst.n]
	for i := range st {
		st[i] = VsInt
	}
	return true
}

// pressureScript builds a script holding k+1 registers live at once: k bindings, every one of them
// read by the result, so none can die before the last instruction. That is the shape the refuted
// argument needs -- a script with two live registers cannot show a working-set effect at any width,
// so a sweep that did not vary this would prove nothing either way.
func pressureScript(tb testing.TB, k int) *ScriptProgram {
	tb.Helper()
	s := NewScript()
	terms := make([]string, 0, k)
	for i := 0; i < k; i++ {
		e, err := parser.ParseExpr(fmt.Sprintf("A%d * %d + %d", i%4, i+1, i))
		if err != nil {
			tb.Fatal(err)
		}
		s = s.Let(fmt.Sprintf("v%d", i), e)
		terms = append(terms, fmt.Sprintf("v%d", i))
	}
	re, err := parser.ParseExpr(strings.Join(terms, " + ") + " > 100")
	if err != nil {
		tb.Fatal(err)
	}
	sp, err := s.Result(re).Compile()
	if err != nil {
		tb.Fatal(err)
	}
	return sp
}

// vectorsLive is how many batch-width vectors an execution actually held: the stack it grew plus one
// place per local. This is the quantity the cost model calls "live registers", and it is read off
// the executor rather than off the register program, because the vector path runs the stack form.
func vectorsLive(s *VecScratch) int { return len(s.stack) + len(s.locals) }

var tileWidths = widthOrder()

// widthOrder flips the sweep direction when ZZ_TILE_REVERSE is set. Run it each way and compare: an
// effect that survives both orders is an effect, and one that does not is the harness warming up.
func widthOrder() []int {
	w := []int{16, 32, 64, 128, 256, 512, 1024, 2048, 4096, 8192}
	if os.Getenv("ZZ_TILE_REVERSE") != "" {
		for i, j := 0, len(w)-1; i < j; i, j = i+1, j-1 {
			w[i], w[j] = w[j], w[i]
		}
	}
	return w
}

// sweep runs one program over total elements in batches of w, at every width, reporting ns/elem --
// which is the comparable unit, since the number of calls varies with the width and the number of
// elements does not.
func sweep(b *testing.B, prefix string, total int, src *tileSource,
	run func(src ColumnSource, w int, s *VecScratch) bool) {
	for _, w := range tileWidths {
		b.Run(fmt.Sprintf("%s/w%05d", prefix, w), func(b *testing.B) {
			var scratch VecScratch
			src.off = 0
			if !run(src, w, &scratch) {
				b.Fatal("declined")
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for off := 0; off+w <= total; off += w {
					src.off = off
					run(src, w, &scratch)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(total), "ns/elem")
		})
	}
}

var pressureLevels = []int{1, 2, 4, 8, 16, 32}

// BenchmarkTileHot sweeps the batch width over a corpus small enough to stay cache-resident, so the
// only thing that grows with the width is the REGISTER FILE. This is tiling's best case: if peak
// live bytes limited throughput, ns/elem would fall as a narrower width brought the register file
// into cache, and it would fall further the more registers were live.
func BenchmarkTileHot(b *testing.B) {
	const total = 8192 // one maximum-width batch; four columns is 256 KiB, cache-resident
	src := newTileSource(total, []string{"A0", "A1", "A2", "A3"})
	for _, k := range pressureLevels {
		sp := pressureScript(b, k)
		sweep(b, fmt.Sprintf("k%02d", k), total, src,
			func(c ColumnSource, w int, s *VecScratch) bool { _, ok := sp.VecEval(c, w, s); return ok })
	}
}

// BenchmarkTileStream is the same sweep over a corpus far larger than any cache, which is what a
// real scan does: the source read is cold at every width, so whatever the register file contributes
// is diluted by traffic no width can change. Between the two, BenchmarkTileHot is the one that could
// have shown an effect and BenchmarkTileStream is the one that resembles production.
func BenchmarkTileStream(b *testing.B) {
	const total = 1 << 20
	src := newTileSource(total, []string{"A0", "A1", "A2", "A3"})
	for _, k := range pressureLevels {
		sp := pressureScript(b, k)
		sweep(b, fmt.Sprintf("k%02d", k), total, src,
			func(c ColumnSource, w int, s *VecScratch) bool { _, ok := sp.VecEval(c, w, s); return ok })
	}
}

// BenchmarkTileControl is the control, and belongs in the same invocation as the sweeps. It runs an
// expression whose working set does not vary with register pressure, so whatever its curve shows at
// a given width is per-call fixed cost and machine drift. The sweeps' curves have the same shape as
// this one, which is the whole finding: what the width changes is fixed cost, not residency.
func BenchmarkTileControl(b *testing.B) {
	const total = 8192
	src := newTileSource(total, []string{"A0"})
	q, err := Parse("A0 > 32")
	if err != nil {
		b.Fatal(err)
	}
	sweep(b, "ctl", total, src,
		func(c ColumnSource, w int, s *VecScratch) bool { _, ok := q.VecEval(c, w, s); return ok })
}

// TestVectorPeakLiveIsExpressionShaped pins the other half of the peak-live-bytes figure: how many
// batch-width vectors the executor actually holds.
//
// The cost model contemplates five to ten live data registers for an expression and more for a
// script. A real pool constraint holds TWO to FOUR, and it holds them because that is the expression's
// shape -- the stack is two deep however many operands the conjunction has, since the lowering is
// left-associative and each comparison is consumed as it is produced. Combined with a sixteen-record
// production block, peak live bytes for a real constraint is about a kilobyte.
//
// This is also the invariant that the executor reuses its vectors rather than growing one per
// instruction: a stack that failed to pop, or a local load that allocated instead of copying into a
// pooled slot, shows up here as a count that tracks instruction count instead of expression shape.
//
// Checked by mutation: leaking one pooled slot per push -- correctness-preserving, since slots past
// the depth are never indexed -- fails this test and TestVecScratchReuse, and nothing else in the
// package. That no VALUE test moved is the point: the quantity here is invisible to every oracle
// that compares results, which is why it needs a test of its own.
func TestVectorPeakLiveIsExpressionShaped(t *testing.T) {
	const w = 8192
	src := newTileSource(w, []string{"A0", "A1", "A2", "A3"})
	for _, tc := range []struct {
		want int
		expr string
	}{
		{2, "A0 > 1"},
		{3, "A0 >= 1 && A1 > 1000"},
		// Four conjuncts rather than two, and the count does not move: the stack is two deep
		// whatever the arity, because each comparison is consumed by the AND above it.
		{3, "(A0 >= 1) && (A1 >= 2) && (A2 >= 3) && (A3 >= 4)"},
		// A seven-term arithmetic chain, fourteen value operations, and still THREE. This is the
		// case worth keeping: it is not that real constraints happen to be small, it is that
		// EXPRESSION SIZE DOES NOT BUY REGISTER PRESSURE at all. The lowering is left-associative,
		// so each product is consumed by the sum above it and the accumulator is the only thing
		// that lives. Only script bindings read after their definition raise the count, which is
		// what pressureScript below has to construct deliberately.
		{3, "A0 * 2 + A1 * 3 + A2 * 4 + A3 * 5 + A0 * 6 + A1 * 7 + A2 * 8 > 10"},
		{4, "A0 * A1 + A2 * A3 > 100 && A0 + A1 > A2 - A3"},
	} {
		q, err := Parse(tc.expr)
		if err != nil {
			t.Fatal(err)
		}
		var scratch VecScratch
		src.off = 0
		if _, ok := q.VecEval(src, w, &scratch); !ok {
			t.Fatalf("%q declined; this test is not measuring the vector path", tc.expr)
		}
		if got := vectorsLive(&scratch); got != tc.want {
			t.Errorf("%q held %d vectors, want %d", tc.expr, got, tc.want)
		}
	}

	// A script is where the count can genuinely grow, and it grows with BINDINGS READ AT THE END
	// rather than with program length: k bindings all read by the result is k places plus a two-deep
	// stack. That is the gradient the width sweep varies, and it is here so the sweep's claim to have
	// varied register pressure is checked rather than asserted.
	for _, k := range []int{1, 8, 32} {
		sp := pressureScript(t, k)
		var scratch VecScratch
		src.off = 0
		if _, ok := sp.VecEval(src, w, &scratch); !ok {
			t.Fatalf("k=%d declined", k)
		}
		if got, want := vectorsLive(&scratch), k+2; got != want {
			t.Errorf("k=%d held %d vectors, want %d", k, got, want)
		}
	}
}
