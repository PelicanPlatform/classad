package collections

import (
	"slices"
	"sort"
	"time"
)

// Index backfill carries the current index configuration to sealed segments whose sidecar
// does not cover it -- the segments a bounded Reindex (Options.IndexBackfillBytes) leaves
// behind when an index is added. Reindex keeps its bound: it runs on the append path after
// every seal and inside every add, so it must stay cheap. The backfill does the rest of the
// work separately, a budgeted slice at a time, newest segments first, so a large archive is
// covered over several maintenance passes instead of in one long stall.
//
// Nothing records its progress: a segment's sidecar says which attributes it covers, the
// rebuilt sidecar is written atomically, and reopen adopts a sidecar whatever configuration
// it was built under. A restart therefore resumes exactly where the last pass stopped.

// IndexBackfillResult reports one backfill pass.
type IndexBackfillResult struct {
	// Rebuilt is how many segments now cover every indexed attribute, and RebuiltBytes their
	// stored size (what the budget is charged against).
	Rebuilt      int   `json:"rebuilt"`
	RebuiltBytes int64 `json:"rebuiltBytes"`
	// Failed is how many segments the pass tried and could not rebuild; they keep their
	// existing sidecar and are retried by the next pass.
	Failed int `json:"failed,omitempty"`
	// PendingSegments and PendingBytes are what still lacks coverage after the pass.
	PendingSegments int   `json:"pendingSegments"`
	PendingBytes    int64 `json:"pendingBytes"`
}

// lacksCoverage reports whether seg is a sealed segment whose sidecar index does not hold
// postings for every attribute spec indexes, or holds them as an older builder left them
// (mmapSegIndex.stalePostings). Only segments already sealed to a sidecar qualify: one
// without a sidecar index is Reindex's to build, whatever its age, because a missing index is
// not optional the way an out-of-date one is. Caller holds sh.mu.
func lacksCoverage(seg, act *segment, spec *indexSpec) bool {
	if seg == nil || seg == act || seg.used == 0 || !spec.any() {
		return false
	}
	mm := seg.msidx.Load()
	if mm == nil {
		return false
	}
	return !mm.coversSpec(spec) || mm.stalePostings()
}

// coversSpec reports whether the sidecar holds every attribute spec indexes, as the kind
// spec indexes it as. An attribute the sidecar holds as the other kind does not count: a
// probe looks the attribute up in the directory for its own kind, so it would read as
// uncovered there too.
func (si *mmapSegIndex) coversSpec(spec *indexSpec) bool {
	for _, id := range spec.catIDs {
		if _, ok := si.catDir[id]; !ok {
			return false
		}
	}
	for _, id := range spec.valIDs {
		if _, ok := si.valDir[id]; !ok {
			return false
		}
	}
	return true
}

// IndexBackfillPending reports how many sealed segments lack postings for some currently
// indexed attribute, and their stored bytes: what the backfill has still to do. It reads
// only resident sidecar directories.
func (c *Collection) IndexBackfillPending() (segments int, bytes int64) {
	spec := c.spec.Load()
	if !spec.any() {
		return 0, 0
	}
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if lacksCoverage(seg, sh.act, spec) {
				segments++
				bytes += int64(seg.used)
			}
		}
		sh.mu.RUnlock()
	}
	return segments, bytes
}

// BackfillIndexes rebuilds the sidecars of sealed segments that lack postings for a
// currently indexed attribute, newest first, until maxBytes of segment data have been
// re-read (the pass overshoots by at most the last segment). maxBytes <= 0 means no bound.
//
// Newest first because that is the end an archive is read from: a newest-first query with
// a limit stops early, so the segments it reaches first gain the most from an index.
//
// It holds reindexMu for one segment at a time rather than for the pass. The archive's
// append path reindexes after every seal under the same lock, so holding it across a
// whole-archive backfill would stall ingest for as long as the backfill ran; per segment,
// an append waits at most one segment's rebuild. The segment is pinned while it is rebuilt,
// so a concurrent merge or rotation that retires it defers the unmap until the rebuild is
// done; the new sidecar is swapped in under the shard lock and the one it replaces is
// released once the scans reading it finish (see installSidecar).
func (c *Collection) BackfillIndexes(maxBytes int64) IndexBackfillResult {
	var res IndexBackfillResult
	type cand struct {
		sh   *shard
		seg  *segment
		rank int // position from the newest segment of its shard; 0 = newest
	}
	var cands []cand
	spec := c.spec.Load()
	for _, sh := range c.shards {
		sh.mu.RLock()
		rank := 0
		for i := len(sh.segs) - 1; i >= 0; i-- {
			seg := sh.segs[i]
			if seg == nil {
				continue
			}
			if lacksCoverage(seg, sh.act, spec) {
				cands = append(cands, cand{sh: sh, seg: seg, rank: rank})
			}
			rank++
		}
		sh.mu.RUnlock()
	}
	// Interleave shards by age so no shard's newest data waits behind another's oldest.
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].rank < cands[j].rank })

	for _, cd := range cands {
		if maxBytes > 0 && res.RebuiltBytes >= maxBytes {
			break
		}
		used, ok, done := c.backfillOne(cd.sh, cd.seg)
		if !done {
			continue // merged away, rotated out, or covered by someone else since the snapshot
		}
		res.RebuiltBytes += int64(used)
		if ok {
			res.Rebuilt++
		} else {
			res.Failed++
		}
	}
	res.PendingSegments, res.PendingBytes = c.IndexBackfillPending()
	return res
}

// backfillOne rebuilds one candidate's sidecar under the current spec, re-checking under
// reindexMu that the segment is still live and still lacks coverage: the candidate list
// was taken without the lock, and a merge, a rotation or a concurrent Reindex may have
// dealt with it since. done reports whether a rebuild was attempted, ok whether it
// succeeded, and used the segment's stored size.
func (c *Collection) backfillOne(sh *shard, seg *segment) (used int, ok, done bool) {
	c.reindexMu.Lock()
	defer c.reindexMu.Unlock()
	spec := c.spec.Load()
	sh.mu.RLock()
	live := slices.Contains(sh.segs, seg) && lacksCoverage(seg, sh.act, spec)
	if live {
		seg.pin()
		used = seg.used
	}
	sh.mu.RUnlock()
	if !live {
		return 0, false, false
	}
	start := time.Now()
	ok = c.reindexSealed(sh, seg, spec)
	c.opm.backfill.observe(time.Since(start))
	seg.unpin() // no shard lock held: the last unpin may reap a segment retired meanwhile
	return used, ok, true
}

// ReindexAll is Reindex with no backfill bound: every sealed segment ends up covering every
// indexed attribute, however old. It is what an operator asking for a reindex means, and
// costs a decompression of every segment that lacks coverage -- for a large archive, a long
// time. The append path is not stalled for that long (see BackfillIndexes).
func (c *Collection) ReindexAll() IndexBackfillResult {
	c.Reindex()
	return c.BackfillIndexes(0)
}
