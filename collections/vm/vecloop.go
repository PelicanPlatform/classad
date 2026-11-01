package vm

// A LOOP OVER A BATCH.
//
// An If runs both arms once and rejoins. A loop cannot: how many times a record goes round is a
// property of that record, so a batch of 8192 may contain one record on its first trip and another
// on its hundredth. The executor runs the iterations in LOCKSTEP and lets the live set shrink --
// every trip evaluates the condition for the records still going, drops the ones it is now false
// for, and stops when none are left. A record that left on trip three simply was not live for trips
// four onward, so it is neither re-executed nor billed for them.
//
// That is why the live mask was worth building. For an If, predication mostly buys accounting; here
// it is the only thing that makes the construct expressible at all.
//
// THE LIVE SET AFTER THE LOOP is the set it was entered with, minus whatever faulted inside. Not the
// set that was still going at the end -- that one is empty by construction, which is how the loop
// knew to stop. Records that left the loop normally carry on after it.
//
// TERMINATION is the one thing a loop needs that nothing else here does, and the shape above makes
// it cheap. Because every live record is on the SAME trip, a single counter per loop is already a
// per-record trip count: the records still live after L trips are exactly the ones that have taken
// L of them. So the bound is one multiply per trip against the per-record budget, and no per-record
// state at all.
//
// WHAT HAPPENS WHEN THE BOUND IS HIT is a decline, not a fault. The batch falls back to the
// per-record scalar path, where each record gets its own budget and its own ERROR -- which is
// exactly what the scalar executor would have done for each of them anyway. Faulting the whole
// batch here would instead punish every record for one runaway.

// loopRegion is a While whose trips are in flight.
type loopRegion struct {
	header, end int32

	// outer is the live set the loop was entered with, BORROWED -- for a loop inside an If arm it is
	// that arm's own mask. The same ownership rule as everywhere else here: a region releases only
	// what it allocated.
	outer      []uint64
	outerAll   bool
	outerCount int

	// owned is this trip's live set, replaced each time round and released when the loop ends.
	owned []uint64

	// iterCost is the charged instructions in one trip, read off the prefix sum. It is an upper
	// bound -- a trip that skips a branch runs fewer -- which is the safe direction: the bound is
	// reached a little early and the batch declines to a path that is exact.
	iterCost int64
	trips    int64

	// chargeIn is the batch's charge when the loop was entered, so the loop's own charge is a
	// subtraction. See the work guard in stepLoop.
	chargeIn int64
}

// loopWasteFactor is how much work the vector path may do beyond what running the same records one
// at a time would have cost, before it gives up and lets the caller do exactly that.
//
// There has to be such a guard, and the trip bound alone is not one. A trip costs a full-WIDTH pass
// whatever the live count is -- the kernels do not compact -- so a loop that ten records are still
// going round costs the same per trip as one all 8192 are. The charge counts what the records
// actually needed; the width is what they cost. When those diverge far enough the batch is better
// off abandoned, and abandoning is free of risk because the per-record path is the exact one.
//
// Four, because giving up has to re-run the whole batch: a factor that is too eager pays for a
// re-run to avoid less waste than the re-run costs.
const loopWasteFactor = 4

// loopWasteFloor is how much work a loop must have done before the ratio above is worth consulting
// at all, in element-operations.
//
// A RATIO SAYS NOTHING ABOUT A SMALL NUMBER. On a batch of eight records a loop that one of them is
// still going round trips the ratio within a handful of trips, and correctly: the vector path really
// is doing eight times the necessary work. It is doing it to a total of a couple of hundred
// operations, so there is nothing to save and a re-run costs more than the waste. The floor is about
// eight full-width passes over the largest block the store produces, which is less than one block's
// decompression -- below that the whole loop is noise next to the work that fetched the data.
const loopWasteFloor = 8 * 8192

// atLoopHeader reports whether the innermost open loop is the one whose header is here.
func (s *VecScratch) atLoopHeader(header int32) bool {
	return len(s.loops) > 0 && s.loops[len(s.loops)-1].header == header
}

