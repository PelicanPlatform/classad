package collections

import (
	"slices"
	"sync/atomic"
)

// AN ORDERED FAN-OUT, FOR THE READS THAT PROMISE NEWEST-FIRST.
//
// An archive is a newest-first collection (Options.ReverseScan, "matching condor_history order"),
// and until this existed that promise cost it the fan-out on every wire read: the gate read
// `... && !c.reverseScan`, because "fan-out has no cross-segment order". The unconstrained wire
// scan had no such gate and so kept the fan-out by quietly giving up the order instead -- and only
// when the worker budget happened to be free, since its serial fallback IS newest-first.
//
// This serves the four wire reads (ScanRawWire, ScanRawWireRefs and their Query forms). The
// full-decode Query keeps its `!c.reverseScan` gate: its workers yield decoded ads rather than
// arena rows, so the same lanes do not fit it unchanged.
//
// It does not have to cost that, because the order a reverse scan promises is not a SORT. It is
// the stored order read backwards: the segments newest to oldest, and each segment's records back
// to front. Segments are already ordered, so recovering the order across them needs no comparison
// between records at all -- only that the consumer take the segments' results in the right order.
// So the workers decode in parallel and the consumer drains in order, and the merge is a copy.
//
// WHAT BOUNDS THE MEMORY. Buffering whole segments would not do: an 8 MiB segment's rows, decoded
// and reassembled, are tens of MiB, and W of those in flight is a read that can be asked for a
// hundred-fold more memory than the serial one it replaces. Instead each worker owns ONE lane of
// orderedRunAhead arenas, and blocks on a full lane. The ceiling is W*(orderedRunAhead+1) arenas
// -- about 1.5 MiB at the default parallelism -- whatever the segment size, and it is the same
// ceiling whether a segment yields ten rows or a million.
//
// WHY THE TASKS ARE DEALT ROUND-ROBIN RATHER THAN STOLEN. runParallelWireQuery's workers steal
// from a shared counter, which balances skew. An ordered drain cannot use that: the consumer must
// know which lane holds task i before the task is claimed, or it has to search for it. Dealing
// task i to worker i%W makes that a `lanes[i%W]` with nothing to coordinate, and the blocking lane
// doubles as the run-ahead bound -- a worker that gets far ahead of the consumer stops on its own.
// The cost is that one slow segment stalls its lane while the others idle. On an archive the
// segments are uniform by construction (SegmentSize caps them), so there is little skew to
// balance; a collection with wildly uneven segments would want stealing and a reorder buffer, and
// would pay for both.
//
// WHAT AN EARLY STOP WASTES. A serial newest-first read that stops after ten rows reads the newest
// segment and nothing else, and a fan-out that started W workers on the newest W segments would
// have thrown most of that away -- measured at LIMIT 5 it was 8x SLOWER than the serial walk it
// replaced. So the newest segment is walked on the caller's own goroutine before any worker
// starts, and a read that stops inside it never starts one. Past it, a stopping consumer still
// wastes what the other lanes hold, which is why the run-ahead is small.
//
// ONE SHARD. The order is recovered from the flattened task list, and gatherTasksPruned flattens
// the shards one after another, so reversing it reverses the SHARDS too -- where the serial walk
// visits shard 0 then shard 1, each reversed within itself. A newest-first collection is an append
// log, which Options forces to a single shard, so this is not a case that arises; it is declined
// rather than reasoned about, because a wrong order here is silent.

// orderedRunAhead is how many wireBatchArena-sized arenas a worker may fill on its lane before
// the consumer has to catch up. It is the run-ahead bound and the memory ceiling, and it is small
// because one arena of slack is enough to keep a worker from stalling on every handoff while more
// only buys deeper buffering of rows the consumer may never ask for.
//
// It is NOT what bounds an early stop's waste -- the serial prelude below is. Shrinking it was
// tried first, with a ramped first flush of 4 KiB, and made a LIMIT 5 twice as slow again (195 us
// to 440 us): the cost of a short newest-first read is the fan-out's own setup and teardown, not
// the depth of its buffers, and a ramp only adds handoffs and arenas to it.
const orderedRunAhead = 2

