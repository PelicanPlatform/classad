package collections

import (
	"slices"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/classad"
)

// THE ORDERED FAN-OUT FOR THE FULL-DECODE QUERY.
//
// A newest-first collection (Options.ReverseScan -- an archive, and only an archive) promises
// condor_history order, and that promise cost Query its fan-out: the gate carried
// `&& !c.reverseScan`, so the one read shape a history archive serves most ran serial however
// large the scan. The wire reads answer the same collection through runOrderedWireQuery; this is
// that structure for the read that yields decoded ads.
//
// The order is recovered the same way, and for the same reason: a reverse scan is not a SORT but
// the stored order read backwards -- segments newest to oldest, each segment's records back to
// front. Segments are already ordered, so no record is ever compared with another. The workers
// decode in parallel and the consumer drains lane by lane, and the merge is a copy.
//
// WHAT IS DIFFERENT FROM THE WIRE FORM, and why it needed its own implementation rather than a
// shared one. A wire worker fills an arena and hands over a byte slice the consumer can recycle;
// the arena count is the memory bound, independent of how many rows it holds. Here a worker hands
// over decoded *classad.ClassAd, which are heap objects of unbounded individual size, so the bound
// is a COUNT of ads in flight: W*(orderedAdRunAhead+1)*parallelBatch, about 1150 ads at the
// default parallelism. There is no free-list, because a yielded ad belongs to the caller and
// cannot be handed back -- which is also why the wire path's recycle channel has no counterpart.
//
// Everything that decides WHAT is emitted is shared with the serial read rather than reimplemented:
// the workers go through refVisitorState, so the columnar prefilter, delta merging, the system-key
// filter and the matcher are the serial path's. A hand-written record loop here would reassemble
// every record the WHERE rejects, which is the mistake runParallelQuery documents having made.
//
// ROUND-ROBIN, NOT STEALING, for the reason runOrderedWireQuery gives: the consumer must know
// which lane holds task i without searching for it, and dealing task i to worker i%W makes that
// `lanes[i%W]`. A full lane doubles as the run-ahead bound. On an archive the segments are uniform
// (SegmentSize caps them) so there is little skew for stealing to balance.
//
// THE NEWEST SEGMENT IS WALKED ON THE CALLER'S GOROUTINE before any worker starts. A newest-first
// read that stops after a handful of rows never leaves that segment, and for it a fan-out is pure
// overhead -- the wire path measured an 8x regression at LIMIT 5 before it did this. A read that
// stops inside the prelude starts no worker at all.
//
// ONE SHARD, for the reason the wire path declines otherwise: the order comes from the flattened
// task list, and gatherTasksPruned flattens shards one after another, so reversing it reverses the
// shards too, where the serial walk visits shard 0 then shard 1 each reversed within itself. A
// newest-first collection is an append log, which Options forces to a single shard, so this does
// not arise; it is declined rather than reasoned about, because a wrong order here is silent.

// orderedAdRunAhead is how many batches a worker may put on its lane before the consumer has to
// catch up. It is the run-ahead bound and, with parallelBatch, the memory ceiling. Small for the
// same reason the wire path's is: one batch of slack keeps a worker from stalling on every
// handoff, and more only buffers ads the consumer may never ask for.
const orderedAdRunAhead = 2

// orderedAdChunk is one handoff down a lane: a batch of decoded ads, or the marker that ends a
// task. A lane carries one worker's tasks back to back, so the consumer needs the marker to know
// where one segment's ads stop and the next's begin.
type orderedAdChunk struct {
	ads []*classad.ClassAd
	eof bool
}

// orderedQueryFanouts counts the ordered full-decode fan-outs that actually happened. Declining is
// silent by design -- the read falls back and yields the same ads in the same order -- so without
// a counter a test comparing this path with the serial one would be comparing the serial path with
// itself. See queryFanouts, which exists for the same reason.
var orderedQueryFanouts atomic.Int64

