package vm

import (
	"math/bits"

	"github.com/PelicanPlatform/classad/classad"
)

// SKIPPING A PATH NO RECORD TAKES.
//
// The scalar executors jump over work a record does not need: `A > 4 && B < 4` never evaluates the
// right operand for a record whose left is false, and an Elvis never evaluates its fallback for a
// record whose left is defined. The vector executor could not, because a jump is all-or-nothing over
// a batch and some records usually need the work.
//
// Some, but not always any. A block where `A > 4` is false for every record needs the right operand
// for none of them, and that is not a rare shape -- it is what a selective constraint looks like
// over sorted or clustered data. So the executor narrows the live set to the records that still need
// the path, and where that set is EMPTY it takes the jump after all.
//
// The narrowing is what makes the charge right as well. A record is billed for the instructions it
// would have executed scalar, which is the property that lets the two be compared per record; a path
// evaluated for everyone and billed to everyone would make the vector charge depend on who else was
// in the batch.
//
// A LEFT OPERAND IS ALREADY ITS OWN SHORT-CIRCUIT RESULT, which is why skipping is exact rather than
// approximate. For `&&` the decisive states are FALSE and ERROR, and `FALSE && x` is FALSE while
// `ERROR && x` is ERROR -- the left value unchanged. Same for `||` with TRUE and ERROR. So when no
// record needs the right operand the executor jumps past the operand AND the combine, leaving the
// left vector on the stack as the result, which is exactly what the scalar interpreter does.

// pathRegion is a stretch of instructions that only some records execute.
//
// saved is BORROWED: it is whatever was live when the region opened, which for a nested region is
// the enclosing region's own mask. Releasing it here would hand the pool a mask something else is
// still holding -- the same hazard the predication regions have, and the same answer. owned is the
// narrowed mask this region allocated, and is the only one it returns.
type pathRegion struct {
	end        int32
	saved      []uint64
	savedAll   bool
	savedCount int
	owned      []uint64
}

// needRightPlanes writes, into dst, the records whose right operand the operator still needs.
//
// Read off the mask encoding (hi lo = 00 FALSE, 01 TRUE, 10 UNDEFINED, 11 ERROR) rather than
// branched per record:
//
//	&&  is decided by FALSE (00) and ERROR (11), so it still needs TRUE and UNDEFINED: hi^lo
//	||  is decided by TRUE (01) and ERROR (11), so it still needs FALSE and UNDEFINED: ^lo
//
// UNDEFINED is on the needs-it side of both, which is the part worth checking against the
// evaluator rather than assuming: `UNDEF && FALSE` is FALSE and `UNDEF || TRUE` is TRUE, so an
// undefined left operand does not decide anything by itself.
func needRightPlanes(op Opcode, left *Vec, dst []uint64, n int) {
	for w := range dst {
		hi, lo := left.Hi[w], left.Lo[w]
		if op == OpShortAnd {
			dst[w] = hi ^ lo
		} else {
			dst[w] = ^lo
		}
	}
	clearTail(dst, n)
}

// undefinedPlanes writes, into dst, the records whose value is UNDEFINED.
//
// This is NOT the same question as the mask form's truthiness, which is why it is written out rather
// than taken from toMask: a string is ERROR as a logical operand and is perfectly well defined as a
// value, and an Elvis asks the second question. A vector in RAW form has no undefined element by
// construction -- widening it assigns every element one state -- so that case is a no-op rather than
// an omission.
func undefinedPlanes(v *Vec, dst []uint64, n int) {
	for w := range dst {
		dst[w] = 0
	}
	switch {
	case v.Mask:
		for w := range dst {
			dst[w] = v.Hi[w] &^ v.Lo[w] // UNDEFINED is 10
		}
	case v.Raw != nil:
		return
	case v.Const:
		if v.St[0] == VsUndef {
			for w := range dst {
				dst[w] = ^uint64(0)
			}
		}
	default:
		for i := 0; i < n; i++ {
			if v.St[i] == VsUndef {
				dst[i/64] |= 1 << uint(i%64)
			}
		}
	}
	clearTail(dst, n)
}

// clearTail zeroes the bits past n, which otherwise count records that do not exist. `^lo` sets them
// all, so this is not a tidiness measure.
func clearTail(m []uint64, n int) {
	if tail := uint(n % 64); tail != 0 && len(m) > 0 {
		m[len(m)-1] &= (uint64(1) << tail) - 1
	}
}

func popcountMask(m []uint64) int {
	n := 0
	for _, w := range m {
		n += bits.OnesCount64(w)
	}
	return n
}

