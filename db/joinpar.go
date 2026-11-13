package db

import (
	"fmt"
	"iter"
	"runtime"
	"sync"
	"sync/atomic"
)

// WALKING THE PAIR LOOP IN PARALLEL.
//
// The join's reads fan out already; its WALK did not. One goroutine built the hashed sides,
// then probed them with every scanned row, evaluated the condition, formed the row and
// yielded it -- and that walk is where the join's time goes once the read is columnar.
// Measured on a columnarized archive join, serial 241 ms against 117 ms with the reads fanned
// out: a 2.06x that Amdahl pins there, because 54% of the work was in a walk that one
// goroutine did. P independent walks over the same hashes measured 2.05x on top of that.
//
// WHAT IS SHARED AND WHY IT IS SAFE. The plan is immutable once compiled. The stages are
// written by buildStage before any walker starts and only read afterwards. The rows in those
// stages are shared, which is safe because the classad package documents that reading an ad
// -- evaluation included -- does not modify it, and because joinRow's one lazily-written
// field is an atomic pointer whose losing racer simply drops an equal ad. Everything a walker
// writes is in its own joinWalk: the accumulated row, the splice scratch, the output buffer
// and its own JoinStats, merged at the end.
//
// ORDER IS PRESERVED. Scan row i goes to walker i%P and the drainer takes lanes in the same
// rotation, so the rows come back in scan order without anything comparing them -- the same
// trick the ordered wire fan-out uses. That matters because a history join's FROM side is an
// archive, whose scan is newest-first; losing that would silently reorder condor_history.
// Each message carries ONE scan row's output, which is what makes the rotation line up.

// forceSerialJoinWalk disables the parallel walk, for the equivalence test that compares it
// with the serial one. It is a plain variable rather than a parameter because nothing outside
// a test may set it, and a test that does must not call t.Parallel -- Go runs parallel tests
// only after the serial ones finish, which is what keeps this from needing synchronisation.
var forceSerialJoinWalk bool

// joinParallelWalks counts joins that actually walked in parallel. Declining is SILENT -- the
// serial walk answers identically -- so a test comparing the two would otherwise be free to
// compare the serial path with itself and pass.
var joinParallelWalks atomic.Uint64

// joinWalkerCap is how many walkers a single join may use.
//
// Half of GOMAXPROCS, not all of it: the join's own reads fan out from the collections'
// worker budget at the same time, and a walk that took every core would leave the scan
// feeding it nothing to walk. This is the conservative half of a split that nothing
// currently enforces jointly -- see the note on joinSem.
func joinWalkerCap() int {
	n := runtime.GOMAXPROCS(0) / 2
	if n < 1 {
		return 1
	}
	return n
}

// newJoinSem is the per-catalog budget for join walkers.
//
// It bounds walkers across CONCURRENT joins, so ten joins at once cannot take ten times the
// walkers one join takes; tryAcquire is non-blocking, so a join that finds the budget spent
// walks serially rather than queueing. The reads those joins issue draw from a DIFFERENT
// budget, one per collection, so the two are not jointly bounded. That is a pre-existing
// property of the read budget (which is documented as machine-wide and is in fact per
// collection), not something this introduces, and halving the walker cap above is the
// conservative response to it.
func newJoinSem() chan struct{} { return make(chan struct{}, joinWalkerCap()) }

// rowArena lets a batch carry its rows as bytes in ONE buffer instead of one allocation each.
//
// Only a row form that already IS bytes can use it, and that is the form the relay wants: a
// wire row aliases the walker's buffer, so without this each emitted row has to be copied
// into a slice of its own -- 80k allocations and 13 MB per op on a 20k-job join, on a join
// whose cost tracks bytes churned. The ClassAd form passes nil and clones instead, which
// costs it nothing: composite already builds a fresh ad per row.
type rowArena[T any] struct {
	// appendTo appends one row's bytes to dst and returns the extended buffer. Growth is
	// safe: the drainer slices the arena only after the whole batch has arrived, and it
	// slices by the recorded offsets rather than holding sub-slices taken as rows were added.
	appendTo func(dst []byte, row T) []byte
	// wrap turns one row's bytes back into the row form. For a wire row this is identity.
	wrap func([]byte) T
}

// joinBatch is one scan row's output rows, moved to the drainer as a unit.
type joinBatch[T any] struct {
	// rows holds the rows when there is no arena; arena and ends hold them when there is.
	// ends[i] is where row i finishes, so row i is arena[ends[i-1]:ends[i]].
	rows  []T
	arena []byte
	ends  []int
	// scanEnds[j] is how many rows the batch held after the chunk's j-th scan row. The
	// drainer needs these because the LIMIT is checked BETWEEN SCAN ROWS -- that is where
	// the serial walk checks it -- and a chunk holds many. Without them a chunk of 32 could
	// overshoot a limit by everything the other 31 rows matched.
	scanEnds []int
	// scan is the scan row this came from, for the drainer's assertion that the rotation
	// has not slipped. Keeping it costs a word and turns a silent reordering into a panic
	// during development.
	scan int
}

