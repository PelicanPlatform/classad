package collections

import (
	"sort"
	"strings"
)

// Retention rotation for an append-only collection: Rotate drops whole oldest
// segments once the collection exceeds a configured bound. This is the archive's
// aging-out expressed as a Collection rule -- there is no compaction here, only
// whole-segment reclamation, so the append log's newest-first order and per-record
// identity are untouched. The Retention type is shared with the archive (archive.go).

// Rotate drops whole oldest segments until the append-only collection is back within
// its Retention bounds, returning the number of segments dropped. now is the caller's
// wall clock (unix seconds) for age-based retention -- the store keeps no clock of its
// own -- and is ignored unless Retention.MaxAgeAttr/MaxAge are set. Rotate is a no-op
// (returns 0) on a non-append-only collection or when Retention is the zero value.
//
// The segment still being appended to is never dropped, so Rotate can bound a growing
// archive without racing the writer. A dropped segment's file is unmapped and unlinked
// once no in-flight scan still pins it (a scan that started earlier keeps reading a
// consistent snapshot until it finishes; the reclaim is deferred to its last unpin).
//
// Retention honors MaxSegments, MaxBytes, and MaxAgeAttr/MaxAge (drop a segment whose
// newest value of the age attribute is older than now-MaxAge); MaxAge requires the age
// attribute to be a configured ZoneAttr so its per-segment max is available.
func (c *Collection) Rotate(now float64) (int, error) { return c.rotate(now, false) }

// EvictOverHighMark frees space when the store is over its MaxBytes ceiling, down to
// TargetBytes, applying only the SIZE bounds -- no age ceiling, no GC floor.
//
// It exists because Rotate runs on the maintenance tick and nothing bounds growth between
// ticks: a spoke replaying a backlog can overshoot MaxBytes by however much arrives in one
// interval, which is the case the ceiling is there to prevent. This is called at segment
// seal, where the cost in the steady state is a counter and a comparison.
//
// It takes no clock deliberately. The age rules need one and the store keeps none of its
// own (Rotate's now is the caller's), while the size rules need nothing but bytes -- so the
// one pass that has to run without a caller is the one that can.
func (c *Collection) EvictOverHighMark() (int, error) {
	n, err := c.rotate(0, true)
	if n > 0 {
		c.highMarkPasses.Add(1)
		c.highMarkDropped.Add(int64(n))
	}
	return n, err
}

// HighMarkStats reports how many seal-time size evictions have actually dropped something
// and how many segments they dropped in total. A pass count that tracks the seal count means
// the archive is sitting ON its ceiling and evicting continuously -- which is what
// Retention.TargetBytes exists to prevent, by freeing down to a low mark instead of to just
// under the ceiling.
func (c *Collection) HighMarkStats() (passes, dropped int64) {
	return c.highMarkPasses.Load(), c.highMarkDropped.Load()
}