// stepLoop runs one arrival at a loop's condition: the first entry or another trip. It returns the
// instruction to continue at.
//
// ok=false declines the batch, which happens only when the trip bound is reached.
func (s *VecScratch) stepLoop(vops []int32, ct condTargets, ip int, perRecord int64) (int, bool) {
	if s.depth == 0 {
		return 0, false
	}
	if s.live == nil {
		s.initPredicates()
	}
	// The condition's own instructions were billed to whoever was still going round; settle that
	// before the live set changes.
	s.flushCharge(vops, ip+1)

	if !s.atLoopHeader(ct.header) {
		cost := int64(vops[ct.end] - vops[ct.header])
		if cost < 1 {
			// A loop whose every instruction is free would never reach the bound. The compiler cannot
			// emit one -- a condition is an expression and the smallest expression is charged -- but
			// a hand-built program can, and an unbounded loop is not a thing to leave reachable.
			cost = 1
		}
		s.loops = append(s.loops, loopRegion{
			header: ct.header, end: ct.end, iterCost: cost, chargeIn: s.charge,
			outer: s.live, outerAll: s.liveAll, outerCount: s.liveCount,
		})
	}
	r := &s.loops[len(s.loops)-1]
	r.trips++
	// Termination. Every live record is on this same trip, so this IS a per-record trip limit.
	if r.trips*r.iterCost > perRecord {
		return 0, false
	}
	// Worth-it. A trip costs full width however few records are still going, so a loop that has run
	// far past the point where most of them left is doing work nobody asked for.
	if width := r.trips * r.iterCost * int64(s.n); width > loopWasteFloor {
		if spent := s.charge - r.chargeIn; spent > 0 && width > loopWasteFactor*spent {
			return 0, false
		}
	}

	cond := s.pop()
	cond.toMask()
	cont := s.takeMask()
	var faulted bool
	for w := range cont {
		hi, lo, cur := cond.Hi[w], cond.Lo[w], s.live[w]
		cont[w] = cur & ^hi & lo // still TRUE: round again
		if bad := cur & hi; bad != 0 {
			s.faulted[w] |= bad
			faulted = true
		}
	}
	if faulted {
		s.anyFault = true
		for i := range s.preds {
			for w := range cont {
				s.preds[i].thenMask[w] &^= s.faulted[w]
				s.preds[i].elseMask[w] &^= s.faulted[w]
			}
		}
	}
	if maskEmpty(cont) {
		// Nobody is still going. Everything that entered carries on after the loop, except whatever
		// faulted on the way -- which is cleared here rather than tracked, because `faulted` is
		// monotone and so says it for every trip at once.
		s.releaseMask(cont)
		s.endLoop(r)
		s.spanStart = int(ct.end)
		return int(ct.end), true
	}
	s.releaseMask(r.owned)
	r.owned = cont
	s.live, s.liveAll, s.liveCount = cont, false, popcountMask(cont)
	s.spanStart = ip + 1
	return ip + 1, true
}

// endLoop restores the live set the loop was entered with, less anything that faulted inside it.
//
// It writes into the borrowed outer mask and hands it back as the live set, which is what the If
// join does at its own reconvergence point. The mask being redefined is the one that was live before
// the construct, so writing it is restoring it rather than clobbering somebody.
func (s *VecScratch) endLoop(r *loopRegion) {
	s.releaseMask(r.owned)
	if s.anyFault {
		for w := range r.outer {
			r.outer[w] &^= s.faulted[w]
		}
		s.live, s.liveAll, s.liveCount = r.outer, false, popcountMask(r.outer)
	} else {
		s.live, s.liveAll, s.liveCount = r.outer, r.outerAll, r.outerCount
	}
	s.loops = s.loops[:len(s.loops)-1]
}

// resetLoops returns every open loop's mask to the pool. Only `owned`, never `outer`, which belongs
// to whatever the loop was entered from.
func (s *VecScratch) resetLoops() {
	for i := range s.loops {
		s.releaseMask(s.loops[i].owned)
	}
	s.loops = s.loops[:0]
}
