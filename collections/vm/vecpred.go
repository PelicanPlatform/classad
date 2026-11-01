package vm

import "math/bits"

// PREDICATION: running a script's STATEMENTS over a batch, where different records take different
// branches.
//
// An expression vectorizes without any of this, because every record computes the same expression and
// the only per-record choice is which value a join selects (see blend). A statement is different: an
// If runs a block for some records and a different block for others, and a block has an EFFECT --
// it binds a local -- so "evaluate both and select" is not enough on its own. Both blocks still run,
// but each store has to leave the records that did not take that path exactly as they were.
//
// THE LIVE MASK is one bit per record: the records still executing at this point in the script. It
// starts all ones, narrows at a branch, and widens back at the join. A store writes only its live
// lanes. One plane, not two, because "executing" is a boolean -- unlike a VALUE, which is three-valued
// and needs the (hi,lo) pair of vecmask.go.
//
// THE FAULT PLANE is one bit per record and MONOTONE: once a record faults it is done for the rest of
// the script, and its answer is ERROR however the remaining statements turn out. A
// condition that is neither true nor false -- UNDEFINED, a string, a list -- faults the records it is
// neither for, and does not pick a branch for them.
//
// The two are not the same thing, and conflating them is the bug this layout exists to prevent.
// `live` is pushed and popped at region boundaries; `faulted` only grows. And a record that faults
// must be cleared from EVERY mask held by an enclosing region, not only from the current one:
// clearing only the current mask leaves the record dead inside the region and ALIVE AGAIN at the
// reconvergence point, because the join's live mask is rebuilt from the arm masks the enclosing
// region saved before the fault happened. That is a wrong answer that looks entirely reasonable.
//
// WHAT THE FAULT PLANE MAKES UNOBSERVABLE, which is worth writing down because it is the reason
// several of the rules above cannot be tested from the outside today. Because a faulted record's
// answer is overwritten with ERROR at the end whatever the statements in between did, EXCLUDING a
// faulted record from an arm mask changes no result: it could take both arms, or the wrong one, and
// still answer ERROR. The same goes for clearing it from the enclosing masks. Those rules are kept
// anyway, for two reasons that are not visible yet. They stop the executor doing work for records
// that are finished, which is the point of predication; and they become load-bearing the moment
// anything consults the live mask for something other than a store -- an early exit when no record
// is live, or compaction. Those are also the only things that make predication FASTER than running
// every record through every arm, so the masks will be read for more than a store before long. A rule
// that is merely redundant today is cheaper to keep than to rediscover.
//
// WHAT IS NOT HERE. A loop. `While` has a back edge, so its live mask narrows across iterations and
// the loop runs until no record is still true -- which needs the per-record trip accounting that the
// element-step budget has not been built for yet. The executor declines a back edge rather than
// guessing, and TestVecScriptDeclinesControlFlow pins that.

// The arm of an If currently executing.
const (
	armThen uint8 = iota
	armElse
)

// predPending is one If whose arms are being executed, innermost last.
//
// thenMask and elseMask are computed together at the branch and kept until the join, because the live
// mask after the If is their union: a record is executing again if it was executing on either path.
// Recomputing it from the condition at the join would be wrong as well as wasteful -- a record that
// faulted inside an arm has been cleared from these, and the condition no longer knows that.
type predPending struct {
	elseAt, end int32
	arm         uint8
	thenMask    []uint64
	elseMask    []uint64

	// outer is the live mask the If was entered with, BORROWED rather than owned: for a nested If it
	// is the enclosing region's own arm mask, so releasing it here would hand a mask the enclosing
	// region is still holding back to the pool, where the next takeMask zeroes it. The join writes the
	// union of the arms back into it and makes it live again, and the two arm masks -- which this
	// region does own -- are the ones released.
	outer []uint64
}

// takeMask returns a zeroed mask from the pool.
func (s *VecScratch) takeMask() []uint64 {
	w := maskWords(s.n)
	if n := len(s.maskPool); n > 0 {
		m := s.maskPool[n-1]
		s.maskPool = s.maskPool[:n-1]
		if cap(m) >= w {
			m = m[:w]
			for i := range m {
				m[i] = 0
			}
			return m
		}
	}
	return make([]uint64, w)
}

func (s *VecScratch) releaseMask(m []uint64) {
	if m != nil {
		s.maskPool = append(s.maskPool, m)
	}
}

