package collections

import (
	"container/heap"
	"math"
)

// Choosing what to evict when an append-only store is over its size bound.
//
// The positional rule -- drop the oldest segment -- is correct only while append order
// tracks the attribute being retained against. A hub ingesting from several spokes breaks
// that: records arrive interleaved and a replaying spoke appends a year of backfill after
// records far younger, so the oldest SEGMENT is not the oldest DATA. See segprofile.go.
//
// Ages overlap, so segments cannot simply be sorted. Instead every profiled segment offers
// its breakpoints -- the value closing each bucketBytes of its data -- and the planner
// merges those streams in ascending value order, crediting one bucket per breakpoint taken.
// It is a k-way merge over per-segment sorted vectors; the heap exists so a pass that needs
// to free a little stops after examining a little, which is the common case.
//
// The result is a CUTOFF, not a segment list: every record whose value is below it may go.
// Segments then fall into two groups -- those entirely below the cutoff, which are unlinked
// whole, and those straddling it, which have to be rewritten to drop part of their contents.
// The two cost wildly different amounts (a rewrite costs the bytes it KEEPS), so the caller
// takes whole segments first and rewrites only if still short.

// evictEntry is one segment's position in the merge: the next breakpoint it offers.
type evictEntry struct {
	segIdx int     // index into the shard's segment slice
	k      int     // which breakpoint
	value  float64 // breaks[k]
}

type evictHeap []evictEntry

func (h evictHeap) Len() int           { return len(h) }
func (h evictHeap) Less(i, j int) bool { return h[i].value < h[j].value }
func (h evictHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *evictHeap) Push(x any)        { *h = append(*h, x.(evictEntry)) }
func (h *evictHeap) Pop() any          { old := *h; n := len(old); it := old[n-1]; *h = old[:n-1]; return it }

// evictPlan is what one size-driven pass decided.
type evictPlan struct {
	// cutoff: a record may be evicted when its value is < cutoff. Set so that the buckets
	// taken are exactly those at or below the last breakpoint the merge consumed (the
	// cutoff sits one representable step above it), which keeps "bytes below the cutoff"
	// countable with the same strict comparison the profiles use.
	cutoff float64
	// freed is a LOWER BOUND on the stored bytes the cutoff releases: whole buckets only,
	// so the final partial bucket of each segment is not counted. Under-counting leaves a
	// pass taking one more bucket than it strictly needed; over-counting would leave it
	// believing it had freed space it had not, and stopping over its ceiling.
	freed int64
	// whole are segment indices entirely below the cutoff: unlinked, nothing rewritten.
	whole []int
	// partial are segment indices holding data on both sides of the cutoff, most-freeable
	// first. Rewriting one costs the bytes it KEEPS, so a caller works this list under a
	// byte budget and may stop early.
	partial []int
	// enough reports whether the cutoff reaches the requested bytes. False means the
	// profiled segments do not hold that much below any cutoff -- the caller must fall
	// back to the positional rule rather than evict everything it can see.
	enough bool
}

// planEviction picks the cutoff that frees at least need bytes, given each segment's
// profile. segs is the shard's segment slice (nil holes and the active segment are skipped;
// the active segment is never evicted, as in Rotate). Only segments whose profile covers
// attr take part -- an unprofiled segment is not "empty below the cutoff", it is unknown,
// and treating the two alike is what would make this evict the wrong data silently.
func planEviction(segs []*segment, active *segment, need int64, attr string) evictPlan {
	var plan evictPlan
	if need <= 0 || attr == "" {
		return plan
	}
	h := make(evictHeap, 0, len(segs))
	profs := make(map[int]*segProfile, len(segs))
	for i, s := range segs {
		if s == nil || s == active || s.profile == nil || !s.profile.haveValues {
			continue
		}
		if len(s.profile.breaks) == 0 {
			continue // smaller than one bucket: nothing it can offer the merge
		}
		profs[i] = s.profile
		h = append(h, evictEntry{segIdx: i, k: 0, value: s.profile.breaks[0]})
	}
	if len(h) == 0 {
		return plan
	}
	heap.Init(&h)

	last := math.Inf(-1)
	for plan.freed < need && h.Len() > 0 {
		e := heap.Pop(&h).(evictEntry)
		p := profs[e.segIdx]
		// Credit the bucket this breakpoint CLOSES. Taking the breakpoint certifies that
		// everything before it in that segment is at or below its value -- not that the
		// bucket it opens is.
		plan.freed += p.bucketBytes
		last = e.value
		if e.k+1 < len(p.breaks) {
			heap.Push(&h, evictEntry{segIdx: e.segIdx, k: e.k + 1, value: p.breaks[e.k+1]})
		}
	}
	if plan.freed < need {
		return plan // not enough below any cutoff: leave enough false
	}
	plan.enough = true
	// One representable step above the last breakpoint taken, so a strict < comparison
	// includes exactly the buckets the merge consumed and nothing beyond them.
	plan.cutoff = math.Nextafter(last, math.Inf(1))

	// Classify. Recomputed from the cutoff rather than accumulated during the merge: the
	// merge visits buckets, and whether a SEGMENT goes whole is a property of all of it.
	var partialFree []int64
	for i, s := range segs {
		if s == nil || s == active || s.profile == nil {
			continue
		}
		b, whole := s.profile.bytesBelow(plan.cutoff)
		switch {
		case whole:
			plan.whole = append(plan.whole, i)
		case b > 0:
			plan.partial = append(plan.partial, i)
			partialFree = append(partialFree, b)
		}
	}
	// Most-freeable first: a rewrite costs the bytes it keeps, so the best candidate is the
	// one that keeps least.
	sortByKeyDesc(plan.partial, partialFree)
	return plan
}

// sortByKeyDesc sorts idx by the parallel key slice, descending. Insertion sort: the list is
// the number of segments straddling one cutoff, which is small.
func sortByKeyDesc(idx []int, key []int64) {
	for i := 1; i < len(idx); i++ {
		for j := i; j > 0 && key[j-1] < key[j]; j-- {
			idx[j-1], idx[j] = idx[j], idx[j-1]
			key[j-1], key[j] = key[j], key[j-1]
		}
	}
}