// runJoinParallel walks the scanned side across P goroutines, in scan order.
//
// ran is false WITHOUT anything having been yielded when it declines (no budget, nothing to
// gain), and the caller walks serially. Once it has yielded it owns the read.
//
// whole says the stream was walked to the end: no limit tripped, no consumer broke out, no
// walker's own budget failed. It is what tells the caller whether a right-outer step's
// leftover rows may be emitted -- an interrupted walk has not finished deciding what matched
// (see emitUnmatchedPreserved).
func runJoinParallel[T any](p *joinPlan, sem chan struct{}, stages []*joinStage,
	scanSeq iter.Seq[*joinRow], stats *JoinStats,
	mk func(wk *joinWalk, p *joinPlan, rows []*joinRow) (T, bool),
	clone func(T) T, arena *rowArena[T], yield func(T) bool) (ran, whole bool) {

	if sem == nil || forceSerialJoinWalk || p.limits.MaxPairs <= 0 {
		return false, false
	}
	P := tryAcquireJoin(sem, joinWalkerCap())
	if P < 2 {
		for i := 0; i < P; i++ {
			<-sem
		}
		return false, false // not enough budget to be worth splitting; the caller walks it
	}
	defer func() {
		for i := 0; i < P; i++ {
			<-sem
		}
	}()

	joinParallelWalks.Add(1)

	const laneDepth = 2
	inbox := make([]chan scanChunk, P)
	lanes := make([]chan *joinBatch[T], P)
	for i := range inbox {
		inbox[i] = make(chan scanChunk, laneDepth)
		lanes[i] = make(chan *joinBatch[T], laneDepth)
	}
	var stopped atomic.Bool
	done := make(chan struct{}) // releases a walker parked in a lane send

	// The dealer drives the scan. It cannot be this goroutine: this one has to be free to
	// yield, because an iter.Seq's rows are yielded on the caller's goroutine.
	go func() {
		chunk := 0
		buf := make([]*joinRow, 0, scanChunkRows)
		flush := func() bool {
			if len(buf) == 0 {
				return true
			}
			select {
			case inbox[chunk%P] <- scanChunk{rows: buf, idx: chunk}:
			case <-done:
				return false
			}
			chunk++
			buf = make([]*joinRow, 0, scanChunkRows) // the walker keeps the one just sent
			return true
		}
		for l := range scanSeq {
			if stopped.Load() {
				break
			}
			buf = append(buf, l)
			if len(buf) == scanChunkRows && !flush() {
				break
			}
		}
		flush()
		for _, ch := range inbox {
			close(ch)
		}
	}()

	walkerStats := make([]JoinStats, P)
	var wg sync.WaitGroup
	for w := 0; w < P; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			defer close(lanes[w])
			wk := newJoinWalk(p)
			// This walker's share of the pair budget, so P walkers together are held to
			// the same total a serial walk is; the drainer owns the row limit.
			wk.maxPairs = p.limits.MaxPairs / P
			if wk.maxPairs < 1 {
				wk.maxPairs = 1
			}
			wk.noRowLimit = true
			st := &walkerStats[w]
			var batch *joinBatch[T]
			// Arenas are sized from what THIS walker's chunks have actually needed, not
			// from a guess. Guessing low is expensive in a way that is easy to miss: the
			// arena doubles as it grows, so a 4 KiB guess against a 16 KiB need allocates
			// 4+8+16+32 KiB per batch and the "arena saves allocations" change ends up
			// churning more bytes than the per-row copies it replaced.
			arenaHint, endsHint := scanChunkRows*256, scanChunkRows*2
			walk := makeWalk(p, stages, wk, st, mk, func(row T) bool {
				if arena != nil {
					batch.arena = arena.appendTo(batch.arena, row)
					batch.ends = append(batch.ends, len(batch.arena))
				} else {
					batch.rows = append(batch.rows, clone(row))
				}
				return true // a walker never stops on its own; the drainer decides
			})
			for chunk := range inbox[w] {
				if stopped.Load() {
					continue // drain the inbox so the dealer is never blocked on a dead lane
				}
				batch = &joinBatch[T]{scan: chunk.idx}
				if arena != nil {
					batch.arena = make([]byte, 0, arenaHint)
					batch.ends = make([]int, 0, endsHint)
				} else {
					batch.rows = make([]T, 0, endsHint)
				}
				for _, row := range chunk.rows {
					st.ScanRows++
					wk.rows[0] = row
					wk.scanRow()
					stopWalk := walk(0)
					batch.scanEnds = append(batch.scanEnds, batch.count(arena != nil))
					if stopWalk {
						break // this walker's own budget tripped; the drainer sees the error
					}
				}
				if n := cap(batch.arena); n > arenaHint {
					arenaHint = n // it grew: start the next one where this one ended up
				}
				if n := cap(batch.ends); n > endsHint {
					endsHint = n
				}
				select {
				case lanes[w] <- batch:
				case <-done:
				}
			}
		}(w)
	}

	// Drain in the dealing rotation. Lane w holds scan rows w, w+P, ... in order, so taking
	// lanes 0,1,..,P-1,0,... yields scan rows 0,1,2,... with no comparison anywhere.
	stop := false
	for t := 0; !stop; t++ {
		b, ok := <-lanes[t%P]
		if !ok {
			break // this walker has no more scan rows, so neither has any later one
		}
		// The rotation is the whole order guarantee, and it is an invariant rather than
		// something checked: lane t%P holds scan row t because the dealer put it there.
		// Checking it costs one comparison per scan row and turns a rotation that has
		// slipped into a loud failure instead of a silently reordered result -- which, on
		// a newest-first archive join, is condor_history coming back out of order.
		if b.scan != t {
			stats.Err = fmt.Errorf("join: internal error: lane %d yielded scan row %d, expected %d",
				t%P, b.scan, t)
			stop = true
			break
		}
		// Walked one SCAN ROW at a time, because the LIMIT is checked between scan rows --
		// that is where the serial walk checks it. It yields every row a scan row produced
		// and then stops, so a LIMIT of 1 on a scan row matching three returns three.
		// Stopping mid scan row would be defensible on its own (Limit says "stops after this
		// many joined rows") but it would make the parallel walk answer differently from the
		// serial one, and which walk ran is not something a caller chose.
		pos := 0
		for _, end := range b.scanEnds {
			for ; pos < end; pos++ {
				if stats.Rows >= p.limits.MaxRows {
					stats.Err = fmt.Errorf("the join produced more than %d rows; narrow it with a constraint",
						p.limits.MaxRows)
					stop = true
					break
				}
				stats.Rows++
				if !yield(b.at(arena, pos)) {
					stop = true
					break
				}
			}
			if stop {
				break
			}
			if p.limit > 0 && stats.Rows >= p.limit {
				stop = true
				break
			}
		}
	}
	if stop {
		stopped.Store(true)
		close(done)
		// Keep draining so a walker parked on a lane send is released even if it missed
		// the close; the lanes are closed by the walkers themselves on the way out.
		for i := range lanes {
			for range lanes[i] {
			}
		}
	}
	wg.Wait()
	for i := range walkerStats {
		mergeJoinStats(stats, &walkerStats[i])
	}
	// Merged first: a walker that tripped its own pair budget broke out of its chunk, so
	// the stream was not walked in full even though the drainer never stopped. Its error
	// does not reach stats until here.
	return true, !stop && stats.Err == nil
}

