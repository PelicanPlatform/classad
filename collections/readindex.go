package collections

import (
	"math"
	"sort"
	"strings"

	"github.com/RoaringBitmap/roaring/v2"
)

// readIndex is a built segment index the query planner reads: an in-RAM segIndex (the
// active/hot representation) or a mmapSegIndex (a sealed segment's pageable sidecar). Both
// answer the same questions, so the planner can hold this interface and let a segment use
// either representation without special-casing.
type readIndex interface {
	covers(usable []usableProbe) bool
	coversGroups(groups [][]usableProbe) bool
	candidateOffsets(usable []usableProbe) *roaring.Bitmap
	candidateOffsetsGroups(groups [][]usableProbe) *roaring.Bitmap
	skipsPrefix(usable []usableProbe) bool
}

// indexPrimitives are the per-tier operations the shared planner logic (below) is built on.
// The skip/selectivity/candidate logic is written ONCE against these primitives so the two
// index representations cannot diverge; each type's readIndex methods are thin delegates.
type indexPrimitives interface {
	// statsFor returns the attribute's per-segment summary, or nil if not indexed here.
	statsFor(up usableProbe) *segStats
	// probeOffsets returns a fresh, mutable superset of record offsets matching one probe.
	probeOffsets(up usableProbe) *roaring.Bitmap
	// coversProbe reports whether this segment indexes the probe's attribute.
	coversProbe(up usableProbe) bool
	// bloomAbsent reports whether a categorical ==/in probe's values are ALL provably absent
	// (via the per-segment bloom). false when undeterminable: no bloom, a value might be
	// present, or the probe is not a categorical equality. The tiers differ only here (the
	// in-RAM stats hold the bloom; the mmap tier consults it on disk), which is why it is a
	// primitive rather than shared logic.
	bloomAbsent(up usableProbe) bool
}

func indexCovers(ix indexPrimitives, usable []usableProbe) bool {
	for _, up := range usable {
		if !ix.coversProbe(up) {
			return false
		}
	}
	return true
}

func indexCoversGroups(ix indexPrimitives, groups [][]usableProbe) bool {
	for _, g := range groups {
		if !indexCovers(ix, g) {
			return false
		}
	}
	return true
}

// indexCanSkip reports whether the indexed prefix provably holds no record satisfying the
// probe, so the query can skip it (and full-scan only the un-indexed tail). Correctness-
// critical: true only when certain. An exceptional record forbids a skip; `!=` never skips.
func indexCanSkip(ix indexPrimitives, up usableProbe) bool {
	s := ix.statsFor(up)
	if s == nil || s.exc > 0 {
		return false
	}
	if up.op == "present" || up.op == "absent" {
		return false // presence spans posted + exc + absent; never provably empty here
	}
	if up.cat {
		if up.op != "==" && up.op != "in" {
			return false
		}
		return ix.bloomAbsent(up) // every probe value definitely absent
	}
	if !s.hasRange {
		// No numeric record in the prefix (and exc==0): nothing an equality/range probe can
		// match. A `!=` still matches every non-exc record, so keep it.
		return up.op != "!="
	}
	switch up.op {
	case "==", "in":
		for _, t := range up.fvals {
			if t >= s.min && t <= s.max {
				return false
			}
		}
		return true
	case "<":
		return s.min >= up.fvals[0]
	case "<=":
		return s.min > up.fvals[0]
	case ">":
		return s.max <= up.fvals[0]
	case ">=":
		return s.max < up.fvals[0]
	}
	return false
}

// indexSkipsPrefix reports whether the whole indexed prefix can be skipped: the candidate
// set is the intersection of per-probe candidate sets, so any single provably-empty probe
// empties it. Cheaper than candidateOffsets for range probes (no key iteration).
func indexSkipsPrefix(ix indexPrimitives, usable []usableProbe) bool {
	for _, up := range usable {
		if indexCanSkip(ix, up) {
			return true
		}
	}
	return false
}

// indexEstCandidates estimates how many records a probe admits, for ordering only (never
// correctness). estCandidatesFromStats holds the logic so it is testable on a bare segStats.
func indexEstCandidates(ix indexPrimitives, up usableProbe) float64 {
	return estCandidatesFromStats(ix.statsFor(up), up)
}

func estCandidatesFromStats(s *segStats, up usableProbe) float64 {
	if s == nil {
		return math.MaxFloat64 // unknown: apply last
	}
	indexable := float64(s.covered - s.exc)
	switch up.op {
	case "==", "in":
		var sum float64
		if up.cat {
			for _, v := range up.svals {
				sum += s.estEqualStr(v)
			}
		} else {
			for _, v := range up.fvals {
				sum += s.estEqualFloat(v)
			}
		}
		return sum + float64(s.exc)
	case "!=":
		if up.cat {
			return indexable - s.estEqualStr(up.svals[0]) + float64(s.exc)
		}
		return indexable - s.estEqualFloat(up.fvals[0]) + float64(s.exc)
	case "isnt":
		if up.cat {
			return indexable - s.estEqualStr(strings.ToLower(up.svals[0])) + float64(s.exc)
		}
		return indexable
	case "<", "<=", ">", ">=":
		return s.estRange(up.op, up.fvals[0])*indexable + float64(s.exc)
	}
	return indexable
}

// indexSelectivityOrder returns indices into usable ordered by ascending estimated candidate
// count (most selective first). Pure ordering heuristic: the AND is commutative, so it never
// changes the result, only the work. Deterministic (stable, ties keep input order).
func indexSelectivityOrder(ix indexPrimitives, usable []usableProbe) []int {
	order := make([]int, len(usable))
	est := make([]float64, len(usable))
	for i, up := range usable {
		order[i] = i
		est[i] = indexEstCandidates(ix, up)
	}
	sort.SliceStable(order, func(a, b int) bool { return est[order[a]] < est[order[b]] })
	return order
}

// indexCandidateOffsets returns the offsets satisfying every usable probe (a superset the
// store re-verifies), applying the most-selective probe first so the roaring intersection
// shrinks fastest. nil means "no candidates".
func indexCandidateOffsets(ix indexPrimitives, usable []usableProbe) *roaring.Bitmap {
	switch len(usable) {
	case 0:
		return nil
	case 1:
		return ix.probeOffsets(usable[0])
	}
	order := indexSelectivityOrder(ix, usable)
	var acc *roaring.Bitmap
	for _, i := range order {
		bm := ix.probeOffsets(usable[i])
		if acc == nil {
			acc = bm
		} else {
			acc.And(bm)
		}
		if acc.IsEmpty() {
			return acc
		}
	}
	return acc
}

// indexCandidateOffsetsGroups returns the DNF union over groups of each group's candidate
// intersection. Callers pass only prunable plans (every group has a usable probe).
func indexCandidateOffsetsGroups(ix indexPrimitives, groups [][]usableProbe) *roaring.Bitmap {
	var acc *roaring.Bitmap
	for _, g := range groups {
		gb := indexCandidateOffsets(ix, g)
		if gb == nil {
			gb = roaring.New()
		}
		if acc == nil {
			acc = gb
		} else {
			acc.Or(gb)
		}
	}
	return acc
}