// rotate is Rotate's body. sizeOnly restricts it to the MaxSegments/MaxBytes ceilings,
// leaving MaxAge and the GC floor to a pass that has a clock.
func (c *Collection) rotate(now float64, sizeOnly bool) (int, error) {
	if !c.appendOnly() {
		return 0, nil
	}
	// Serialize against other whole-collection maintenance (retrain/rewrite) and against
	// SetRetention, matching Compact. Commits and queries never take maintMu. c.ret is read
	// only under maintMu, so the retention check moves inside the lock.
	c.maintMu.Lock()
	defer c.maintMu.Unlock()
	if c.ret == (Retention{}) {
		return 0, nil // no bounds configured: keep everything
	}
	if sizeOnly && c.ret.MaxBytes <= 0 && c.ret.MaxSegments <= 0 {
		return 0, nil // nothing a size-only pass can enforce
	}
	mp := c.beginMaint(MaintRotate, 0)
	defer mp.finish()

	sh := c.shards[0] // AppendOnly forces a single shard
	// Resolve the two independent age attributes (via the shared intern table): MaxAgeAttr for
	// the MaxAge ceiling, MinAgeAttr for the MinAge floor and the GC-floor drain. Each is
	// resolved only when its bound is active.
	gcFloor := c.gcFloor
	if sizeOnly {
		gcFloor = 0 // the GC floor is an age rule; it needs the clock this pass does not have
	}
	var maxAgeID, minAgeID uint32
	var maxAgeOK, minAgeOK bool
	if c.ret.MaxAgeAttr != "" && c.ret.MaxAge > 0 && !sizeOnly {
		maxAgeID, maxAgeOK = c.intern.LookupID(c.ret.MaxAgeAttr)
	}
	if c.ret.MinAgeAttr != "" && gcFloor > 0 && !sizeOnly {
		minAgeID, minAgeOK = c.intern.LookupID(c.ret.MinAgeAttr)
	}
	var toReap []*segment
	dropped := 0

	// Value-ordered eviction runs before the positional rule, in three steps of rising cost.
	// Each only runs if the one before it did not get the store under its target.
	//
	//  1. Segments ENTIRELY below the cutoff: unlinked, nothing rewritten, no record above
	//     the cutoff lost.
	//  2. Segments STRADDLING it: rewritten without the part below. This costs the bytes the
	//     segment keeps, which is why it is not first -- but it is the only step that can
	//     free space from a segment holding data on both sides, and with records interleaved
	//     (a hub tailing several spokes) every segment is that shape.
	//  3. Whole segments ordered by the newest value each holds: a last resort that DOES
	//     discard data above the cutoff, taken only when the budget or the records ran out
	//     before the target did.
	//
	// Anything this cannot account for falls through to the positional loop, which is also
	// what reclaims segments it cannot see (unprofiled, or holding unbounded values).
	outOfOrderFloor := uint64(0)
	evictAttr := c.ret.evictAttr()
	// Free down to the low mark, not to just under the ceiling, so this runs rarely instead
	// of on every pass that crosses the line.
	target := c.ret.TargetBytes
	if target <= 0 || target > c.ret.MaxBytes {
		target = c.ret.MaxBytes
	}
	if evictAttr != "" && c.ret.MaxBytes > 0 {
		var trim []*segment
		var cutoff float64
		// The ceiling is what TRIGGERS a pass; the target is how far it then frees. Deciding
		// this once, here, is what makes the gap between them hysteresis: without it the
		// steps below would fire on every seal that left the store above the target, which is
		// most of them, and each would free one segment -- the churn the gap exists to stop.
		sh.mu.Lock()
		over := sealedBytesLocked(sh) > c.ret.MaxBytes
		if over {
			plan := planEviction(sh.segs, sh.act, sealedBytesLocked(sh)-target, evictAttr)
			for _, idx := range plan.whole {
				if sealedBytesLocked(sh) <= target {
					break
				}
				seg := sh.segs[idx]
				if seg == nil || seg == sh.act {
					continue
				}
				sh.dropSegmentLocked(idx, &toReap)
				dropped++
				outOfOrderFloor = max(outOfOrderFloor, seg.maxSeq)
			}
			if sealedBytesLocked(sh) > target && plan.cutoff != 0 {
				cutoff = plan.cutoff
				for _, idx := range plan.partial {
					if s := sh.segs[idx]; s != nil && s != sh.act {
						trim = append(trim, s)
					}
				}
			}
		}
		sh.mu.Unlock()

		// Step 2, outside the lock: each trim's swap takes it.
		if over {
			dropped += c.trimStraddlers(sh, trim, evictAttr, cutoff, target)
		}

		// Step 3.
		sh.mu.Lock()
		for _, idx := range segsByValueAsc(sh, evictAttr) {
			if !over {
				break
			}
			if sealedBytesLocked(sh) <= target {
				break
			}
			seg := sh.segs[idx]
			if seg == nil || seg == sh.act {
				continue
			}
			sh.dropSegmentLocked(idx, &toReap)
			dropped++
			outOfOrderFloor = max(outOfOrderFloor, seg.maxSeq)
		}
		sh.mu.Unlock()
	}

	sh.mu.Lock()
	for {
		idx := oldestLiveSeg(sh)
		if idx < 0 || sh.segs[idx] == sh.act {
			break // nothing left, or only the active (still-appended) segment remains
		}
		if !sh.overRetention(c.ret, idx, now, maxAgeID, maxAgeOK, minAgeID, minAgeOK, gcFloor) {
			break
		}
		sh.dropSegmentLocked(idx, &toReap)
		dropped++
	}
	if dropped > 0 {
		// A watch cursor below the new oldest record has missed rotated-out history. Raised
		// in the same critical section that drops the segments, so a catch-up that reads the
		// floor (appendCatchupView) never sees the drop without it.
		//
		// droppedFloorLocked answers for a PREFIX drop -- it is the oldest surviving record's
		// seq, which is correct only while everything below it is gone. Value-ordered eviction
		// drops holes, and a hole sits above that floor, so its own bound is folded in here.
		sh.appendFloor = max(sh.appendFloor, sh.droppedFloorLocked(), outOfOrderFloor)
	}
	sh.mu.Unlock()

	var err error
	for _, seg := range toReap {
		if e := seg.reapAndHook(); e != nil && err == nil { // munmap + unlink outside the lock
			err = e
		}
	}
	return dropped, err
}