// count is how many rows this batch holds.
func (b *joinBatch[T]) count(arena bool) int {
	if arena {
		return len(b.ends)
	}
	return len(b.rows)
}

// at returns row i, slicing it out of the arena when there is one.
func (b *joinBatch[T]) at(a *rowArena[T], i int) T {
	if a == nil {
		return b.rows[i]
	}
	start := 0
	if i > 0 {
		start = b.ends[i-1]
	}
	return a.wrap(b.arena[start:b.ends[i]:b.ends[i]])
}

// scanChunk is a RUN of consecutive scanned rows and the chunk's position in the rotation.
//
// Rows are dealt in chunks rather than singly because the handoff, not the walking, was what
// the first version spent its time on: one inbox send, one lane send and one lane receive per
// scan row is ~60k channel operations on a 20k-row join, each a potential goroutine wakeup,
// against a pair loop that takes microseconds per row. Chunking divides all of that by
// scanChunkRows and changes nothing about the order -- chunk k still goes to walker k%P, the
// rows inside it are consecutive, and the drainer still takes lanes in the same rotation.
type scanChunk struct {
	rows []*joinRow
	idx  int // chunk number, not row number: the rotation is over chunks
}

// scanChunkRows is how many scan rows one chunk carries. Large enough that the handoff is
// amortized, small enough that a walker that draws a run of expensive rows cannot hold the
// drainer up for long, and that the first rows come out promptly for a LIMIT.
const scanChunkRows = 32

// tryAcquireJoin takes up to n tokens without blocking, returning how many it got.
func tryAcquireJoin(sem chan struct{}, n int) int {
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

// mergeJoinStats folds one walker's counters into the caller's. Rows is NOT merged: the
// drainer counts the rows it actually yielded, which is the number the caller asked about,
// and a walker's own count includes rows produced after an early stop.
func mergeJoinStats(dst, src *JoinStats) {
	dst.ScanRows += src.ScanRows
	dst.BandWindows += src.BandWindows
	dst.BandCandidates += src.BandCandidates
	dst.Pairs += src.Pairs
	if dst.Err == nil && src.Err != nil {
		dst.Err = src.Err
	}
}
