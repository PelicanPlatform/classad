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
	// catValueCount returns how many records in the indexed prefix carry exactly key
	// (case-sensitive) for categorical attribute id -- the cardinality of that value's
	// posting, without materializing the posting itself. ok is false when the attribute is
	// not categorically indexed here or no record spells the value that way. Paired with
	// catCanonicalValues it answers a GROUP BY count from the index alone.
	catValueCount(id uint32, key string) (count uint64, ok bool)
	// compositeCandidates reports the EXACT number of candidates a composite index would
	// hand the scan for this conjunction, and whether this segment holds one the planner
	// would take. For EXPLAIN, which otherwise reports the minimum of the per-attribute
	// estimates -- independent guesses about correlated columns.
	compositeCandidates(usable []usableProbe) (count float64, tuple string, ok bool)
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
	// recordCount is how many records this segment's index covers -- the size of the set an
	// `absent` probe subtracts from. Called ONLY for present/absent estimates: on the mmap
	// tier it reads the all-records bitmap, which the planner otherwise deliberately avoids
	// paging in, and which an absent probe's execution clones anyway.
	recordCount() uint64
	// compositeMatch reports the composite index this segment holds that covers a
	// conjunction's equality probes -- which probes it covers, the records posted under the
	// tuple, and the composite's exception set. ok is false when the segment holds no
	// composite whose every component the conjunction pins (see composite.go). It is a
	// primitive because the two tiers hold their composites differently (a map of postings
	// in RAM, a byte-sorted key run in the sidecar); everything decided FROM the match --
	// whether to take it, what it replaces -- is shared logic below.
	compositeMatch(usable []usableProbe) (compositeMatch, bool)
	// bloomAbsent reports whether a categorical ==/in probe's values are ALL provably absent
	// (via the per-segment bloom). false when undeterminable: no bloom, a value might be
	// present, or the probe is not a categorical equality. The tiers differ only here (the
	// in-RAM stats hold the bloom; the mmap tier consults it on disk), which is why it is a
	// primitive rather than shared logic.
	bloomAbsent(up usableProbe) bool
	// prefixKeyCount is how many distinct folded categorical keys begin with a prefix
	// probe's literal, or ok=false when the attribute is not indexed categorically here.
	// Two boundary searches over the sorted keys: it answers how much the prefix prunes
	// without reading a single posting, so a planner that then declines the index has paid
	// O(log n) to decline it. A zero count with no exceptional records also means the
	// segment holds nothing the probe can match (indexCanSkip).
	prefixKeyCount(up usableProbe) (int, bool)
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
	// A probe the planner proved impossible -- an inverted band (`Memory < 1024 && Memory
	// > 2048`), or an equality whose values a range on the same attribute excluded -- is
	// unsatisfiable for every value, so it skips ahead of and independently of the
	// per-segment stats. Unlike the data-driven skips below it need not spare the exception
	// set: those records are re-verified because the INDEX cannot classify them, but no
	// value of any type can satisfy the constraint. See usableProbe.neverMatches.
	if up.neverMatches() {
		return true
	}
	s := ix.statsFor(up)
	if s == nil || s.exc > 0 {
		return false
	}
	if up.op == "present" || up.op == "absent" {
		return false // presence spans posted + exc + absent; never provably empty here
	}
	if up.cat {
		if up.op == "prefix" {
			// No key in the segment begins with the literal, and no exceptional record to
			// re-verify (checked above): nothing here can match the pattern.
			n, ok := ix.prefixKeyCount(up)
			return ok && n == 0
		}
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
	// A composite can prove the prefix empty where no single probe can: `ClusterId == 7 &&
	// ProcId == 3` is unskippable on either attribute alone whenever the segment holds
	// cluster 7 and holds some proc 3, and provably empty the moment nothing posted the
	// PAIR. That is the joint selectivity the per-attribute stats cannot see. Sound because
	// a record satisfying both equalities carries both components and is exceptional in
	// neither, so it would be in the tuple's posting -- and cm.empty() requires the
	// composite's exception set to be empty too.
	if cm, ok := ix.compositeMatch(usable); ok && cm.empty() {
		return true
	}
	return false
}

// indexEstCandidates estimates how many records a probe admits, for ordering only (never
// correctness). estCandidatesFromStats holds the logic so it is testable on a bare segStats.
func indexEstCandidates(ix indexPrimitives, up usableProbe) float64 {
	if up.op == "prefix" {
		// Needs the key run as well as the stats, so it cannot be expressed on a bare
		// segStats the way the other operators are.
		return estPrefixCandidates(ix, up)
	}
	var total float64
	// Only the presence probes need the segment's record count, and only they pay for it.
	if up.op == "present" || up.op == "absent" {
		total = float64(ix.recordCount())
	}
	return estCandidatesFromStats(ix.statsFor(up), total, up)
}

