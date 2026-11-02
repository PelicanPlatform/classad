package collections

import (
	"sync/atomic"
)

// Fanning the columnar value pass across segments.
//
// The pass is the same unit of work a parallel Query fans out over -- one segment window, immutable
// bytes, lock-free reads -- and it is cheaper per record than a Query by more than an order of
// magnitude, so the fixed cost of a fan-out buys proportionally more. What it does NOT share with a
// Query is the shape of the result: a Query yields rows that concatenate, where an aggregate yields
// ONE answer that has to be folded back together. That fold is what the caller supplies here.
//
// Measured on this pass, per record: a columnar block costs ~12 ns and the row fallback a segment
// without a usable block falls to costs ~210 ns. Both are fanned, which matters more than it
// sounds: the expensive case is the one with the most work in it, and a work-stealing loop gives it
// to whichever worker is free rather than to whichever worker drew the long straw.

// defaultAggMinBytes is the minimum live segment bytes an aggregate pass must cover before fan-out
// is worth its fixed cost.
//
// It is far HIGHER than a Query's 1 MiB, which is the opposite of what the obvious argument
// predicts, and the argument is worth writing down because it is wrong in a way that reads as
// obvious: the aggregate pass is cheaper per record than a Query, so the same bytes are a shorter
// pass, so the gate should be lower. What a fan-out has to amortize is a fixed cost against TOTAL
// WORK, and a cheaper pass per record means a given byte count is LESS work to win back, not more.
// Cheaper per record argues the threshold UP.
//
// Measured, serial against fanned out, over the same data (BenchmarkAggParallel), the built-in and
// user-defined arms agreeing at every size:
//
//	 2 MB  0.62x   fan-out loses
//	 4 MB  0.73x   loses
//	 8 MB  0.77x   loses
//	16 MB  1.05x   break-even
//	32 MB  2.80x   wins
//	64 MB  2.35x   wins
//
// The gate is set where the win is DECISIVE rather than at the crossover, for two reasons. A
// fan-out that merely breaks even is not worth taking worker tokens from concurrent queries, which
// draw from the same budget. And a crossover is a quantity a noisy machine invents: it has to be
// re-measured per machine, where a 2x margin survives one. These numbers were taken on a loaded
// box, which biases against the fan-out, so the threshold is conservative by construction.
const defaultAggMinBytes = 1 << 25

// aggFanouts counts the aggregate passes that actually fanned out.
//
// Declining to fan out is SILENT by design -- valuesPar runs the same tasks on the calling goroutine
// and produces the same answer -- so without this counter a change that disabled the dispatch would
// leave every parallel-vs-serial test passing, having compared the serial path with itself. The
// parallel Query has the same counter for the same reason.
var aggFanouts atomic.Int64

// AggFanouts reports how many aggregate passes have fanned out across segments. Test-only
// observability; see aggFanouts.
func AggFanouts() int64 { return aggFanouts.Load() }

// Segments the aggregate pass answered from a columnar block, and segments it had to read record by
// record instead.
//
// WHY THESE EXIST. The row fallback costs 17-26x what a columnar block costs per record, and the
// pass reports success either way -- so a request served ENTIRELY from the row fallback is an order
// of magnitude off its advertised cost and nothing in the answer says so. That is not hypothetical:
// a fixture built with segments large enough that none of them had sealed sent every segment down
// the row fallback, and the first scaling numbers taken off it were nonsense in a way that read as
// a surprising result rather than as a broken fixture. A benchmark or test that means to measure the
// columnar path can now assert it ran, instead of inferring it from a timing that is exactly what is
// in question.
var (
	aggColSegments atomic.Int64
	aggRowSegments atomic.Int64
)

// AggSegmentPaths reports how many segments the aggregate value pass has answered from a columnar
// block and how many it has read record by record. See the note above the counters.
func AggSegmentPaths() (columnar, row int64) {
	return aggColSegments.Load(), aggRowSegments.Load()
}

