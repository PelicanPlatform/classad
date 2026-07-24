package collections

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
// NOTE: MaxAgeAttr/MaxAge (drop by a segment's max value of a time attribute) needs the
// per-segment zone map, which the append-only Collection does not yet maintain; until
// then Rotate honors MaxSegments and MaxBytes only. Both suffice to bound disk use.
func (c *Collection) Rotate(now float64) (int, error) {
	if !c.appendOnly() || (c.ret == Retention{}) {
		return 0, nil
	}
	// Serialize against other whole-collection maintenance (retrain/rewrite), matching
	// Compact. Commits and queries never take maintMu.
	c.maintMu.Lock()
	defer c.maintMu.Unlock()

	sh := c.shards[0] // AppendOnly forces a single shard
	var toReap []*segment
	dropped := 0

	sh.mu.Lock()
	for {
		idx := oldestLiveSeg(sh)
		if idx < 0 || sh.segs[idx] == sh.act {
			break // nothing left, or only the active (still-appended) segment remains
		}
		if !sh.overRetention(c.ret, idx) {
			break
		}
		seg := sh.segs[idx]
		if n := segLiveCount(seg); n <= sh.count {
			sh.count -= n
		} else {
			sh.count = 0
		}
		if seg.retire() { // unpinned ⇒ reap now; else the last unpin reaps it
			toReap = append(toReap, seg)
		}
		sh.segs[idx] = nil // keep the slot so seg.id still equals its index (segAt invariant)
		dropped++
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

// overRetention reports whether the segment at idx (the oldest live one) should be
// dropped under r. Evaluated one segment at a time so count/byte bounds converge as
// segments are reclaimed. Caller holds sh.mu.
func (sh *shard) overRetention(r Retention, idx int) bool {
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
	// MaxAgeAttr/MaxAge: needs per-segment zone maps (a follow-up); ignored for now.
	return false
}

// segLiveCount counts the data records (skipping time-checkpoint markers) in a segment
// by a single forward walk. Used to keep the append-only shard's record count accurate
// when a segment is rotated out. Append-only records are never superseded, so every
// data record is live.
func segLiveCount(seg *segment) int {
	n := 0
	for off := 0; off < seg.used; {
		o := uint32(off)
		total := recTotalLen(seg.data, o)
		if total == 0 {
			break
		}
		if !recIsMarker(seg.data, o) {
			n++
		}
		off += int(total)
	}
	return n
}
