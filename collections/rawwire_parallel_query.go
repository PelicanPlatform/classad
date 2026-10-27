package collections

import (
	"sync"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// wireQueryFanouts counts the constrained wire reads that actually fanned out.
//
// The counter is the only way a test can tell a fan-out from the serial fallback, because declining
// to fan out is SILENT by design -- the caller re-runs the same read serially and returns the same
// rows. Without it, a change that disabled the dispatch would leave every parallel-vs-serial
// comparison passing, having compared the serial path with itself.
var wireQueryFanouts atomic.Int64

// runParallelWireQuery is runParallelWireScan for a CONSTRAINED wire-row read: the same segment
// fan-out, worker budget, arena recycling and row assembly, but each worker evaluates the query
// itself instead of emitting every record.
//
// Until this existed the constrained read was always serial while the unconstrained dump fanned
// out, which is backwards -- measured against a schedd's own queue walk over a 100k-job mirror,
// the unconstrained dump ran at 0.83x and a 2k-row constrained query at 0.79x falling to 0.68x
// under concurrency, so the constrained path was where the headroom was.
//
// WHY IT REUSES refVisitor RATHER THAN THE SCAN LOOP runParallelWireScan WRITES OUT. The naive
// version -- walk the records, decompress, match, assemble -- would have dropped the columnar
// prefilter, which only the refVisitor path has. On the same mirror, `JobUniverse==5 &&
// JobPrio>100` decided all 100000 records from the columns and reassembled NONE of them, in
// 30.7 ms; without the prefilter every one of those records is rebuilt from the arena, which is
// far more work than eight workers can win back. So a parallel scan that lost the prefilter
// would have been SLOWER than the serial one it replaced on exactly the queries that matter.
//
// The prefilter is not safe to share (it caches per-block state in a colScope), so each worker
// builds its OWN through scanVisitState -- the same per-goroutine-state rule that already
// governs the decompression buffer, the subset scratch and the narrower here.
//
// Returns false without consuming the yield when parallelism is not engaged -- too little work,
// no budget -- and the caller runs the serial path, exactly as ScanRawWire does.
func (c *Collection) runParallelWireQuery(qp queryPlan, sel *wireSubsetSelector, needed int, yield func([]byte) bool) bool {
	// Gather with the segment counts held ASIDE. When the gate below declines to fan out, the
	// caller re-runs the whole read serially and prunes (and counts) the same segments again, so
	// recording them before the decision would double them in an EXPLAIN ANALYZE.
	var gathered ScanStats
	gq := qp
	gq.stats = &gathered
	tasks, totalBytes, release := c.gatherTasksPruned(gq)
	W := 0
	if c.qsem != nil && len(tasks) >= 2 && totalBytes >= c.parallelMinBytes {
		want := c.queryPar
		if want > len(tasks) {
			want = len(tasks)
		}
		W = tryAcquire(c.qsem, want)
	}
	if W < 2 {
		for i := 0; i < W; i++ {
			<-c.qsem // not enough to parallelize; hand the tokens back
		}
		release()
		return false
	}
	defer release()
	defer func() {
		for i := 0; i < W; i++ {
			<-c.qsem
		}
	}()
	wireQueryFanouts.Add(1)
	qp.stats.segments(gathered.SegmentsTotal, gathered.SegmentsPruned)

	// The key that opens a sealed value during assembly -- nil unless the store encrypts at rest,
	// read once rather than per record, as the serial emitter does per call.
	seal := c.renderKey(sel.redact)

	// Per-worker counters, merged once below: ScanStats is a plain counter set with no
	// synchronization, so workerPlan drops the shared sink and each worker counts into its own.
	// Only when the caller actually asked for stats -- otherwise the workers keep workerPlan's nil
	// sink and the per-record counting stays the nil check it is on the serial path.
	var workerStats []ScanStats
	if qp.stats != nil {
		workerStats = make([]ScanStats, W)
	}
	results := make(chan wireRowBatch, W)
	recycle := make(chan wireRowBatch, 2*W+2)
	getBatch := func() wireRowBatch {
		select {
		case b := <-recycle:
			b.buf = b.buf[:0]
			b.offs = append(b.offs[:0], 0)
			return b
		default:
			return wireRowBatch{buf: make([]byte, 0, wireBatchArena), offs: []int{0}}
		}
	}
	var next int64
	var stopped atomic.Bool
	var wg sync.WaitGroup
	for i := 0; i < W; i++ {
		wg.Add(1)
		go func(wi int) {
			defer wg.Done()
			wqp := c.workerPlan(qp)
			if workerStats != nil {
				wqp.stats = &workerStats[wi]
				if wqp.fold != nil {
					wqp.fold.stats = wqp.stats // workerPlan's fold clone is this worker's alone
				}
			}
			// Everything below is per-goroutine by construction: the visit state (which owns
			// this worker's columnar prefilter and decompression buffer), the narrower's
			// resolved-projection cache and the subset assembler's scratch.
			var st scanVisitState
			var sc wire.SubsetScratch
			var narrow wireNarrower
			batch := getBatch()
			flush := func() {
				if len(batch.offs) > 1 {
					results <- batch
					batch = getBatch()
				}
			}
			// The same assembly the serial yieldWireRow does, writing DIRECTLY into the arena so
			// a row is copied once: narrow in the segment's own id space first, convert to
			// inline names, then subset. Both steps take the key only when the reader is
			// entitled to it.
			emit := func(w []byte, dict *segDictHandle) bool {
				start := len(batch.buf)
				w = c.narrowProjected(&narrow, sel, dict, w)
				w = c.wireToInlineRead(dict, w, sel.redact)
				out, ok := c.appendWireRow(batch.buf, w, sel, needed, seal, &sc)
				if !ok {
					batch.buf = out[:start] // discard the partial append
					return true             // not an inline ad / malformed: skip, keep scanning
				}
				batch.buf = out
				batch.offs = append(batch.offs, len(out))
				if len(batch.buf) >= wireBatchArena {
					flush()
				}
				return !stopped.Load()
			}
			for {
				if stopped.Load() {
					return
				}
				idx := int(atomic.AddInt64(&next, 1)) - 1
				if idx >= len(tasks) {
					flush()
					return
				}
				// A visitor per window because it binds the window's shard snapshot sequence;
				// st carries the expensive state across them.
				visit, _ := c.refVisitorState(&st, tasks[idx].s0, wqp, emit)
				forEachVisibleWindowRef(tasks[idx].s0, tasks[idx].win, visit)
			}
		}(i)
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	// Both exits below leave only once results is CLOSED, which happens after wg.Wait(), so every
	// worker has finished writing its counters by the time this runs.
	defer func() {
		for i := range workerStats {
			qp.stats.mergeWorker(&workerStats[i])
		}
	}()
	for b := range results {
		stop := false
		for i := 0; i+1 < len(b.offs); i++ {
			if !yield(b.buf[b.offs[i]:b.offs[i+1]]) {
				stop = true
				break
			}
		}
		select {
		case recycle <- b:
		default: // free-list full; let the arena be collected
		}
		if stop {
			stopped.Store(true)
			for range results {
				// drain so the workers and closer finish
			}
			return true
		}
	}
	return true
}