// runOrderedQuery is runParallelQuery for a collection that promises newest-first order.
//
// Returns false WITHOUT having yielded when it cannot engage, and the caller runs the whole read
// serially. Once it has yielded it owns the read and returns true, finishing serially itself if
// the worker budget turns it down after the prelude.
func (c *Collection) runOrderedQuery(qp queryPlan, yield func(*classad.ClassAd) bool, redact bool) bool {
	// Gather with the segment counts held ASIDE: when the gate below declines, the caller re-runs
	// the read serially and prunes (and counts) the same segments again.
	var gathered ScanStats
	gq := qp
	gq.stats = &gathered
	tasks, totalBytes, release := c.gatherTasksPruned(gq)
	// Newest segment first. gatherTasksPruned returns append order, which in an append log is
	// oldest first; the records inside each segment are reversed by the walk itself.
	slices.Reverse(tasks)
	if c.qsem == nil || len(c.shards) != 1 || len(tasks) < 2 || totalBytes < c.parallelMinBytes {
		release()
		return false
	}
	defer release()
	qp.stats.segments(gathered.SegmentsTotal, gathered.SegmentsPruned)

	if !c.walkTasksOrderedAds(tasks[:1], qp, yield, redact) {
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
		c.walkTasksOrderedAds(tasks, qp, yield, redact)
		return true
	}
	defer func() {
		for i := 0; i < W; i++ {
			<-c.qsem
		}
	}()
	orderedQueryFanouts.Add(1)

	// Per-worker counters, merged once below: ScanStats carries no synchronization.
	var workerStats []ScanStats
	if qp.stats != nil {
		workerStats = make([]ScanStats, W)
	}

	lanes := make([]chan orderedAdChunk, W)
	for i := range lanes {
		lanes[i] = make(chan orderedAdChunk, orderedAdRunAhead)
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
			// Per-goroutine by construction: this worker's columnar prefilter and its colScope,
			// the decompression buffer, and the reverse walk's offset slice.
			var st scanVisitState
			var offs []uint32
			batch := make([]*classad.ClassAd, 0, parallelBatch)
			send := func(ch orderedAdChunk) bool {
				select {
				case lane <- ch:
					return true
				case <-done:
					return false
				}
			}
			flush := func() bool {
				if len(batch) == 0 {
					return true
				}
				if !send(orderedAdChunk{ads: batch}) {
					return false
				}
				batch = make([]*classad.ClassAd, 0, parallelBatch)
				return true
			}
			full := false // the last flush was refused: stop, do not keep decoding
			emit := func(w []byte, dict *segDictHandle) bool {
				// Decoded HERE rather than through the caller's emit, so redact has to be
				// threaded in: a redacted Query returning opened secrets is exactly what the
				// decode-it-yourself shape costs when the entitlement is not carried with it.
				a, err := c.decodeWireDictAs(dict, w, redact)
				if err != nil {
					noteReadSkip(err)
					return true // skip malformed record, keep scanning
				}
				batch = append(batch, classad.FromAST(a))
				if len(batch) >= parallelBatch {
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
				if !send(orderedAdChunk{eof: true}) {
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
		// Merged HERE rather than from a defer of its own: a worker writes its counters until it
		// returns, and the consumer loop below ends at the last eof, which the worker sends BEFORE
		// returning. A separate defer would be LIFO-first and could read a counter set its worker
		// was still adding to; after wg.Wait() none of them is.
		for i := range workerStats {
			qp.stats.mergeWorker(&workerStats[i])
		}
	}()

	// THE ORDER. Task i's ads are on lane i%W because worker w was dealt tasks w, w+W, ...; the
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
			for _, a := range ch.ads {
				if !yield(a) {
					return true
				}
			}
		}
	}
	return true
}

// walkTasksOrderedAds walks tasks newest-first on the CALLING goroutine, yielding each matching ad.
// It returns false once the consumer has stopped.
//
// It is the ordered fan-out's serial half: the prelude over the newest segment, and the remainder
// when the worker budget turns the read down after that prelude has already yielded. The ads it
// produces come from yieldAdAs -- what the plain serial read uses -- so the two halves of one read
// cannot differ in what they emit, only in which goroutine built it.
func (c *Collection) walkTasksOrderedAds(tasks []scanTask, qp queryPlan, yield func(*classad.ClassAd) bool, redact bool) bool {
	if len(tasks) == 0 {
		return true
	}
	// workerPlan, although nothing here is a worker: it gives this walk its own matcher, wire
	// scope and projection cache, so it shares no execution state with the workers that take the
	// rest of the tasks. It also drops the stats sink, so the counts accumulate here and are
	// merged the way a worker's are.
	wqp := c.workerPlan(qp)
	var own ScanStats
	if qp.stats != nil {
		wqp.stats = &own
		if wqp.fold != nil {
			wqp.fold.stats = &own
		}
		defer func() { qp.stats.mergeWorker(&own) }()
	}
	emit := c.yieldAdAs(yield, redact)
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
