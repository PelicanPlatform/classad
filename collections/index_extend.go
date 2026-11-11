package collections

import (
	"maps"
	"slices"
	"sync/atomic"

	"github.com/RoaringBitmap/roaring/v2"
)

// KEEPING A WRITABLE SEGMENT'S INDEX CURRENT.
//
// An index covers its segment's records up to the point it was built, and a read walks the rest
// in full. On a mutable table that rest is never small for long: Reindex runs at open, after a
// compaction and when the index configuration changes, so every record written in between --
// the newest version of every key that has changed since -- sits in an uncovered tail of the
// shard's write target, or of a segment that has since filled and rotated. Every indexed read
// walks all of it, whatever its probes, and on a delta-record store each record it walks is a
// delta, rebuilt by merging the key's version chain. A queue mirror with a few thousand updates
// since its last compaction paid that for every lookup, including one the postings prove empty.
//
// An append log has the same tail for a different reason. Its indexes are built at a SEAL
// (Archive.afterAppend reindexes once the seal counter moves), and a seal also opens the next
// active segment, so what that reindex covers of the new segment is the one record that did not
// fit in the old one. Everything appended until the next seal -- up to a whole segment, 8 MiB by
// default -- is uncovered tail, and every indexed read decompresses and matches all of it. The
// active segment also has no zone map, so it is not prunable either: a probe whose key every
// sealed segment's zone map rules out still pays the open segment in full, and what that costs
// depends only on how long ago the last seal happened.
//
// So an indexed read first brings the indexes of the segments it is about to walk up to their
// current end (catchUpIndexes). That costs one decode of each new record -- less than the walk it
// replaces costs for each one -- and is paid once, by the first read after the writes, rather
// than by every read until the next Reindex or seal.
//
// Only the covered prefix moves. An index covers [0, upto) and the tail is walked (index.go), so
// a longer prefix is more selective and never a different answer.

// indexCatchUpBytes is the uncovered tail a segment may carry before an indexed read extends its
// index. Extending re-summarizes every posting of the segment, a cost that does not shrink with
// the tail, so a read leaves a short tail to be walked rather than paying it for a few records.
const indexCatchUpBytes = 4 << 10

// indexCatchUps counts segment indexes an indexed read extended (or built) before walking.
var indexCatchUps atomic.Int64

// IndexCatchUps reports how many segment indexes reads have brought up to their segment's end.
func IndexCatchUps() int64 { return indexCatchUps.Load() }

// catchUpIndexes extends the index of every segment that is not sealed to an immutable sidecar
// and has more than indexCatchUpBytes past its index.
//
// It keeps current what a Reindex built; it does not stand in for one. A segment indexed under an
// older configuration is left alone (adding an index is followed by the caller's Reindex, on the
// caller's schedule), and a segment with no index at all is indexed only in a shard that holds an
// index under the current configuration -- the write target a rotation opened after the last
// Reindex -- so a store nobody has asked to index stays unindexed.
//
// An append-only shard is held to be indexed whatever its segments currently carry, because it
// has already asked: a collection with any index configured indexes every segment of an append
// log as it seals, without a caller scheduling anything. Waiting for a segment to seal before its
// successor becomes eligible would leave the first segment of a new archive -- the only one a
// young archive has -- walked in full by every read.
//
// Its one unsealed segment is the only target it can offer. Sealed segments carry their index in
// an immutable sidecar, which is skipped below, and an append log never supersedes a record, so
// the bytes below the snapshotted write cursor are immutable while the build reads them off-lock.
//
// It runs only when no other index build holds reindexMu, so a read never waits behind a Reindex;
// one that finds it busy walks the tails, exactly as it did before this existed.
func (c *Collection) catchUpIndexes() {
	spec := c.spec.Load()
	if !spec.any() {
		return
	}
	appendOnly := c.appendOnly()
	type target struct {
		seg     *segment
		used    int
		indexed bool // the shard holds an index under spec
	}
	var tgts []target
	for _, sh := range c.shards {
		sh.mu.RLock()
		indexed := appendOnly
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			if mm := seg.msidx.Load(); mm != nil && mm.specGen == spec.gen {
				indexed = true
			} else if si := seg.idx.Load(); si != nil && si.specGen == spec.gen {
				indexed = true
			}
		}
		for _, seg := range sh.segs {
			if seg == nil || seg.used == 0 || seg.msidx.Load() != nil {
				continue
			}
			if catchUpDue(seg.idx.Load(), seg.used, spec, indexed) {
				seg.pin()
				tgts = append(tgts, target{seg: seg, used: seg.used, indexed: indexed})
			}
		}
		sh.mu.RUnlock()
	}
	if len(tgts) == 0 {
		return
	}
	defer func() {
		for _, t := range tgts {
			t.seg.unpin()
		}
	}()
	if !c.reindexMu.TryLock() {
		return
	}
	defer c.reindexMu.Unlock()
	// Re-read under reindexMu: a Reindex that finished while the targets were gathered may have
	// built these already, and the configuration may have moved under them.
	spec = c.spec.Load()
	if !spec.any() {
		return
	}
	for _, t := range tgts {
		if t.seg.msidx.Load() != nil {
			continue // sealed to its sidecar meanwhile; that index is final
		}
		cur := t.seg.idx.Load()
		if !catchUpDue(cur, t.used, spec, t.indexed) {
			continue
		}
		t.seg.idx.Store(extendSegIndex(c, t.seg, cur, t.used, spec))
		indexCatchUps.Add(1)
	}
}

