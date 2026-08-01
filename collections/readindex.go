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
	estCandidates(up usableProbe) float64 // selectivity estimate (ordering/tuning only)
	coveredUpto() uint32                  // the index covers offsets [0, coveredUpto); the tail is scanned
	// catCanonicalValues emits each distinct canonical spelling of categorical attribute id
	// (for MATCH's finite-value materialization). add returns false to stop early;
	// catCanonicalValues then returns false too. Returns true when all values were emitted.
	catCanonicalValues(id uint32, add func(string) bool) bool
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

// bulkOrMin is the posting count at which unionPostings switches from a running in-place
// Or to roaring's batched union. The batch trades transient memory (it promotes each
// accumulating container to a dense bitmap container) for linear rather than quadratic
// merge cost, so it only pays once there are enough postings for the quadratic term to
// dominate -- measured crossover is around 100 on an 8 MiB segment's offset space.
const bulkOrMin = 128

// unionOrWorkers is the worker count handed to roaring's batched union. One is deliberate:
// almost all of the batch's win is algorithmic (see unionPostings), extra workers add well
// under 2x on top, and a query is often already fanned out across shards and segments --
// so spending a whole machine's cores inside one probe would oversubscribe it.
const unionOrWorkers = 1

// unionPostings ORs n posting bitmaps -- at(i) returns the i'th, or nil for "no posting" --
// into one fresh, mutable bitmap.
//
// A range probe over a high-cardinality attribute (say `CompletionDate > t`) unions one
// posting per distinct key in the matching run, which can be thousands, and each posting is
// a handful of record offsets scattered across the segment. Folding those with a running
// acc.Or is quadratic: the accumulator's containers stay ARRAY containers, so every one of
// the n merges re-runs a two-pointer merge over everything accumulated so far. roaring's
// batched union instead promotes each accumulating container to a bitmap container and
// unions lazily into it (cardinality repaired once at the end), which makes each posting
// cost its own size rather than the accumulator's -- ~15x by itself on a 10k-posting range
// over an 8 MiB segment's offset space. FastOr does NOT do this promotion, so it is not the
// right batch here despite the name; ParOr is.
func unionPostings(n int, at func(i int) *roaring.Bitmap) *roaring.Bitmap {
	if n < bulkOrMin {
		return runningOr(n, at)
	}
	bms := make([]*roaring.Bitmap, 0, n)
	for i := 0; i < n; i++ {
		if p := at(i); p != nil {
			bms = append(bms, p)
		}
	}
	if len(bms) < bulkOrMin { // mostly holes: not enough left to repay the batch
		return runningOr(len(bms), func(i int) *roaring.Bitmap { return bms[i] })
	}
	// ParOr returns a fresh bitmap that shares containers copy-on-write with its inputs
	// (exactly as a running Or's appendCopy already would), so the result is safe to
	// mutate. It also compacts bms in place, which is why bms is not reused after.
	return roaring.ParOr(unionOrWorkers, bms...)
}

func runningOr(n int, at func(i int) *roaring.Bitmap) *roaring.Bitmap {
	bm := roaring.New()
	for i := 0; i < n; i++ {
		if p := at(i); p != nil {
			bm.Or(p)
		}
	}
	return bm
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
	case "<", "<=", ">", ">=":
		// A two-sided probe skips if EITHER bound puts the segment's whole [min,max]
		// outside the band, so a band narrower than the segment's span prunes segments
		// that neither half-open side would have.
		return rangeBoundSkips(s, up.op, up.fvals[0]) ||
			(up.twoSided() && rangeBoundSkips(s, up.hiOp, up.hiVal))
	}
	return false
}

// rangeBoundSkips reports whether no value in [s.min, s.max] satisfies one range bound.
func rangeBoundSkips(s *segStats, op string, t float64) bool {
	switch op {
	case "<":
		return s.min >= t
	case "<=":
		return s.min > t
	case ">":
		return s.max <= t
	case ">=":
		return s.max < t
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
		frac := s.estRange(up.op, up.fvals[0])
		if up.twoSided() {
			frac = s.estBand(up)
		}
		return frac*indexable + float64(s.exc)
	}
	return indexable
}

// estBand estimates the fraction of indexable records inside a two-sided range probe's
// band. Over a uniform [min,max] the two one-sided fractions overlap by
// lowFrac + highFrac - 1, so a narrow band scores far more selective than either side
// alone -- which is the point of coalescing them: the planner now applies the band first.
func (s *segStats) estBand(up usableProbe) float64 {
	lo := s.estRange(up.op, up.fvals[0])
	hi := s.estRange(up.hiOp, up.hiVal)
	if !s.hasRange || s.max <= s.min {
		// A degenerate span makes each side a 0/1 verdict; the band is their AND.
		return math.Min(lo, hi)
	}
	if f := lo + hi - 1; f > 0 {
		return f
	}
	return 0
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
	if len(groups) == 0 {
		return nil
	}
	// Each group's intersection is computed first, then the disjuncts are unioned in one
	// batch (see unionPostings) rather than folded pairwise.
	per := make([]*roaring.Bitmap, len(groups))
	for i, g := range groups {
		if per[i] = indexCandidateOffsets(ix, g); per[i] == nil {
			per[i] = roaring.New()
		}
	}
	return unionPostings(len(per), func(i int) *roaring.Bitmap { return per[i] })
}