// resetPredicates returns every mask in flight to the pool and leaves the scratch unpredicated.
//
// Called on every reset, which matters because a program can DECLINE part-way through -- a back edge,
// an opcode this executor does not serve -- and leave arms open. The next batch must not inherit them.
//
// It allocates and fills NOTHING, because it runs in front of every block of every columnar scan and
// almost none of them are scripts. Setting up the planes here cost ~2% of an expression scan, which
// is a poor price for state an expression cannot use. initPredicates does that, and only a program
// that actually has statements calls it.
func (s *VecScratch) resetPredicates() {
	// EVERY MASK HAS EXACTLY ONE OWNER, and releasing one twice is the bug this spells out to avoid:
	// the pool would then hand the same backing array to two masks that are supposed to be
	// different, and the symptom is wrong answers in a LATER batch.
	//
	// So each region releases only what it allocated -- its arm masks -- and never its `outer`,
	// which is always somebody else's: an enclosing region's mask, or baseLive. s.live is not
	// released here at all, because while any region is open it ALIASES that region's mask. baseLive
	// is the one this function owns.
	for i := range s.preds {
		s.releaseMask(s.preds[i].thenMask)
		s.releaseMask(s.preds[i].elseMask)
	}
	s.preds = s.preds[:0]
	s.releaseMask(s.faulted)
	s.releaseMask(s.baseLive)
	s.live, s.baseLive, s.faulted = nil, nil, nil
	s.liveAll, s.anyFault = true, false
}

// initPredicates gives the batch its live and fault planes, for a program that has statements.
func (s *VecScratch) initPredicates() {
	s.baseLive, s.faulted = s.takeMask(), s.takeMask()
	fillLive(s.baseLive, s.n)
	s.live = s.baseLive
	s.liveAll, s.anyFault = true, false
}

// fillLive sets the first n bits and clears the rest, so a word's tail beyond the batch never counts
// as a live record.
func fillLive(m []uint64, n int) {
	for i := range m {
		m[i] = ^uint64(0)
	}
	if rem := n % 64; rem != 0 && len(m) > 0 {
		m[len(m)-1] = (uint64(1) << uint(rem)) - 1
	}
}

func maskEmpty(m []uint64) bool {
	for _, w := range m {
		if w != 0 {
			return false
		}
	}
	return true
}

// enterIf splits the live mask on a condition and begins the then arm, returning the instruction to
// continue at.
//
// The condition is converted to mask form, where TRUE is 01, FALSE is 00 and the hi plane marks
// exactly the states that are neither -- UNDEFINED and ERROR, which this treats alike because the
// script's answer is the same either way: it could not tell which branch to take.
func (s *VecScratch) enterIf(vops []int32, ct condTargets, at int) (int, bool) {
	if s.depth == 0 {
		return 0, false
	}
	if s.live == nil {
		// hasStatements said this program had none, and it does. That flag is an optimization -- it
		// keeps the planes off the expression path -- so getting it wrong must cost a late setup and
		// not a nil mask, which would be a panic inside a daemon rather than a wrong answer.
		s.initPredicates()
	}
	// The condition was evaluated for whoever reached it; settle that before the arms narrow it.
	s.flushCharge(vops, at+1)
	cond := s.pop()
	cond.toMask()
	thenM, elseM := s.takeMask(), s.takeMask()
	var faulted bool
	for w := range thenM {
		hi, lo, cur := cond.Hi[w], cond.Lo[w], s.live[w]
		thenM[w] = cur & ^hi & lo
		elseM[w] = cur & ^hi & ^lo
		if bad := cur & hi; bad != 0 {
			s.faulted[w] |= bad
			faulted = true
		}
	}
	if faulted {
		s.anyFault = true
		// Out of every enclosing arm mask too, not just this one. See the file comment: a record left
		// in an enclosing thenMask comes back alive at that region's join.
		for i := range s.preds {
			for w := range thenM {
				s.preds[i].thenMask[w] &^= s.faulted[w]
				s.preds[i].elseMask[w] &^= s.faulted[w]
			}
		}
	}
	s.preds = append(s.preds, predPending{
		elseAt: ct.onFalse, end: ct.end, arm: armThen,
		thenMask: thenM, elseMask: elseM, outer: s.live,
	})
	s.live, s.liveAll = thenM, false
	s.liveCount = popcountMask(thenM)
	s.spanStart = at + 1
	return at + 1, true
}