// wireRowChunk is one handoff down a lane: a batch of assembled rows, or the marker that ends a
// task. A lane carries the chunks of one worker's tasks back to back, so the consumer needs the
// marker to know where one segment's rows stop and the next's begin.
type wireRowChunk struct {
	b   wireRowBatch
	eof bool
}

// wireOrderedFanouts counts the ordered fan-outs that actually happened, for the same reason
// wireQueryFanouts does: declining to fan out is silent, and without a counter a test that
// compared the ordered path with the serial one would be comparing the serial path with itself.
var wireOrderedFanouts atomic.Int64

// runOrderedWireQuery is runParallelWireQuery for a collection that promises newest-first order:
// the same segment fan-out, worker budget, arena recycling and refVisitorState reuse (and so the
// same columnar prefilter, which is the reason both reuse the visitor rather than writing their
// own record loop), but the segments are walked backward and the rows are drained in segment
// order instead of arrival order.
//
// Returns false without consuming the yield when parallelism is not engaged, and the caller runs
// the serial path -- exactly as runParallelWireQuery does.
func (c *Collection) runOrderedWireQuery(qp queryPlan, sel *wireSubsetSelector, needed int, yield func([]byte) bool) bool {
	// Gather with the segment counts held ASIDE: when the gate below declines, the caller re-runs
	// the read serially and prunes (and counts) the same segments again.
	var gathered ScanStats
	gq := qp
	gq.stats = &gathered
	tasks, totalBytes, release := c.gatherTasksPruned(gq)
	// Newest segment first. gatherTasksPruned returns them in append order, and within an append
	// log that is oldest first; the records inside each are reversed by the walk below.
	slices.Reverse(tasks)
	if c.qsem == nil || len(c.shards) != 1 || len(tasks) < 2 || totalBytes < c.parallelMinBytes {
		release()
		return false // nothing yielded: the caller runs the whole read serially
	}
	// From here this call owns the read: it has yielded, or is about to, so it can no longer hand
	// the read back. The worker budget is asked for BELOW, after the prelude, and a refusal is
	// answered by finishing serially here rather than by returning false.
	defer release()
	qp.stats.segments(gathered.SegmentsTotal, gathered.SegmentsPruned)

	// THE NEWEST SEGMENT IS WALKED HERE, ON THE CALLER'S GOROUTINE, BEFORE ANY WORKER STARTS.
	//
	// A newest-first read that stops after a handful of rows -- condor_history's own shape -- never
	// leaves the newest segment, and for it the fan-out is pure overhead: six goroutines, six
	// worker plans with their own prefilters and assemblers, six arenas, and a teardown that waits
	// for all of them. Measured at LIMIT 5 over a 100k archive that was 195 us against the serial
	// walk's 24 us, an 8x regression on the query this collection exists to serve, and shrinking
	// the buffers did not touch it because the buffers were not the cost.
	//
	// Walking the first segment here means such a read pays nothing for a fan-out it will not use,
	// and a read that does go on pays the setup once, after one segment of the scan -- 1/293 of it
	// on that same archive.
	if !c.walkTasksOrdered(tasks[:1], qp, sel, yield) {
		return true
	}
	tasks = tasks[1:]

	W := 0
	if len(tasks) >= 2 {
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
		c.walkTasksOrdered(tasks, qp, sel, yield)
		return true
	}
	defer func() {
		for i := 0; i < W; i++ {
			<-c.qsem
		}
	}()
	wireOrderedFanouts.Add(1)

	seal := c.renderKey(sel.redact)

	// Per-worker counters, merged once below: ScanStats carries no synchronization.
	var workerStats []ScanStats
	if qp.stats != nil {
		workerStats = make([]ScanStats, W)
	}

	lanes := make([]chan wireRowChunk, W)
	for i := range lanes {
		lanes[i] = make(chan wireRowChunk, orderedRunAhead)
	}
	// Arena free-list, shared: a drained batch goes back to whichever worker asks next. Sized for
	// every arena that can be in flight plus one per lane being filled.
	recycle := make(chan wireRowBatch, W*(orderedRunAhead+2))
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
	var stopped atomic.Bool
	// done releases a worker blocked on a full lane. stopped alone cannot: it is only read between
	// records, and a worker parked in a channel send reads nothing.
	done := make(chan struct{})
	var closeOnce atomic.Bool
	stopAll := func() {
		if closeOnce.CompareAndSwap(false, true) {
			stopped.Store(true)
			close(done)
		}
	}
	wg := workerGroup{stop: &stopped}
	for i := 0; i < W; i++ {
		wg.Go(func() {
			wi := i
			lane := lanes[wi]
			// A closed lane is how the consumer learns this worker has no more tasks, and --
			// because capture() recovers a panic outside fn -- how it learns of a panicking one
			// instead of waiting forever for a chunk nobody will send.
			defer close(lane)
			wqp := c.workerPlan(qp)
			if workerStats != nil {
				wqp.stats = &workerStats[wi]
				if wqp.fold != nil {
					wqp.fold.stats = wqp.stats
				}
			}
			// Per-goroutine by construction, as in runParallelWireQuery: the visit state (this
			// worker's columnar prefilter and decompression buffer), the narrower's cache, the
			// subset assembler's scratch -- and here also the reverse walk's offset slice.
			var st scanVisitState
			var narrow wireNarrower
			var offs []uint32
			asm := c.newWireRowAssembler(sel)
			batch := getBatch()
			send := func(ch wireRowChunk) bool {
				select {
				case lane <- ch:
					return true
				case <-done:
					return false
				}
			}
			flush := func() bool {
				if len(batch.offs) <= 1 {
					return true
				}
				if !send(wireRowChunk{b: batch}) {
					return false
				}
				batch = getBatch()
				return true
			}
			full := false // the last flush was refused: stop, do not keep assembling
			emit := func(w []byte, dict *segDictHandle) bool {
				start := len(batch.buf)
				w = c.narrowProjected(&narrow, sel, dict, w)
				out, ok := asm.appendRowConverted(batch.buf, dict, w, needed, seal)
				if !ok {
					batch.buf = out[:start] // discard the partial append
					return true             // not an inline ad / malformed: skip, keep scanning
				}
				batch.buf = out
				batch.offs = append(batch.offs, len(out))
				if len(batch.buf) >= wireBatchArena {
					if !flush() {
						full = true
						return false
					}
				}
				return !stopped.Load()
			}
			for idx := wi; idx < len(tasks); idx += W {
				if stopped.Load() {
					return
				}
				visit, _ := c.refVisitorState(&st, tasks[idx].s0, wqp, emit)
				forEachVisibleWindowRefReverse(tasks[idx].s0, tasks[idx].win, &offs, visit)
				if full || !flush() {
					return
				}
				if !send(wireRowChunk{eof: true}) {
					return
				}
			}
		})
	}
	// Deferred in this order so the drain runs first: a worker's panic is re-raised only once every
	// worker has returned, and a panic in yield still stops and waits for them before release()
	// unpins what they read.
	defer wg.rethrow()
	defer func() {
		stopAll()
		for _, l := range lanes {
			for range l { //nolint:revive // drain until the worker closes it
			}
		}
		wg.Wait()
		// Merged HERE rather than from a defer of its own. A worker writes its counters until it
		// returns, and the consumer loop below ends when it has read the last eof -- which the
		// worker sends BEFORE returning. A separate defer would be LIFO-first and so could read a
		// counter set its worker was still adding to; after wg.Wait() none of them is.
		for i := range workerStats {
			qp.stats.mergeWorker(&workerStats[i])
		}
	}()

	// THE ORDER. Task i's rows are on lane i%W because worker w was dealt tasks w, w+W, ...; the
	// worker writes its tasks' chunks down that lane in the order it was dealt them, so reading
	// lane i%W up to its next eof marker reads exactly task i.
	for i := range tasks {
		lane := lanes[i%W]
		for {
			ch, ok := <-lane
			if !ok {
				return true // the worker stopped early (or panicked); the defers settle it
			}
			if ch.eof {
				break
			}
			stop := false
			for j := 0; j+1 < len(ch.b.offs); j++ {
				if !yield(ch.b.buf[ch.b.offs[j]:ch.b.offs[j+1]]) {
					stop = true
					break
				}
			}
			select {
			case recycle <- ch.b:
			default: // free-list full; let the arena be collected
			}
			if stop {
				return true
			}
		}
	}
	return true
}