// trimStraddlers rewrites the segments that hold data on both sides of the cutoff, most
// freeable first, until the shard is back under its target or the pass's rewrite budget is
// spent. Returns how many segments it rewrote.
//
// Not called with the shard lock held: each trim's swap takes it.
func (c *Collection) trimStraddlers(sh *shard, trim []*segment, attr string, cutoff float64, target int64) int {
	if len(trim) == 0 || attr == "" || target <= 0 {
		return 0
	}

	n, spent := 0, int64(0)
	for _, seg := range trim {
		sh.mu.RLock()
		over := sealedBytesLocked(sh) > target
		sh.mu.RUnlock()
		if !over || spent >= trimBudgetBytes {
			break
		}
		// A trim costs the bytes it KEEPS, which is the whole segment's worth of read and
		// re-encode whatever share of it goes away.
		cost := int64(seg.used)
		was := seg.maxSeq
		filtered, ok := c.trimSegmentToCutoff(sh, seg, attr, cutoff)
		if !ok {
			continue
		}
		spent += cost
		n++
		sh.mu.Lock()
		if filtered <= sh.count {
			sh.count -= filtered
		} else {
			sh.count = 0
		}
		// Records vanished from the middle of the log, so a cursor below this segment's
		// newest record can no longer be resumed: it would read past the gap and report the
		// stream as contiguous. Same bound as an out-of-order drop.
		sh.appendFloor = max(sh.appendFloor, was)
		sh.mu.Unlock()
	}
	return n
}

// SetRetention updates the retention bounds at runtime (an append-only collection). The
// next Rotate enforces them. Serialized against Rotate/maintenance via maintMu, so c.ret is
// only ever read or written under that lock.
func (c *Collection) SetRetention(r Retention) {
	c.maintMu.Lock()
	c.ret = r
	c.maintMu.Unlock()
	// Future seals profile the new axis. Segments already sealed keep the profile they were
	// sealed with; one built over a different attribute is rejected on read rather than
	// misread, so they simply stop contributing to value-ordered eviction until rewritten.
	for _, sh := range c.shards {
		sh.setProfileAttr(r.evictAttr())
	}
}

// SetGCFloor installs a runtime GC watermark, in Retention.MinAgeAttr units, that lets Rotate
// reclaim already-consumed records EARLY: a segment whose newest MinAgeAttr value is below
// floor may be dropped before it reaches MaxAge. It is how a change-feed source drains records
// every live subscriber has acknowledged (floor is the feed's GC floor, i.e. the min ack over
// live subscribers). It only ever shortens retention -- it can never keep data past the
// configured ceilings (a slow or absent subscriber holds a low floor, so its data simply ages
// out under MaxAge), and it never drops anything younger than Retention.MinAge. Requires
// MinAgeAttr set (and zone-mapped). Passing floor <= 0 clears it. Not persisted -- callers
// re-assert it each pass from the current live floor; a stale saved value must never GC data
// across a restart.
func (c *Collection) SetGCFloor(floor float64) {
	c.maintMu.Lock()
	if floor < 0 {
		floor = 0
	}
	c.gcFloor = floor
	c.maintMu.Unlock()
}

// GCFloor returns the current runtime GC watermark (0 when unset).
func (c *Collection) GCFloor() float64 {
	c.maintMu.Lock()
	defer c.maintMu.Unlock()
	return c.gcFloor
}

// Retention returns the current retention bounds.
func (c *Collection) Retention() Retention {
	c.maintMu.Lock()
	defer c.maintMu.Unlock()
	return c.ret
}

// segsByValueAsc lists the indices of the segments eligible for value-ordered eviction,
// ordered by the NEWEST value each one holds, ascending -- so the segment whose newest
// record is oldest comes first. The active segment is never listed (it is still being
// appended to), nor is one whose profile does not cover attr, nor one holding bytes whose
// value could not be bounded: for those the newest value is not known, and guessing it
// would evict on a number nobody computed. Caller holds sh.mu.
func segsByValueAsc(sh *shard, attr string) []int {
	type cand struct {
		idx int
		max float64
	}
	var cs []cand
	for i, seg := range sh.segs {
		if seg == nil || seg == sh.act || seg.profile == nil {
			continue
		}
		p := seg.profile
		if !p.haveValues || p.unknownBytes > 0 || !strings.EqualFold(p.attr, attr) {
			continue
		}
		cs = append(cs, cand{idx: i, max: p.valueMax})
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].max < cs[j].max })
	out := make([]int, len(cs))
	for i, c := range cs {
		out[i] = c.idx
	}
	return out
}

