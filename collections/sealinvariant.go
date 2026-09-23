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

// checkSealCollapseInvariant counts live deltas in this collection's sealed segments and returns
// their keys, capped. It runs at open, where the cost is bounded by what is already being mapped
// and no reader is waiting on it -- not per pass, which would be a full scan on the hot path.
//
// It reports rather than repairs: a repair here would write during open, and until it is known
// HOW a fragment gets stranded, quietly rewriting them would erase the evidence for the cause.
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
