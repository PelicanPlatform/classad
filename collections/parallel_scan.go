package collections

import (
	"sync"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
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
func (c *Collection) gatherTasks() (tasks []scanTask, totalBytes int, release func()) {
	var pinned [][]segWindow
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		if len(wins) == 0 {
			continue
		}
		pinned = append(pinned, wins)
		for i := range wins {
			tasks = append(tasks, scanTask{win: wins[i], s0: s0})
			totalBytes += wins[i].used
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
func forEachVisibleWindow(s0 uint64, w segWindow, fn func(ad []byte, codec Codec) bool) {
	for off := 0; off < w.used; {
		o := uint32(off)
		total := recTotalLen(w.data, o)
		if total == 0 {
			break
		}
		if recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0 {
			if !fn(recAd(w.data, o), w.codec) {
				return
			}
		}
		off += int(total)
	}
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

// makeWorkerPlan builds a queryPlan with fresh per-goroutine execution state (a
// Matcher, a wireScope, and a bound resolver). The program/read-plan it references is
// immutable and shared.
func (c *Collection) makeWorkerPlan(q *vm.Query) queryPlan {
	plan := q.ReadPlan()
	ws := &wireScope{ctx: c}
	return queryPlan{
		q:        q,
		plan:     plan,
		m:        q.Matcher(),
		wireOK:   q.Native() && plan.PartialSafe,
		ws:       ws,
		resolver: ws.resolve,
	}
}

// runParallelQuery executes a full-scan query, fanning out across segments when the
// scan is large enough and the collection-wide worker budget has capacity; otherwise
// it runs serially over the same snapshotted tasks. It yields each matching ad and
// returns when the consumer stops or the scan completes.
func (c *Collection) runParallelQuery(q *vm.Query, yield func(*classad.ClassAd) bool) {
	tasks, totalBytes, release := c.gatherTasks()
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
		c.scanTasksSerial(tasks, c.makeWorkerPlan(q), yield)
		return
	}
	defer func() {
		for i := 0; i < W; i++ {
			<-c.qsem
		}
	}()

	results := make(chan []*classad.ClassAd, W)
	var next int64
	var stopped atomic.Bool
	var wg sync.WaitGroup
	for i := 0; i < W; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			qp := c.makeWorkerPlan(q)
			var dbuf []byte
			batch := make([]*classad.ClassAd, 0, parallelBatch)
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
				forEachVisibleWindow(tasks[idx].s0, tasks[idx].win, func(ad []byte, codec Codec) bool {
					if stopped.Load() {
						return false
					}
					w, err := codec.Decompress(dbuf[:0], ad)
					if err != nil {
						return true
					}
					dbuf = w
					if !matchWire(w, qp) {
						return true
					}
					a, err := c.decodeWire(w)
					if err != nil {
						return true
					}
					batch = append(batch, classad.FromAST(a))
					if len(batch) >= parallelBatch {
						results <- batch
						batch = make([]*classad.ClassAd, 0, parallelBatch)
					}
					return true
				})
			}
		}()
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
}

// scanTasksSerial runs the query over the gathered tasks on the calling goroutine --
// the fallback when a scan is too small to parallelize or the worker budget is busy.
func (c *Collection) scanTasksSerial(tasks []scanTask, qp queryPlan, yield func(*classad.ClassAd) bool) {
	var dbuf []byte
	for _, t := range tasks {
		stop := false
		forEachVisibleWindow(t.s0, t.win, func(ad []byte, codec Codec) bool {
			w, err := codec.Decompress(dbuf[:0], ad)
			if err != nil {
				return true
			}
			dbuf = w
			if !matchWire(w, qp) {
				return true
			}
			a, err := c.decodeWire(w)
			if err != nil {
				return true
			}
			if !yield(classad.FromAST(a)) {
				stop = true
				return false
			}
			return true
		})
		if stop {
			return
		}
	}
}