// flushCharge bills everything executed since the last flush, at the live count that was in force
// for it, and starts a new run at cur.
//
// Called where a run ENDS -- a jump, a region opening or closing, the end of the program -- which is
// also where the live count can change. Between those points nothing needs counting, which is the
// whole point: the dispatch loop does no accounting at all.
func (s *VecScratch) flushCharge(prefix []int32, cur int) {
	if n := prefix[cur] - prefix[s.spanStart]; n > 0 {
		s.charge += int64(n) * int64(s.liveCount)
	}
	s.spanStart = cur
}

// intersectLive narrows mask in place to the records that are also live now.
func (s *VecScratch) intersectLive(mask []uint64) {
	if s.liveAll {
		return
	}
	for w := range mask {
		mask[w] &= s.live[w]
	}
}

// condStatePlanes splits the records by the condition state of a conditional's selector.
//
// It asks classad.ConditionState per element, which is the SAME call the join makes when it picks a
// record's value. That is not a convenience: if these masks disagreed with the join about which path
// a record took, the record would be billed for one path and given the value of another, and the two
// would never be compared because nothing computes both.
func condStatePlanes(sel *Vec, masks [4][]uint64, n int) {
	for k := range masks {
		for w := range masks[k] {
			masks[k][w] = 0
		}
	}
	for i := 0; i < n; i++ {
		var k int
		switch classad.ConditionState(sel.value(i)) {
		case classad.CondTrue:
			k = 0
		case classad.CondFalse:
			k = 1
		case classad.CondUndef:
			k = 2
		default:
			k = 3
		}
		masks[k][i/64] |= 1 << uint(i%64)
	}
}

// openPath makes mask the live set until ip reaches end. It takes ownership of mask.
func (s *VecScratch) openPath(prefix []int32, at int, end int32, mask []uint64) {
	s.openPathAs(prefix, at, end, mask, mask)
}

// openPathBorrowed is openPath for a mask somebody else owns -- a conditional's four path masks
// belong to the pending join, which outlives each of them.
func (s *VecScratch) openPathBorrowed(prefix []int32, at int, end int32, mask []uint64) {
	s.openPathAs(prefix, at, end, mask, nil)
}

func (s *VecScratch) openPathAs(prefix []int32, at int, end int32, mask, owned []uint64) {
	s.flushCharge(prefix, at)
	s.paths = append(s.paths, pathRegion{
		end: end, saved: s.live, savedAll: s.liveAll, savedCount: s.liveCount, owned: owned,
	})
	s.live, s.liveAll = mask, false
	s.liveCount = popcountMask(mask)
}

// endPaths closes every region converging on ip, innermost first.
func (s *VecScratch) endPaths(prefix []int32, ip int) {
	for len(s.paths) > 0 && s.paths[len(s.paths)-1].end == int32(ip) {
		s.flushCharge(prefix, ip)
		r := s.paths[len(s.paths)-1]
		s.paths = s.paths[:len(s.paths)-1]
		s.live, s.liveAll, s.liveCount = r.saved, r.savedAll, r.savedCount
		s.releaseMask(r.owned)
	}
}

// condPathAt opens the conditional path that starts at ip, if one does.
//
// A path no record takes is not run at all: a placeholder is pushed so the join still finds one value
// per path, and control jumps to the next path's start. The placeholder is never selected, because
// the join picks by the same condition state that made this mask empty.
//
// Returns the instruction to jump to, if the path was skipped.
func (s *VecScratch) condPathAt(prefix []int32, ip int) (int, bool) {
	if len(s.blends) == 0 {
		return 0, false
	}
	b := &s.blends[len(s.blends)-1]
	if !b.cond || b.nextPath >= 4 || int(b.bounds[b.nextPath]) != ip {
		return 0, false
	}
	k := b.nextPath
	b.nextPath++
	if maskEmpty(b.masks[k]) {
		fillState(s.push(), VsUndef)
		// Bill everything up to here first, at the count that was in force for it, and only then
		// move the run past the skipped path -- which itself bills nobody.
		s.flushCharge(prefix, ip)
		s.spanStart = int(b.bounds[k+1])
		return int(b.bounds[k+1]), true
	}
	s.openPathBorrowed(prefix, ip, b.bounds[k+1], b.masks[k])
	return 0, false
}

// releaseCondMasks returns a resolved conditional's path masks to the pool.
func (s *VecScratch) releaseCondMasks(b *blendPending) {
	for k := range b.masks {
		s.releaseMask(b.masks[k])
		b.masks[k] = nil
	}
}

// resetPaths returns every region's mask to the pool and starts the batch whole.
//
// Each owned mask exactly once, and never a saved one: a saved mask is either an enclosing region's
// owned mask, which that region releases, or a predication arm mask, which resetPredicates releases.
func (s *VecScratch) resetPaths() {
	for i := range s.paths {
		s.releaseMask(s.paths[i].owned)
	}
	s.paths = s.paths[:0]
	s.charge, s.liveCount, s.spanStart = 0, s.n, 0
}
