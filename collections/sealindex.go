package collections

import "sync/atomic"

// provisionalIndexBuilds counts key indexes built at seal time rather than waiting for a reindex
// pass. It is the work this closes the window with: one build per sealed segment, once.
var provisionalIndexBuilds atomic.Int64

// ProvisionalIndexBuilds reports how many sealed segments were given a provisional key index at
// seal time. See segment.keyIdxMem.
func ProvisionalIndexBuilds() int64 { return provisionalIndexBuilds.Load() }

// indexSealedSegments gives every sealed segment that has no key index a provisional one, built
// on the Go heap.
//
// The durable index is written by a reindex pass, not when a segment seals, so between the two
// the sealed-segment probe had nothing to look in: it could not see that segment's records at
// all. A chain whose base lived there could not be found, and the write was refused as
// delta-index-pending and dropped -- though a retry after the reindex would have succeeded. A
// production mirror skipped three million probes in six hours that way.
//
// Running it here, from the post-commit hook, keeps the build off the shard write lock and off
// the path that seals (writeRecord holds that lock, and this does file-free work only). The scan
// itself is a nil check per sealed segment; the build happens once per segment, ever.
func (c *Collection) indexSealedSegments() {
	for _, sh := range c.shards {
		sh.mu.RLock()
		var todo []*segment
		for _, seg := range sh.segs {
			if seg == nil || seg == sh.act || seg.used == 0 {
				continue
			}
			if seg.keyIdx.Load() == nil && seg.keyIdxMem.Load() == nil {
				todo = append(todo, seg)
			}
		}
		sh.mu.RUnlock()
		if len(todo) == 0 {
			continue
		}
		// Built off the lock. A segment retired between the two is harmless: the index is
		// published onto the segment and goes away with it.
		for _, seg := range todo {
			ki, err := parseKeyIndex(buildKeyIndex(seg.data, seg.used, c.h))
			if err != nil {
				continue // the durable pass will build it; the probe falls back to a walk
			}
			// CompareAndSwap, not Store: two commits can reach the same segment concurrently,
			// and the second would otherwise replace an index the first already published.
			if seg.keyIdxMem.CompareAndSwap(nil, ki) {
				provisionalIndexBuilds.Add(1)
			}
		}
	}
}
