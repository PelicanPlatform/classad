package collections

import (
	"sync"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/classad"
)

// Cross-segment query parallelism. A full-scan Query can fan out across the
// collection's segments: each segment is an independent unit of work (immutable
// bytes, lock-free reads), and the version of a key visible at a shard's snapshot
// sequence lives in exactly one segment, so workers process disjoint segments and
// their results simply concatenate -- no cross-segment dedup. Opt-in and gated; see
// docs/PARALLEL_QUERY.md.

const (
	// defaultParallelMinBytes is the minimum total live segment bytes a scan must
	// cover before fan-out is worth its fixed cost. Benchmark-tuned.
	defaultParallelMinBytes = 1 << 20
	// parallelBatch caps how many decoded ads a worker buffers before handing them to
	// the consumer, so a low-selectivity segment does not buffer unboundedly.
	parallelBatch = 64
	// defaultAutoQueryWorkers is the per-query worker cap the auto policy
	// (QueryParallelism=0) uses, clamped to GOMAXPROCS. Deliberately modest: returns
	// diminish past a few workers, and a lower cap lets several concurrent queries
	// each fan out rather than one taking the whole worker budget.
	defaultAutoQueryWorkers = 6
)

// scanTask is one segment window to scan, tagged with its shard's snapshot sequence
// (S0 is per shard).
type scanTask struct {
	win segWindow
	s0  uint64
}

// gatherTasks snapshots every shard once and flattens their live segment windows into
// a task list (with the total live bytes). release unpins every window; the caller
// must invoke it only after all workers have finished reading.
//
// It is the unfiltered form, for the scans that carry no query (the wire-row relay
// scan) or no queryPlan at all (matchmaking). A filtered scan uses gatherTasksPruned.
func (c *Collection) gatherTasks() (tasks []scanTask, totalBytes int, release func()) {
	return c.gatherTasksPruned(queryPlan{})
}

// gatherTasksPruned is gatherTasks with qp's sealed-segment zone maps applied to each
// shard's windows, so a parallel full scan skips exactly the segments the serial path
// (scanWindows) skips. Without this a zoned collection full-scanned in parallel while
// the serial path over the same snapshot proved whole sealed segments impossible --
// fan-out silently bought back the work zone maps had already removed.
//
// A pruned window is still PINNED and still released: the narrowing decides what is
// walked, never what was snapshotted. totalBytes counts only the windows that will be
// walked, because it exists to size the fan-out against the work actually left.
//
// The pruning runs here, on the calling goroutine, which is also why qp keeps its stats
// sink: the per-worker plans drop it (workerPlan), but the segment counts are settled
// before any worker starts.
func (c *Collection) gatherTasksPruned(qp queryPlan) (tasks []scanTask, totalBytes int, release func()) {
	var pinned [][]segWindow
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		if len(wins) == 0 {
			continue
		}
		pinned = append(pinned, wins)
		walk := c.zoneWalk(wins, qp)
		for i := range walk {
			tasks = append(tasks, scanTask{win: walk[i], s0: s0})
			totalBytes += walk[i].used
		}
	}
	return tasks, totalBytes, func() {
		for _, w := range pinned {
			releaseWindows(w)
		}
	}
}

// forEachVisibleWindow walks one window's records visible at S0 (the single-window
// form of forEachVisible), used by the per-segment workers.
func (c *Collection) forEachVisibleWindow(s0 uint64, w segWindow, fn func(ad []byte, codec Codec) bool) {
	var rbuf []byte
	forEachVisibleWindowRef(s0, w, func(r recRef) bool {
		ad, codec, ok := c.adBytes(r, s0, &rbuf)
		if !ok {
			return true
		}
		return fn(ad, codec)
	})
}

// forEachVisibleWindowKeyed is forEachVisibleWindow that also passes each record's
// key, so the parallel wire-relay workers can filter internal system records out of
// the client-facing result (the serial scanShard path does the same). The query
// workers do not use it: they go through refVisitorState, which applies the same
// filter along with the prefilter and the reassembly rules.
func (c *Collection) forEachVisibleWindowKeyed(s0 uint64, w segWindow, fn func(key, ad []byte, codec Codec) bool) {
	var rbuf []byte
	forEachVisibleWindowRef(s0, w, func(r recRef) bool {
		ad, codec, ok := c.adBytes(r, s0, &rbuf)
		if !ok {
			return true
		}
		return fn(r.key(), ad, codec)
	})
}

// tryAcquire takes up to n tokens from the worker budget without blocking, returning
// how many it got (0..n). The taken tokens must be released by the caller. It is
// greedy (grab whatever is free) rather than all-or-nothing: under concurrent-query
// load the budget is mostly spoken for, so a query naturally gets few workers (or
// falls to serial), which the contention benchmark shows degrades gracefully.
func tryAcquire(sem chan struct{}, n int) int {
	got := 0
	for got < n {
		select {
		case sem <- struct{}{}:
			got++
		default:
			return got
		}
	}
	return got
}

// queryFanouts counts the full-decode fan-outs that actually happened.
//
// The counter is the only way a test can tell the fan-out from the serial fallback, because
// declining to fan out is SILENT by design -- runParallelQuery runs the same tasks serially and
// yields the same ads. Without it, a change that disabled the dispatch would leave every
// parallel-vs-serial comparison passing, having compared the serial path with itself.
var queryFanouts atomic.Int64