// walkTasksOrdered walks tasks newest-first on the CALLING goroutine, yielding each matching row.
// It returns false once the consumer has stopped.
//
// It is the ordered fan-out's serial half: the prelude over the newest segment, and the remainder
// when the worker budget turns the read down after that prelude has already yielded. The rows it
// produces are assembled exactly as a worker's are (yieldWireRow is what the plain serial read
// uses), so the two halves of one read cannot differ in what they emit -- only in which goroutine
// built it.
func (c *Collection) walkTasksOrdered(tasks []scanTask, qp queryPlan, sel *wireSubsetSelector, yield func([]byte) bool) bool {
	if len(tasks) == 0 {
		return true
	}
	// workerPlan, although nothing here is a worker: it gives this walk its own matcher, wire
	// scope and projection cache, which is what keeps it from sharing execution state with the
	// workers that take the rest of the tasks. It also drops the stats sink, so the counts are
	// accumulated here and merged the same way a worker's are.
	wqp := c.workerPlan(qp)
	var own ScanStats
	if qp.stats != nil {
		wqp.stats = &own
		if wqp.fold != nil {
			wqp.fold.stats = &own
		}
		defer func() { qp.stats.mergeWorker(&own) }()
	}
	emit := c.yieldWireRow(yield, sel)
	var st scanVisitState
	var offs []uint32
	for i := range tasks {
		visit, stopped := c.refVisitorState(&st, tasks[i].s0, wqp, emit)
		forEachVisibleWindowRefReverse(tasks[i].s0, tasks[i].win, &offs, visit)
		if stopped() {
			return false
		}
	}
	return true
}