// valuesPar runs the columnar value pass over every segment, fanning it across workers when the
// pass is large enough and the collection's worker budget has capacity.
//
// THE CONTRACT WITH THE CALLER, which is what makes the fold safe:
//
//   - newSink is called once per worker, on THIS goroutine, before any worker starts. The caller
//     therefore builds its accumulators in a known order and keeps them; it never has to publish one
//     across a goroutine boundary itself.
//   - Each sink is fed by exactly ONE goroutine, so an accumulator needs no synchronization.
//   - merge is called once, on THIS goroutine, after every worker has returned -- and only when the
//     pass succeeded. The caller folds its accumulators there.
//
// It calls newSink exactly once and merge exactly once in the serial case too, so a caller has one
// path to get right rather than two. Folding one accumulator into itself must therefore be a no-op,
// which it is for the natural shape: fold accs[1:] into accs[0].
//
// VALUES ARRIVE IN A DIFFERENT ORDER THAN THEY DO SERIALLY, and not a fixed one -- which worker
// takes which segment is a race. Every aggregate fed from this pass has already promised to be
// order-independent, so for them this changes nothing. It does change float summation: the SUM of a
// real column is now associativity-dependent and may differ in its last bits between two runs of the
// same query. That is the accepted cost of fan-out, not an oversight -- an exact float sum needs a
// fixed order, which is the thing being given up. Integer sums are unaffected: they are exact in
// int64 and their order cannot matter.
//
// ok=false means the pass could not serve the request and the caller must scan; the sinks are then
// garbage and merge is not called.
func (c *Collection) valuesPar(aggID uint32, preds []fieldPred, bc *blockCache,
	newSink func() numSink, merge func()) bool {
	tasks, totalBytes, release := c.gatherTasks()
	defer release()

	W := 0
	if c.qsem != nil && len(tasks) >= 2 && totalBytes >= c.aggMinBytes {
		want := c.queryPar
		if want > len(tasks) {
			want = len(tasks)
		}
		W = tryAcquire(c.qsem, want)
	}
	if W < 2 {
		for i := 0; i < W; i++ {
			<-c.qsem // one token is not enough to parallelize; give it back
		}
		p := c.planValuePass(aggID, preds)
		var st valuePassState
		sink := newSink()
		for _, t := range tasks {
			if !c.valuePassWindow(&p, &st, t.win, t.s0, bc, sink) {
				return false
			}
		}
		merge()
		return true
	}
	defer func() {
		for i := 0; i < W; i++ {
			<-c.qsem
		}
	}()
	aggFanouts.Add(1)

	p := c.planValuePass(aggID, preds)
	sinks := make([]numSink, W)
	for i := range sinks {
		sinks[i] = newSink()
	}
	// declined stops the other workers as soon as one finds a window this pass cannot serve. It is
	// a hint, not the verdict: a worker that has already started a window finishes it, and the
	// verdict is read once below, after everyone has returned.
	var declined atomic.Bool
	var next int64
	wg := workerGroup{stop: &declined}
	for i := 0; i < W; i++ {
		wg.Go(func() {
			wi := i
			// This worker's own narrowing scratch, carried across the windows it steals so it is
			// sized once per pass rather than once per segment.
			var st valuePassState
			for {
				if declined.Load() {
					return
				}
				idx := int(atomic.AddInt64(&next, 1)) - 1
				if idx >= len(tasks) {
					return
				}
				if !c.valuePassWindow(&p, &st, tasks[idx].win, tasks[idx].s0, bc, sinks[wi]) {
					declined.Store(true)
					return
				}
			}
		})
	}
	wg.Wait()
	// Only after Wait: a worker's panic is re-raised here, on the caller's goroutine, once nothing
	// is still reading what release() is about to unpin.
	wg.rethrow()
	if declined.Load() {
		return false
	}
	merge()
	return true
}

// merge folds another pass's accumulator into this one. Every field is either additive, an extreme,
// or a flag that is set if either side set it -- so the fold is associative and commutative, which
// is what lets the workers finish in any order.
//
// It operates on statsAccum rather than on NumStats because anyInt guards IntMin/IntMax and does not
// survive result(), which also rewrites Min/Max for an empty pass. Merging results instead of
// accumulators would read those rewritten zeros as data.
func (a *statsAccum) merge(b *statsAccum) {
	a.out.N += b.out.N
	a.out.Sum += b.out.Sum
	a.out.IntSum += b.out.IntSum
	a.out.AnyReal = a.out.AnyReal || b.out.AnyReal
	a.out.AnyBool = a.out.AnyBool || b.out.AnyBool
	a.out.AnyNaN = a.out.AnyNaN || b.out.AnyNaN
	// Min/Max are seeded to +Inf/-Inf, so an empty side loses both comparisons and contributes
	// nothing -- no emptiness test needed. NaN loses them too, exactly as it does in add.
	if b.out.Min < a.out.Min {
		a.out.Min = b.out.Min
	}
	if b.out.Max > a.out.Max {
		a.out.Max = b.out.Max
	}
	// IntMin/IntMax hold nothing until anyInt, so they need the test Min/Max do not.
	if b.anyInt {
		if !a.anyInt || b.out.IntMin < a.out.IntMin {
			a.out.IntMin = b.out.IntMin
		}
		if !a.anyInt || b.out.IntMax > a.out.IntMax {
			a.out.IntMax = b.out.IntMax
		}
		a.anyInt = true
	}
}