// dropSegmentLocked retires the segment at idx, adjusting the shard's record counts and
// leaving the slot nil so seg.id still equals its index (the segAt invariant). A segment
// that no scan still pins is appended to toReap for unmapping outside the lock; otherwise
// the last unpin reaps it. Caller holds sh.mu.
func (sh *shard) dropSegmentLocked(idx int, toReap *[]*segment) {
	seg := sh.segs[idx]
	if seg == nil {
		return
	}
	n, nsys := segLiveCount(seg)
	if n <= sh.count {
		sh.count -= n
	} else {
		sh.count = 0
	}
	if nsys <= sh.sysCount {
		sh.sysCount -= nsys
	} else {
		sh.sysCount = 0
	}
	if seg.retire() {
		*toReap = append(*toReap, seg)
	}
	sh.segs[idx] = nil
}

// sealedBytesLocked is the stored size of every live segment, which is what MaxBytes bounds.
// Caller holds sh.mu.
func sealedBytesLocked(sh *shard) int64 {
	var total int64
	for _, seg := range sh.segs {
		if seg != nil {
			total += int64(seg.used)
		}
	}
	return total
}

// oldestLiveSeg returns the index of the oldest non-nil segment, or -1 if the shard
// holds none. Rotation nils reclaimed slots (never slices sh.segs), so the oldest live
// segment can sit past leading nil slots from earlier rotations. Caller holds sh.mu.
func oldestLiveSeg(sh *shard) int {
	for i, seg := range sh.segs {
		if seg != nil {
			return i
		}
	}
	return -1
}

// overRetention reports whether the segment at idx (the oldest live one) should be dropped
// under r. Evaluated one segment at a time so count/byte bounds converge as segments are
// reclaimed. maxAgeID/maxAgeOK carry the interned MaxAgeAttr (for MaxAge); minAgeID/minAgeOK
// carry the interned MinAgeAttr (for MinAge and gcFloor, the runtime GC watermark from
// SetGCFloor). Caller holds sh.mu.
//
// Two independent reasons drop a segment: (1) it exceeds a hard ceiling -- MaxSegments,
// MaxBytes, or MaxAge -- enforced unconditionally, so neither a slow/absent consumer nor the
// MinAge floor can keep data past the configured policy (in particular MaxBytes always wins
// over MinAge); or (2) it has been fully consumed (its newest MinAgeAttr value is below
// gcFloor) AND is older than the MinAge floor, letting a short-lived queue drain consumed
// records early without ever dropping anything younger than MinAge.
func (sh *shard) overRetention(r Retention, idx int, now float64, maxAgeID uint32, maxAgeOK bool, minAgeID uint32, minAgeOK bool, gcFloor float64) bool {
	// Hard ceilings first: size/count/age caps that bound resource use unconditionally.
	if r.MaxSegments > 0 {
		live := 0
		for _, seg := range sh.segs {
			if seg != nil {
				live++
			}
		}
		if live > r.MaxSegments {
			return true
		}
	}
	if r.MaxBytes > 0 {
		var total int64
		for _, seg := range sh.segs {
			if seg != nil {
				total += int64(seg.used)
			}
		}
		if total > r.MaxBytes {
			return true
		}
	}
	// MaxAge ceiling: newest MaxAgeAttr value older than now-MaxAge ⇒ drop, unconditionally.
	// (A segment with no zone for the attribute is left to the size ceilings above: it holds
	// no numeric value for it, or some record holds an expression whose age only evaluating
	// it could tell, so it is kept. The active segment has nil zones and is never dropped here.)
	if r.MaxAge > 0 && maxAgeOK {
		if z, ok := sh.segs[idx].zones[maxAgeID]; ok && z.Max < now-r.MaxAge {
			return true
		}
	}
	// Early GC of consumed data: newest MinAgeAttr value below the feed's GC floor, but never
	// younger than the MinAge minimum-retention floor.
	if gcFloor > 0 && minAgeOK {
		if z, ok := sh.segs[idx].zones[minAgeID]; ok && z.Max < gcFloor && (r.MinAge <= 0 || z.Max < now-r.MinAge) {
			return true
		}
	}
	return false
}

// segLiveCount counts the data records (skipping time-checkpoint markers) in a segment
// by a single forward walk, returning the total and how many of them are system-keyed.
// Used to keep the append-only shard's record counts accurate when a segment is rotated
// out. Append-only records are never superseded, so every data record is live.
func segLiveCount(seg *segment) (n, sys int) {
	for off := 0; off < seg.used; {
		o := uint32(off)
		if end, ok := seg.damaged().skip(o); ok {
			off = int(end)
			continue
		}
		total := recTotalLen(seg.data, o)
		if total == 0 {
			break
		}
		if !recIsMarker(seg.data, o) {
			n++
			if isSystemKeyBytes(recKey(seg.data, o)) {
				sys++
			}
		}
		off += int(total)
	}
	return n, sys
}
