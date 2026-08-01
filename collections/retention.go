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
// Retention honors MaxSegments, MaxBytes, and MaxAgeAttr/MaxAge (drop a segment whose
// newest value of the age attribute is older than now-MaxAge); MaxAge requires the age
// attribute to be a configured ZoneAttr so its per-segment max is available.
func (c *Collection) Rotate(now float64) (int, error) {
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

	sh := c.shards[0] // AppendOnly forces a single shard
	// Resolve the two independent age attributes (via the shared intern table): MaxAgeAttr for
	// the MaxAge ceiling, MinAgeAttr for the MinAge floor and the GC-floor drain. Each is
	// resolved only when its bound is active.
	gcFloor := c.gcFloor
	var maxAgeID, minAgeID uint32
	var maxAgeOK, minAgeOK bool
	if c.ret.MaxAgeAttr != "" && c.ret.MaxAge > 0 {
		maxAgeID, maxAgeOK = c.intern.LookupID(c.ret.MaxAgeAttr)
	}
	if c.ret.MinAgeAttr != "" && gcFloor > 0 {
		minAgeID, minAgeOK = c.intern.LookupID(c.ret.MinAgeAttr)
	}
	var toReap []*segment
	dropped := 0

	sh.mu.Lock()
	for {
		idx := oldestLiveSeg(sh)
		if idx < 0 || sh.segs[idx] == sh.act {
			break // nothing left, or only the active (still-appended) segment remains
		}
		if !sh.overRetention(c.ret, idx, now, maxAgeID, maxAgeOK, minAgeID, minAgeOK, gcFloor) {
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

// SetRetention updates the retention bounds at runtime (an append-only collection). The
// next Rotate enforces them. Serialized against Rotate/maintenance via maintMu, so c.ret is
// only ever read or written under that lock.
func (c *Collection) SetRetention(r Retention) {
	c.maintMu.Lock()
	c.ret = r
	c.maintMu.Unlock()
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
	// (A segment with no zone for the attribute carries no such value and is left to the
	// size ceilings above; the active segment has nil zones and is never dropped here.)
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