// runParallelQuery executes a full-scan query, fanning out across segments when the
// scan is large enough and the collection-wide worker budget has capacity; otherwise
// it runs serially over the same snapshotted tasks. It yields each matching ad and
// returns when the consumer stops or the scan completes.
// runParallelQuery fans a full scan across workers. redact is the read's entitlement to sealed values,
// threaded because this path decodes ads ITSELF rather than through the caller's emit -- which is exactly
// how a redacted Query kept returning opened secrets while every emit-based path was fixed.
//
// WHY THE WORKERS GO THROUGH refVisitorState RATHER THAN A LOOP OF THEIR OWN. They used to walk
// the records, decompress, matchWire and decode, which skipped the columnar prefilter that only
// the refVisitor path has -- so over a columnarized store every record the WHERE rejects was
// rebuilt from the arena first. On a 100k-job store `JobUniverse==5 && JobPrio>100` decides all
// 100000 records from the columns and reassembles none of them; the hand-written loop reassembled
// all 100000, which is far more work than a handful of workers can win back. Reusing the visitor
// also keeps delta merging, columnar reassembly, the columnar projection and the system-key filter
// identical to the serial scan instead of a second copy that can drift from it.
//
// The prefilter is not safe to share (its colScope caches per-block state), so each worker builds
// its OWN through scanVisitState -- the same per-goroutine-state rule that governs the matcher,
// the wireScope, the projection plan's segment cache and the decompression buffer.
func (c *Collection) runParallelQuery(qp queryPlan, yield func(*classad.ClassAd) bool, redact bool) {
	tasks, totalBytes, release := c.gatherTasksPruned(qp)
	defer release()

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
			<-c.qsem // release the lone token; not enough to parallelize
		}
		sp := c.workerPlan(qp)
		// workerPlan drops the stats sink because a WORKER must not share one. There is nothing
		// to share with on this goroutine, and leaving it dropped made an EXPLAIN ANALYZE of the
		// serial fallback report zero records visited for a scan that walked every one of them.
		sp.stats = qp.stats
		if sp.fold != nil {
			sp.fold.stats = qp.stats
		}
		c.scanTasksSerial(tasks, sp, yield, redact)
		return
	}
	defer func() {
		for i := 0; i < W; i++ {
			<-c.qsem
		}
	}()
	queryFanouts.Add(1)

	// Per-worker counters, merged once below: ScanStats is a plain counter set with no
	// synchronization, so workerPlan drops the shared sink and each worker counts into its own.
	// Only when the caller actually asked for stats -- otherwise the workers keep workerPlan's nil
	// sink and the per-record counting stays the nil check it is on the serial path.
	var workerStats []ScanStats
	if qp.stats != nil {
		workerStats = make([]ScanStats, W)
	}
	results := make(chan []*classad.ClassAd, W)
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
			// This worker's own visit state: it owns the columnar prefilters (and their
			// colScopes), the columnar projection scope and scratch, and the decompression
			// buffer. One set per goroutine, carried across the windows this worker steals so
			// the prefilter is built once per scan rather than once per segment.
			var st scanVisitState
			batch := make([]*classad.ClassAd, 0, parallelBatch)
			emit := func(w []byte, dict *segDictHandle) bool {
				a, err := c.decodeWireDictAs(dict, w, redact)
				if err != nil {
					noteReadSkip(err)
					return true // skip malformed record, keep scanning
				}
				batch = append(batch, classad.FromAST(a))
				if len(batch) >= parallelBatch {
					results <- batch
					batch = make([]*classad.ClassAd, 0, parallelBatch)
				}
				return !stopped.Load()
			}
			for {
				if stopped.Load() {
					return
				}
				idx := int(atomic.AddInt64(&next, 1)) - 1
				if idx >= len(tasks) {
					if len(batch) > 0 {
						results <- batch
					}
					return
				}
				// A visitor per window because it binds the window's shard snapshot sequence;
				// st carries the expensive state across them.
				visit, _ := c.refVisitorState(&st, tasks[idx].s0, wqp, emit)
				forEachVisibleWindowRef(tasks[idx].s0, tasks[idx].win, visit)
			}
		}(i)
	}
	go func() { wg.Wait(); close(results) }()

	// Consumer: always drain to close (so a blocked worker send unblocks and every
	// worker exits before release() unpins), but stop yielding once asked to stop.
	stop := false
	for b := range results {
		for _, a := range b {
			if !stop && !yield(a) {
				stop = true
				stopped.Store(true)
			}
		}
	}
	// results is closed only after wg.Wait(), so every worker has finished writing its
	// counters by the time this merges them.
	for i := range workerStats {
		qp.stats.mergeWorker(&workerStats[i])
	}
}

// scanTasksSerial runs the query over the gathered tasks on the calling goroutine --
// the fallback when a scan is too small to parallelize or the worker budget is busy.
// scanTasksSerial is runParallelQuery's fallback when it cannot get enough workers; redact carries the
// same entitlement (see runParallelQuery).
//
// It goes through the same refVisitorState the workers and the plain serial scan use, so the
// fallback keeps the columnar prefilter rather than being the one path that reassembles every
// rejected record.
func (c *Collection) scanTasksSerial(tasks []scanTask, qp queryPlan, yield func(*classad.ClassAd) bool, redact bool) {
	var st scanVisitState
	emit := c.yieldAdAs(yield, redact)
	for _, t := range tasks {
		visit, stopped := c.refVisitorState(&st, t.s0, qp, emit)
		forEachVisibleWindowRef(t.s0, t.win, visit)
		if stopped() {
			return
		}
	}
}