// runWireFanout is the constrained wire read's one entry to fan-out: the stealing fan-out for a
// collection with no order to keep, the ordered one for a collection that promises newest-first.
//
// Both gates that call it used to carry `&& !c.reverseScan`, which denied an archive -- the only
// kind of collection that sets it -- a fan-out entirely. The choice is here rather than at the
// call sites so the two spellings of the read cannot drift into making it differently.
func (c *Collection) runWireFanout(qp queryPlan, sel *wireSubsetSelector, needed int, yield func([]byte) bool) bool {
	if c.reverseScan {
		return c.runOrderedWireQuery(qp, sel, needed, yield)
	}
	return c.runParallelWireQuery(qp, sel, needed, yield)
}

// runUnconstrainedWireFanout is runWireFanout for the wire scans that carry no constraint.
//
// Their serial fallback goes through scanShard, which honours reverseScan -- so the unconstrained
// read was ALREADY newest-first whenever it ran serially, and silently was not whenever it fanned
// out, because runParallelWireScan carries no order. The two orders were a property of how busy
// the worker budget happened to be. Here the same choice runWireFanout makes for a constrained
// read is made for this one: an ordered fan-out on a newest-first collection, the stealing one
// otherwise.
//
// The caller passes the plan its serial fallback uses, so the two agree whichever way the read
// goes. For a refs scan that is now a real projection plan (see rawRefsProjPlan); it used to be
// none, which cost the columnar projection on every record.
//
// An earlier version of this comment said that giving the unconstrained read the projection was a
// separate change with its own crossover, "1.24x faster at 20k rows and 2.4x slower at 100k when
// it cannot fan out". Both halves of that were artefacts. The slowdown was serial columnar being
// compared against a parallel record scan, which this function is what fixes; and the small
// margin was the fixture, whose ads carry eight attributes, so projecting three of them leaves
// nothing to narrow. At the width a completed-job ad actually has, the projection is worth 8x in
// time and 90x in allocations (see db.BenchmarkRawWireRefsByWidth).
func (c *Collection) runUnconstrainedWireFanout(proj *projPlan, sel *wireSubsetSelector, redact bool, yield func([]byte) bool) bool {
	if c.reverseScan {
		return c.runOrderedWireQuery(queryPlan{proj: proj, sealed: sealedUseFor(redact)}, sel, sel.neededCount(), yield)
	}
	return c.runParallelWireScan(sel, sel.neededCount(), yield)
}
