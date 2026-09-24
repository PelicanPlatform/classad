package collections

import "sync/atomic"

// strandedSealedDeltas counts live delta records found sitting in SEALED segments.
//
// The seal-collapse invariant says there are none: liveDeltaKeys scans only the active segments
// (plus the pending-seal and retry sets) on the stated grounds that "by the seal-collapse
// invariant they are the only place a live fragment exists", and every collapse and every
// pre-compaction pass is built on that. Nothing has ever checked it.
//
// It matters because the failure is silent and terminal. A live delta in a sealed segment is
// invisible to every collapse, so its whole record is never rewritten; compaction then reclaims
// that superseded base, and the key is left permanently unreadable and unwritable -- which is
// what production reports as delta-no-base, on chains of 29 to 41 versions with not one whole
// record among them and the walk otherwise intact.
var strandedSealedDeltas atomic.Int64

// StrandedSealedDeltas reports how many live delta records were found in sealed segments at
// open. Any value above zero means the seal-collapse invariant is broken on disk, and the keys
// behind those records are on their way to becoming unreadable.
func StrandedSealedDeltas() int64 { return strandedSealedDeltas.Load() }

// repairStrandedSealedDeltas collapses the fragments checkSealCollapseInvariant found, rescuing
// every one whose base is still on disk, and returns the keys it acted on for reporting.
//
// Repairing is worth the write because the loss is otherwise permanent AND silent. Of the first
// five stranded keys sampled on a production mirror, one was still readable -- its base intact,
// rescuable by exactly this collapse -- and four were already gone, compaction having reclaimed
// their bases while nothing was looking. A fragment collapsed here becomes a whole record; one
// left alone becomes an unreadable row that also refuses every future write to that key.
//
// A key whose base is ALREADY gone cannot be collapsed (collapsing it means reading it), so it
// stays as it is: this recovers what is recoverable and does not pretend about the rest.
func (c *Collection) repairStrandedSealedDeltas(keys [][]byte) {
	if len(keys) == 0 {
		return
	}
	if c.deltas == nil {
		c.deltas = newDeltaTracker() // a reopen may not have one yet; collapseBatch needs it
	}
	for attempt := 0; attempt < collapseAttempts && len(keys) > 0; attempt++ {
		keys = c.collapseBatch(keys)
	}
	// Remember what is still not collapsed, as collapseLiveChains does. Discarding it here made
	// the one pass whose whole purpose is rescuing stranded fragments the one pass that forgot
	// its own failures.
	for _, k := range keys {
		c.rememberCollapse(k)
	}
}

// segHasLiveDelta reports whether a segment holds any LIVE delta record. Callers about to seal
// a segment use it to refuse: sealing one is what strands the fragment.
//
// Caller holds the shard lock.
func segHasLiveDelta(seg *segment) bool {
	if seg == nil || seg.used == 0 {
		return false
	}
	for off := uint32(0); off < uint32(seg.used); {
		tl := recTotalLen(seg.data, off)
		if tl == 0 || off+tl > uint32(seg.used) {
			break
		}
		if !recIsMarker(seg.data, off) && recSuperseded(seg.data, off) == seqMax && segRecIsDelta(seg, off) {
			return true
		}
		off += tl
	}
	return false
}

// checkSealCollapseInvariant counts live deltas in this collection's sealed segments and returns
// their keys, capped. It runs at open, where the cost is bounded by what is already being mapped
// and no reader is waiting on it -- not per pass, which would be a full scan on the hot path.
func (c *Collection) checkSealCollapseInvariant(maxKeys int) [][]byte {
	if !c.deltaRead {
		return nil // no delta records: the invariant is vacuous
	}
	var keys [][]byte
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil || seg == sh.act || seg.used == 0 {
				continue
			}
			for off := uint32(0); off < uint32(seg.used); {
				tl := recTotalLen(seg.data, off)
				if tl == 0 || off+tl > uint32(seg.used) {
					break
				}
				// Live (not superseded) AND a delta: a superseded fragment in a sealed segment
				// is ordinary history, and only the live one is a stranded chain head.
				if !recIsMarker(seg.data, off) && recSuperseded(seg.data, off) == seqMax && segRecIsDelta(seg, off) {
					strandedSealedDeltas.Add(1)
					if len(keys) < maxKeys {
						keys = append(keys, append([]byte(nil), recKey(seg.data, off)...))
					}
				}
				off += tl
			}
		}
		sh.mu.RUnlock()
	}
	return keys
}
