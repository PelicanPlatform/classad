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
// So an indexed read first brings the indexes of the segments it is about to walk up to their
// current end (catchUpIndexes). That costs one decode of each new record -- less than the walk it
// replaces costs for each one -- and is paid once, by the first read after the writes, rather
// than by every read until the next Reindex.

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
// It runs only when no other index build holds reindexMu, so a read never waits behind a Reindex;
// one that finds it busy walks the tails, exactly as it did before this existed.
func (c *Collection) catchUpIndexes() {
	spec := c.spec.Load()
	if !spec.any() || c.appendOnly() {
		return // an archive indexes each segment as it seals (Archive.afterAppend)
	}
	type target struct {
		seg     *segment
		used    int
		indexed bool // the shard holds an index under spec
	}
	var tgts []target
	for _, sh := range c.shards {
		sh.mu.RLock()
		indexed := false
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
	for _, cp := range si.cat {
		cp.finalizeExact()
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