// catchUpDue reports whether a segment with used bytes and index cur (nil: none) is far enough
// past cur to be indexed before a read. A segment with no index qualifies only in a shard that is
// indexed under spec (indexed); one indexed under another configuration never does.
func catchUpDue(cur *segIndex, used int, spec *indexSpec, indexed bool) bool {
	switch {
	case cur == nil:
		return indexed && used > indexCatchUpBytes
	case cur.specGen != spec.gen:
		return false
	}
	return used-int(cur.upto) > indexCatchUpBytes
}

// extendSegIndex returns seg's index over [0, upto), built from base -- its index over a prefix --
// plus the records base does not cover. The result is exactly what buildSegIndex(c, seg, upto,
// spec) returns; only the work differs. A base that is nil, built under another configuration, or
// longer than upto is no help, and the index is built afresh.
//
// base is published and may be in use by a concurrent read, so nothing of it is modified: a
// posting the new records extend is replaced by a union, and one they do not touch is shared.
func extendSegIndex(c *Collection, seg *segment, base *segIndex, upto int, spec *indexSpec) *segIndex {
	if base == nil || base.specGen != spec.gen || int(base.upto) > upto {
		return buildSegIndex(c, seg, upto, spec)
	}
	if int(base.upto) == upto {
		return base
	}
	si := indexSegRange(c, seg, int(base.upto), upto, spec)
	si.all = roaring.Or(base.all, si.all)
	vals := make(map[uint32][]float64, len(si.val))
	for id, np := range si.cat {
		bp := base.cat[id]
		if bp == nil {
			return buildSegIndex(c, seg, upto, spec) // cannot happen under one spec gen; never guess
		}
		// finalizeExact folded each case-uniform bucket's spelling into exactCase and dropped its
		// bitmap. Undo that for base, so the merged index finalizes over every spelling exactly as
		// a fresh build does: a bucket the new records give a second spelling becomes mixed.
		exact := maps.Clone(bp.exact)
		if exact == nil {
			exact = map[string]*roaring.Bitmap{}
		}
		for f, ec := range bp.exactCase {
			if ec == "" {
				ec = f
			}
			exact[ec] = bp.post[f]
		}
		np.post = mergePostings(bp.post, np.post)
		np.exact = mergePostings(exact, np.exact)
		np.exc = roaring.Or(bp.exc, np.exc)
		np.posted = roaring.Or(bp.posted, np.posted)
	}
	for id, np := range si.val {
		bp := base.val[id]
		if bp == nil {
			return buildSegIndex(c, seg, upto, spec)
		}
		var added []float64 // values the new records introduce, which base's order lacks
		for k := range np.post {
			if bp.post[k] == nil {
				added = append(added, k)
			}
		}
		np.post = mergePostings(bp.post, np.post)
		np.exc = roaring.Or(bp.exc, np.exc)
		np.posted = roaring.Or(bp.posted, np.posted)
		vals[id] = mergeSorted(bp.sortedKeys, added)
	}
	for key, np := range si.comp {
		bp := base.comp[key]
		if bp == nil {
			return buildSegIndex(c, seg, upto, spec)
		}
		np.post = mergePostings(bp.post, np.post)
		np.exc = roaring.Or(bp.exc, np.exc)
	}
	for _, cp := range si.cat {
		cp.finalizeExact()
		cp.finishStats()
	}
	for _, cp := range si.comp {
		cp.finishStats()
	}
	for id, vp := range si.val {
		vp.finishStatsSorted(vals[id])
	}
	return si
}

// mergePostings returns base with add's postings merged in, as a new map: a key in both gets a new
// bitmap holding the union, a key in only one keeps that bitmap. Neither input is modified (add's
// bitmaps are carried over, so add must not be used afterwards).
//
// A key in both keeps base's spelling of it, as a build that met base's records first would have:
// for a value index -0 and +0 are one key, and assigning through add's spelling would replace it.
func mergePostings[K comparable](base, add map[K]*roaring.Bitmap) map[K]*roaring.Bitmap {
	out := make(map[K]*roaring.Bitmap, len(base)+len(add))
	for k, bm := range base {
		if a := add[k]; a != nil {
			bm = roaring.Or(bm, a)
		}
		out[k] = bm
	}
	for k, bm := range add {
		if base[k] == nil {
			out[k] = bm
		}
	}
	return out
}

// mergeSorted returns the ascending union of sorted, which is ascending, and added, which holds
// values sorted does not, in any order. Neither input is modified.
func mergeSorted(sorted, added []float64) []float64 {
	slices.Sort(added)
	out := make([]float64, 0, len(sorted)+len(added))
	i, j := 0, 0
	for i < len(sorted) && j < len(added) {
		if added[j] < sorted[i] {
			out = append(out, added[j])
			j++
		} else {
			out = append(out, sorted[i])
			i++
		}
	}
	out = append(out, sorted[i:]...)
	return append(out, added[j:]...)
}