// jmpVoid resolves a statement-level jump: the end of an arm, or the end of the If.
//
// It is driven entirely by the pending region rather than by the jump's target, because the two arms
// of an If jump to the SAME place and only the region knows which of them just finished. A jump that
// does not match the innermost region is a back edge or a shape this executor was not written for,
// and it declines.
func (s *VecScratch) jmpVoid(vops []int32, at int, target int32) (int, bool) {
	if len(s.preds) == 0 {
		return 0, false
	}
	pd := &s.preds[len(s.preds)-1]
	if pd.end != target {
		return 0, false
	}
	// Each arm is billed to the records that took it, so the run ends here whichever arm just did.
	s.flushCharge(vops, at+1)
	if pd.arm == armThen {
		pd.arm = armElse
		s.live, s.liveCount = pd.elseMask, popcountMask(pd.elseMask)
		s.spanStart = int(pd.elseAt)
		return int(pd.elseAt), true
	}
	// The join. A record is executing again if it was executing on either path, which is NOT the same
	// as the mask the If was entered with: a record that faulted inside an arm has been cleared from
	// both and must stay cleared. The union goes back into the borrowed outer mask, which is what the
	// enclosing region -- if there is one -- is still holding.
	for w := range pd.outer {
		pd.outer[w] = pd.thenMask[w] | pd.elseMask[w]
	}
	s.live = pd.outer
	s.liveCount = popcountMask(pd.outer)
	s.releaseMask(pd.thenMask)
	s.releaseMask(pd.elseMask)
	s.preds = s.preds[:len(s.preds)-1]
	if len(s.preds) == 0 && !s.anyFault {
		// Back to the whole batch, so stores take the unmasked path again.
		s.liveAll, s.liveCount = true, s.n
	}
	s.spanStart = int(target)
	return int(target), true
}

// maskedStore writes the top of the stack into a local at the LIVE lanes only, leaving the others as
// they were. The vector form of a store under a predicate.
//
// IT FORCES THE DESTINATION OUT OF ITS COMPRESSED FORMS, but only the ones that cannot describe the
// lanes separately. A Const vector says one thing about every record and a Raw vector is a column at
// one stored width, so neither can absorb a value that differs per record; both are expanded.
//
// A DICTIONARY IS NOT ONE OF THOSE, which is worth stating because the obvious rule -- "a masked
// store materializes its destination" -- is wrong here and costs a string per record. A dictionary
// describes a vector's codes individually, so a destination can keep its own codes while this writes
// other values into other lanes; the mixed result is a shape the executor already reads. What cannot
// survive is a code arriving from a DIFFERENT dictionary, because the two number their entries
// independently, so a foreign code is resolved to its string as it lands. Same rule as blend, and the
// same reason: resolution is the correctness mechanism, and carrying a dictionary is the optimization.
func (s *VecScratch) maskedStore(idx int) bool {
	dst := s.locals[idx]
	if dst == nil || dst.n != s.n {
		dst = newVec(s.n)
		s.locals[idx] = dst
		s.localsSet[idx] = false
	}
	if !s.localsSet[idx] {
		// Lanes this store does not write have never been bound, and an unbound local is UNDEFINED.
		for i := 0; i < dst.n; i++ {
			dst.St[i] = VsUndef
		}
		dst.Mask, dst.Const, dst.Raw, dst.Dict = false, false, nil, nil
		s.localsSet[idx] = true
	}
	dst.toData()
	dst.materialize()
	src := s.pop()
	src.ensureInts()
	src.toData()
	// Whether the source's codes can land AS codes. Only when the destination carries no dictionary
	// of its own, in which case it adopts the source's and nothing has to be resolved; a destination
	// that already has one keeps it, and the incoming codes are resolved instead.
	//
	// The rule is "has one" and not "has the same one" on purpose: comparing two DictResolver
	// interface values would panic if their shared dynamic type were ever non-comparable. The cost is
	// that a source and a destination holding the SAME dictionary resolve anyway -- slower, never
	// wrong -- which is the trade blendDictSource makes for the same reason.
	adopt := src.Dict != nil && dst.Dict == nil
	if adopt {
		dst.Dict = src.Dict
	}
	for w, word := range s.live {
		for word != 0 {
			b := word & -word
			word ^= b
			i := w*64 + bits.TrailingZeros64(b)
			if i >= dst.n {
				break
			}
			if !dst.CopyElem(i, src, i) {
				return false
			}
			if dst.St[i] == VsDictCode && !adopt {
				// A code the destination's own dictionary does not number. It is meaningless here, so
				// it lands as the string it stands for.
				if !dst.setValueData(i, src.valueData(i)) {
					return false
				}
			}
		}
	}
	return true
}

// applyFaults forces every faulted record's result to ERROR, which is the script's answer for a
// record whose condition it could not evaluate.
//
// Applied once at the end rather than as each fault happens: the statements after the fault still run
// for the other records, and the faulted record's own result is whatever they left behind, which is
// not an answer.
func (s *VecScratch) applyFaults(res *Vec) bool {
	if !s.anyFault || res == nil {
		return true
	}
	res.toData()
	res.ensureInts()
	res.materialize()
	for w, word := range s.faulted {
		for word != 0 {
			b := word & -word
			word ^= b
			i := w*64 + bits.TrailingZeros64(b)
			if i >= res.n {
				break
			}
			res.St[i] = VsError
		}
	}
	return true
}