// estCandidatesFromStats estimates the candidates one probe admits. total is the segment's
// covered record count, needed only by the presence probes (0 otherwise).
func estCandidatesFromStats(s *segStats, total float64, up usableProbe) float64 {
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
	case "present":
		// Executes as posted|exc, so every record carrying the attribute in any form is a
		// candidate -- that is exactly `covered`.
		return float64(s.covered)
	case "absent":
		// Executes as all AND NOT posted: the records with no indexed value, which includes
		// the exceptional ones (present but not the indexed literal type) because those are
		// re-verified rather than trusted.
		//
		// Without this case both presence probes fell through to the default below and were
		// estimated at `indexable` -- so `attr is undefined` was estimated at the count of
		// records that HAVE the attribute, and the two complementary probes scored
		// identically instead of summing to the segment. On a table where every ad carries
		// the attribute that is the difference between "matches nothing" and "matches
		// everything", which is the whole of what selectivity decides.
		if e := total - indexable; e > 0 {
			return e
		}
		return 0
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
	if up.emptyBand() {
		return 0 // admits nothing: the most selective probe there is, so apply it first
	}
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
func indexSelectivityOrder(ix indexPrimitives, usable []usableProbe) ([]int, []float64) {
	order := make([]int, len(usable))
	est := make([]float64, len(usable))
	for i, up := range usable {
		order[i] = i
		est[i] = indexEstCandidates(ix, up)
	}
	sort.SliceStable(order, func(a, b int) bool { return est[order[a]] < est[order[b]] })
	return order, est
}

// indexCandidateOffsets returns the offsets satisfying every usable probe (a superset the
// store re-verifies), applying the most-selective probe first so the roaring intersection
// shrinks fastest. nil means "no candidates".
func indexCandidateOffsets(ix indexPrimitives, usable []usableProbe) *roaring.Bitmap {
	// An impossible band admits nothing at all -- not even the exception set a range
	// probe otherwise unions in, since no value of any type can satisfy both bounds
	// (see usableProbe.emptyBand). Answering here keeps both index tiers consistent
	// without either probeOffsets having to special-case it.
	if unsatisfiable(usable) {
		return roaring.New()
	}
	switch len(usable) {
	case 0:
		return nil
	case 1:
		return ix.probeOffsets(usable[0])
	}
	// A composite over the tuple the conjunction pins replaces that whole group of probes
	// with one lookup -- when it is worth it. compositeWins is the cost test: the composite
	// is taken only when it admits no more candidates than the most selective probe it
	// covers, which is what makes it a replacement rather than merely an alternative.
	// Asked BEFORE the selectivity ordering, because a composite that answers the whole
	// conjunction (cm.certainWin) makes the ordering dead work -- there is nothing left to
	// order -- and pricing probes is not free on a high-cardinality attribute.
	cm, hasComposite := ix.compositeMatch(usable)
	if hasComposite && cm.certainWin {
		return cm.candidates() // the composite answers the whole conjunction, exactly
	}
	order, est := indexSelectivityOrder(ix, usable)
	var acc *roaring.Bitmap
	var covered []bool
	if hasComposite && compositeWins(cm, est) {
		acc = cm.candidates()
		if acc.IsEmpty() {
			return acc
		}
		covered = make([]bool, len(usable))
		for _, i := range cm.used {
			covered[i] = true
		}
	}
	for _, i := range order {
		if len(covered) > 0 && covered[i] {
			continue // already answered, exactly, by the composite
		}
		if acc != nil && !worthProbing(est[i], acc.GetCardinality()) {
			continue
		}
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

// indexCompositeCandidates is the readIndex surface of the composite plan: it derives the
// per-probe estimates the cost test needs, so a caller holding only the conjunction can ask,
// and reports the EXACT candidate count the composite would hand the scan along with the
// tuple's identity. EXPLAIN reports that in place of the minimum of the per-attribute
// estimates, which are independent guesses about columns that are typically correlated
// (ClusterId and ProcId maximally so).
func indexCompositeCandidates(ix indexPrimitives, usable []usableProbe) (float64, string, bool) {
	if len(usable) < 2 {
		return 0, "", false
	}
	_, est := indexSelectivityOrder(ix, usable)
	cm, ok := ix.compositeMatch(usable)
	if !ok || !compositeWins(cm, est) {
		return 0, "", false
	}
	return float64(cm.count()), cm.spec.key, true
}

// offsetsPerVerify is how many record offsets can be merged into a candidate bitmap in the
// time it takes to re-verify one candidate record. A roaring merge runs at a few nanoseconds
// per offset; a verify costs a decompress plus a wire-native match, on the order of a
// microsecond. 200 is that ratio, rounded to the conservative side -- it over-claims what a
// probe is worth, so a probe is dropped only when it is clearly not worth building.
const offsetsPerVerify = 200

// worthProbing reports whether intersecting a probe that would merge about `est` record
// offsets can pay for itself against an accumulator already down to `card` candidates.
//
// This is a bound, not a guess. The candidate set is a superset the store re-verifies record
// by record, so dropping a probe is always correct -- it only widens the superset. The MOST
// a probe can save is the entire remaining verify cost, `card` verifies, and only if it
// eliminates every candidate. So once building its bitmap costs more than that, applying it
// cannot pay off even in the best case. `Owner == "alice" && CompletionDate > <t>` is the
// shape that matters: the equality leaves a few hundred candidates, and the range would
// merge most of the segment to remove a handful of them.
//
// est is the planner's estimated candidate count for the probe -- already computed to order
// the probes -- which tracks merge cost better than a posting-list count would: a
// low-cardinality attribute reaches 200k offsets in 60 postings, a unique one in 200k.
//
// The first (most selective) probe always runs; there is no accumulator to price against.
func worthProbing(est float64, card uint64) bool {
	return est < float64(card)*offsetsPerVerify
}

// indexCandidateOffsetsGroups returns the DNF union over groups of each group's candidate
// intersection. Callers pass only prunable plans (every group has a usable probe).
func indexCandidateOffsetsGroups(ix indexPrimitives, groups [][]usableProbe) *roaring.Bitmap {
	if len(groups) == 0 {
		return nil
	}
	// An impossible disjunct contributes nothing to the union; if every one is impossible
	// the union is empty.
	if unsatisfiableGroups(groups) {
		return roaring.New()
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
