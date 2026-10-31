package collections

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// Reindex (re)builds the per-segment value/categorical indexes for every live
// segment, covering all records written so far. It reads only immutable segment
// bytes, so it runs off the write path and does not block writers or compaction.
// Call it on whatever schedule you like: queries use whatever coverage exists and
// full-scan the rest, so Reindex only affects query speed, never results.
//
// Reindex also reconciles segments with the current index configuration: a segment
// indexed before an AddIndex is rebuilt so the new attribute is backfilled, and one
// indexed before a DropIndex is rebuilt (or, if nothing is indexed anymore, its
// index is dropped) so the removed attribute's postings are reclaimed. A whole
// span of segments therefore evolves toward the current spec at whatever cadence
// the caller reindexes — no write-path or compaction coupling.
//
// With Options.IndexBackfillBytes set, rebuilding an already-sealed segment for a
// configuration change is confined to the newest IndexBackfillBytes; BackfillIndexes and
// ReindexAll carry it further.
func (c *Collection) Reindex() {
	// Serialize concurrent reindexers so two do not build and install the same segment's
	// sidecar at once (the eager per-seal reindex vs. the periodic one). Idempotent and
	// cheap when there is nothing new to build.
	c.reindexMu.Lock()
	defer c.reindexMu.Unlock()
	// Reindex nests inside compaction and inside a merge, so this publishes "reindex" while it
	// runs and restores the enclosing phase when it returns -- an operator sees the innermost
	// thing actually running rather than a pass that appears stuck in "compact". Its cumulative
	// total still lands in opm.reindex, via mp.finish.
	mp := c.beginMaint(MaintReindex, 0)
	defer mp.finish()
	spec := c.spec.Load()
	persistent := c.dir != ""
	for _, sh := range c.shards {
		// Snapshot segments + the active append target under the read lock, then build
		// off-lock. Every segment except sh.act is sealed (immutable until compacted), so
		// safe to convert to the pageable mmap sidecar; the active segment stays in-RAM.
		sh.mu.RLock()
		act := sh.act
		sealRAM := sh.sealRAM
		// Pinned below, once the target list is known: reindexing decompresses every record it
		// covers, off the shard lock, and holds only reindexMu -- which does not exclude Compact,
		// a merge or a rotation, any of which reaps (munmaps) a sealed segment.
		var pinned []*segment
		window := c.backfillWindow(sh)
		type target struct {
			seg    *segment
			used   int
			seal   bool // convert to the mmap sidecar after (re)building
			reseal bool // already sidecar-sealed under an older spec: rebuild the sidecar in place
		}
		var tgts []target
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			if mm := seg.msidx.Load(); mm != nil {
				// Already sealed to an immutable mmap sidecar. Its DATA is final, but the
				// index derived from it is not: rebuild the sidecar in place when the index
				// configuration has moved on, so .addindex/.dropindex reach existing
				// segments without the whole-store re-encode a rewrite would cost.
				// Rebuilding an already-sealed segment's sidecar is BACKFILL -- carrying a
				// configuration change to data that already has a working index. That is
				// the one part of a reindex worth bounding: it decompresses every record it
				// covers, while the index it replaces still answers for the attributes it
				// holds. Everything else here is a missing or incomplete index, which is
				// not optional.
				if spec.any() && seg.used > 0 && mm.specGen != spec.gen && (window == nil || window[seg]) {
					tgts = append(tgts, target{seg: seg, used: seg.used, reseal: true})
				}
				continue
			}
			cur := seg.idx.Load()
			if !spec.any() {
				if cur != nil {
					tgts = append(tgts, target{seg: seg, used: seg.used}) // clear a now-orphaned index
				}
				continue
			}
			if seg.used == 0 {
				continue
			}
			sealable := seg != act && (persistent || sealRAM)
			// Rebuild when the index is missing, behind the write watermark, or built under
			// an older spec generation; otherwise a current-but-unsealed sealable segment
			// still needs converting.
			if cur == nil || int(cur.upto) < seg.used || cur.specGen != spec.gen {
				tgts = append(tgts, target{seg: seg, used: seg.used, seal: sealable})
			} else if sealable {
				tgts = append(tgts, target{seg: seg, used: seg.used, seal: true})
			}
		}
		for _, t := range tgts {
			t.seg.pin()
			pinned = append(pinned, t.seg)
		}
		sh.mu.RUnlock()
		// Held until Reindex returns rather than released per target. A pin only DEFERS a concurrent
		// compaction's reap, so the cost is that replaced segment files linger for the rest of this
		// reindex; releasing them earlier would mean restructuring a loop whose body branches with
		// continue, which is not worth doing in a correctness fix.
		defer unpinAll(pinned)
		for _, t := range tgts {
			if t.reseal {
				c.reindexSealed(sh, t.seg, spec)
				continue
			}
			if !spec.any() {
				t.seg.idx.Store(nil)
				continue
			}
			si := t.seg.idx.Load()
			if si == nil || int(si.upto) < t.used || si.specGen != spec.gen {
				si = buildSegIndex(c, t.seg, t.used, spec)
				t.seg.idx.Store(si)
			}
			// Sealed segment: move its index off the heap into the mmap sidecar (reclaimable,
			// GC-invisible) -- a file sidecar for a persistent segment, an anonymous mapping
			// for a RAM one. Best-effort; on failure the in-RAM index stays.
			if t.seal {
				if persistent && t.seg.keyIdx.Load() != nil {
					// Already sealed to a sidecar that carries no attribute index -- sealed
					// while nothing was indexed, or by a path that seals the key index before
					// building postings. sealSegmentIndex is a no-op once a key index exists,
					// so writing through it would leave this index on the heap for the life of
					// the process and rebuild it from every record at every open. Rewrite the
					// sidecar with the attribute section instead.
					c.reindexSealedFile(sh, t.seg, si)
				} else if persistent {
					c.sealSegmentIndex(t.seg, si)
				} else {
					c.sealSegmentIndexAnon(t.seg, si)
				}
			}
		}
	}
	// Realize the pageable-directory RAM win now (phase 3): seal any sealed segment that
	// still lacks a key sidecar (a key-only collection never entered the loop above) and
	// evict every sealed segment's keys from the resident directory. Idempotent, so a
	// segment sealed just above is only evicted here, not re-written.
	if persistent {
		for _, sh := range c.shards {
			c.sealAndEvictShard(sh)
		}
		c.persistCTProofs()
	}
}

// usableProbe is a query Probe matched to a configured index (interned attr id,
// normalized value type). The store builds candidate offset bitmaps from these;
// the full query is re-verified per candidate, so any probe omitted only costs
// selectivity.
type usableProbe struct {
	attrID uint32
	cat    bool
	op     string
	svals  []string
	fvals  []float64

	// hiOp/hiVal bound a numeric range probe from above, so a conjunction like
	// `Memory > 1024 && Memory < 4096` plans as ONE two-sided band rather than two
	// half-open probes that each union half the key space before intersecting. Set
	// only when op is ">" or ">=" (coalesceRanges normalizes to lower-bound-first);
	// empty hiOp means the probe is one-sided.
	//
	// The fields are an optimization, never a correctness requirement: a code path
	// that reads only op/fvals still produces a correct candidate superset -- just a
	// wider one -- because dropping the upper bound only widens the range.
	hiOp  string
	hiVal float64

	// unsat marks a probe the planner proved can never hold: an equality whose value list
	// was emptied by a range, or by another equality, on the same attribute (see
	// foldEqualities). An inverted band is proved structurally instead, by emptyBand.
	unsat bool

	// inexact marks a value probe on an integer float64 cannot represent exactly, whose op
	// floatKeyOp widened. Its candidates are a superset; see floatKeyOp.
	inexact bool
}

// twoSided reports whether up carries both bounds of a numeric band.
func (up usableProbe) twoSided() bool { return up.hiOp != "" }

// emptyBand reports whether a coalesced range admits no value whatsoever -- the query said
// `Memory < 1024 && Memory > 2048`, or `Memory > 700 && Memory <= 700`.
//
// This is a property of the PREDICATE, not of any segment's data, which makes it stronger
// than every other skip in the planner: those must re-verify the exception set (records
// whose value is present but not an indexed literal), because the index cannot classify
// them. Here there is nothing to classify. A conjunct must be TRUE for an ad to match, and
// no ClassAd value -- number, string, list, boolean, undefined, or error -- can be both
// below the low bound and above the high one at the same time (three-valued logic makes
// the non-numeric cases ERROR/UNDEFINED, which are not TRUE either). See
// TestEmptyBandSemantics, which pins that across every value shape.
//
// A NaN bound compares false everywhere, so it reports not-empty and the query proceeds
// normally rather than eliminating anything on the strength of a NaN.
func (up usableProbe) emptyBand() bool {
	if !up.twoSided() || len(up.fvals) == 0 {
		return false
	}
	lo, hi := up.fvals[0], up.hiVal
	if lo > hi {
		return true
	}
	// A single-point band is satisfiable only when BOTH bounds include the point.
	return lo == hi && (up.op == ">" || up.hiOp == "<")
}

// neverMatches reports whether the planner has proved this probe cannot hold for any
// record -- structurally (an inverted band) or by folding (an equality left with no
// admissible value).
func (up usableProbe) neverMatches() bool { return up.unsat || up.emptyBand() }

// isEqOp reports whether op pins the value to the probe's listed set, so fvals enumerates
// every value a matching record can hold for the attribute. `!=` is deliberately excluded:
// it excludes one value rather than pinning the rest.
func isEqOp(op string) bool { return op == "==" || op == "in" }

// unsatisfiable reports whether a conjunction can never be satisfied, because one of its
// probes was proved impossible. Every probe in a group must hold, so one impossible
// conjunct empties the group -- no segment need be opened at all.
func unsatisfiable(usable []usableProbe) bool {
	for _, up := range usable {
		if up.neverMatches() {
			return true
		}
	}
	return false
}

// unsatisfiableGroups reports whether a DNF plan can never be satisfied. A union is empty
// only when EVERY disjunct is, so one satisfiable group keeps the whole plan alive.
func unsatisfiableGroups(groups [][]usableProbe) bool {
	if len(groups) == 0 {
		return false
	}
	for _, g := range groups {
		if !unsatisfiable(g) {
			return false
		}
	}
	return true
}

// anyUnsatisfiableGroup reports whether SOME disjunct is impossible, i.e. whether dropping
// the dead ones would change the plan at all.
func anyUnsatisfiableGroup(groups [][]usableProbe) bool {
	for _, g := range groups {
		if unsatisfiable(g) {
			return true
		}
	}
	return false
}

// isRangeOp reports whether op is an indexable numeric range comparison.
func isRangeOp(op string) bool {
	switch op {
	case "<", "<=", ">", ">=":
		return true
	}
	return false
}

// coalesceRanges merges the range probes of a conjunction that constrain the same value
// attribute from both sides into a single two-sided band probe. `Memory > 1024 && Memory
// < 4096` otherwise plans as two probes, each of which unions the postings of half the
// attribute's key space (potentially every record in the segment) only for the
// intersection to throw most of it away; a band scans just the keys inside it.
//
// Repeated bounds on one side tighten (`Memory > 1024 && Memory > 2048` keeps > 2048).
// Probes on other attributes, categorical probes, and non-range operators pass through
// untouched and keep their input order, so the plan stays deterministic.
// coalesceRanges and foldEqualities compact and rewrite `out` IN PLACE. That is safe only
// because both run inside planIndex, on a slice it built for this call and has not yet
// returned -- a plan becomes shared (one goroutine per shard, see indexedMatches) only once
// planning is done. Do not call either on a slice a caller already holds.
func coalesceRanges(out []usableProbe) []usableProbe {
	// Fast path: nothing to merge unless two range probes share an attribute.
	if !hasMergeableRanges(out) {
		return out
	}
	// lo/hi hold the tightest bound seen per attribute, at the slot of its first range
	// probe; later range probes on that attribute are folded in and dropped.
	slot := make(map[uint32]int, len(out))
	merged := out[:0]
	for _, up := range out {
		if up.cat || !isRangeOp(up.op) {
			merged = append(merged, up)
			continue
		}
		i, seen := slot[up.attrID]
		if !seen {
			slot[up.attrID] = len(merged)
			merged = append(merged, up)
			continue
		}
		merged[i] = mergeRange(merged[i], up)
	}
	return merged
}

// hasMergeableRanges reports whether two range probes in the conjunction target the same
// value attribute (the only case coalesceRanges changes anything).
func hasMergeableRanges(out []usableProbe) bool {
	for i, a := range out {
		if a.cat || !isRangeOp(a.op) {
			continue
		}
		for _, b := range out[i+1:] {
			if !b.cat && isRangeOp(b.op) && b.attrID == a.attrID {
				return true
			}
		}
	}
	return false
}

// mergeRange folds range probe b into a (same attribute), keeping the tightest bound on
// each side and normalizing to lower-bound-first. A probe that ends up with only an upper
// bound stays one-sided in "<"/"<=" form.
func mergeRange(a, b usableProbe) usableProbe {
	loOp, loVal, hasLo := rangeLow(a)
	hiOp, hiVal, hasHi := rangeHigh(a)
	if op, v, ok := rangeLow(b); ok {
		// Tighter lower bound wins; at equal values the exclusive ">" wins.
		if !hasLo || v > loVal || (v == loVal && op == ">") {
			loOp, loVal, hasLo = op, v, true
		}
	}
	if op, v, ok := rangeHigh(b); ok {
		if !hasHi || v < hiVal || (v == hiVal && op == "<") {
			hiOp, hiVal, hasHi = op, v, true
		}
	}
	inexact := a.inexact || b.inexact
	switch {
	case hasLo && hasHi:
		return usableProbe{attrID: a.attrID, op: loOp, fvals: []float64{loVal}, hiOp: hiOp, hiVal: hiVal, inexact: inexact}
	case hasLo:
		return usableProbe{attrID: a.attrID, op: loOp, fvals: []float64{loVal}, inexact: inexact}
	default:
		return usableProbe{attrID: a.attrID, op: hiOp, fvals: []float64{hiVal}, inexact: inexact}
	}
}

// rangeLow / rangeHigh decompose a (possibly two-sided) range probe into its bounds.
func rangeLow(up usableProbe) (op string, val float64, ok bool) {
	if up.op == ">" || up.op == ">=" {
		return up.op, up.fvals[0], true
	}
	return "", 0, false
}

func rangeHigh(up usableProbe) (op string, val float64, ok bool) {
	if up.hiOp != "" {
		return up.hiOp, up.hiVal, true
	}
	if up.op == "<" || up.op == "<=" {
		return up.op, up.fvals[0], true
	}
	return "", 0, false
}

// rangeMerges maps a value attribute to the single range probe the planner coalesced its
// range conjuncts into. coalesceRanges leaves at most one range probe per attribute, so
// the mapping is well defined. nil when the conjunction has no range probe.
func rangeMerges(usable []usableProbe) map[uint32]usableProbe {
	var merges map[uint32]usableProbe
	for _, up := range usable {
		if up.cat || !foldable(up.op) {
			continue
		}
		if merges == nil {
			merges = make(map[uint32]usableProbe, 1)
		}
		merges[up.attrID] = up
	}
	return merges
}

// foldable reports whether an op participates in the per-attribute value folding, and so
// whether an explain should attribute its conjunct to the folded probe.
func foldable(op string) bool { return isRangeOp(op) || isEqOp(op) }

// applyMerge redirects an explain's selectivity estimate for a range or equality conjunct to
// the probe the planner actually runs for that attribute. A conjunct that IS that probe
// passes through unmarked; one that was folded into it -- the other half of a band, a looser
// same-side bound the tighter one subsumed, or a range an equality absorbed -- is marked,
// because the numbers beside it then describe the folded probe rather than the conjunct's
// own reach.
func applyMerge(merges map[uint32]usableProbe, up usableProbe, pe *ProbeExplain) usableProbe {
	if merges == nil || up.cat || !foldable(up.op) {
		return up
	}
	m, ok := merges[up.attrID]
	if !ok || sameProbe(m, up) {
		return up
	}
	pe.Coalesced = true
	return m
}

// sameProbe reports whether two value probes express the identical constraint.
func sameProbe(a, b usableProbe) bool {
	if a.op != b.op || a.hiOp != b.hiOp || a.hiVal != b.hiVal || len(a.fvals) != len(b.fvals) {
		return false
	}
	for i := range a.fvals {
		if a.fvals[i] != b.fvals[i] {
			return false
		}
	}
	return true
}

// foldEqualities folds the value constraints on one attribute together: an equality (or
// membership) probe absorbs the attribute's range probe, and several equalities intersect
// into one. `Memory == 2048 && Memory > 1024` otherwise runs two probes, the second unioning
// the postings of every key above 1024 -- most of the segment -- only for the intersection
// to reduce it to the one key the equality already named.
//
// Absorbing is exact, not merely a sound superset. A record holds ONE value for the
// attribute, so its posting list post[v] lies wholly inside the range's key union when v
// satisfies the range and wholly outside it otherwise; and both probes union in the same
// exception set. So intersecting the two probes yields exactly the equality probe narrowed
// to the values the range admits, which is what this leaves behind.
//
// When nothing survives the narrowing, the conjunction cannot hold for any record -- an ad's
// attribute has a single value, and it cannot both be one of the named values and outside
// them. That is recorded as unsat and eliminated by the same machinery as an inverted band
// (see unsatisfiable); TestEqualityFoldSemantics pins it across the value space.
//
// Runs after coalesceRanges, so each attribute has at most one range probe left to absorb.
func foldEqualities(out []usableProbe) []usableProbe {
	if !hasFoldableEqualities(out) {
		return out
	}
	eqAt := make(map[uint32]int, 2) // attr -> index of the surviving equality probe
	drop := make([]bool, len(out))
	for i := range out {
		if out[i].cat || !isEqOp(out[i].op) {
			continue
		}
		if j, seen := eqAt[out[i].attrID]; seen {
			out[j] = keepValues(out[j], func(v float64) bool { return containsFloat(out[i].fvals, v) })
			drop[i] = true
			continue
		}
		eqAt[out[i].attrID] = i
	}
	for i := range out {
		if out[i].cat || !isRangeOp(out[i].op) {
			continue
		}
		j, ok := eqAt[out[i].attrID]
		if !ok {
			continue // no equality on this attribute: the range probe stands on its own
		}
		rng := out[i]
		out[j] = keepValues(out[j], func(v float64) bool { return rangeAdmits(rng, v) })
		drop[i] = true
	}
	kept := out[:0]
	for i := range out {
		if !drop[i] {
			kept = append(kept, out[i])
		}
	}
	return kept
}

// hasFoldableEqualities reports whether some value attribute carries an equality probe
// alongside another equality or a range probe -- the only case foldEqualities changes.
func hasFoldableEqualities(out []usableProbe) bool {
	var eq, other map[uint32]struct{}
	for _, up := range out {
		if up.cat {
			continue
		}
		switch {
		case isEqOp(up.op):
			if _, dup := eq[up.attrID]; dup {
				return true
			}
			if _, ok := other[up.attrID]; ok {
				return true
			}
			if eq == nil {
				eq = map[uint32]struct{}{}
			}
			eq[up.attrID] = struct{}{}
		case isRangeOp(up.op):
			if _, ok := eq[up.attrID]; ok {
				return true
			}
			if other == nil {
				other = map[uint32]struct{}{}
			}
			other[up.attrID] = struct{}{}
		}
	}
	return false
}

// keepValues narrows an equality probe to the values satisfying admits, marking it unsat
// when none do. The retained slice is fresh, so no other probe's values are disturbed.
func keepValues(up usableProbe, admits func(float64) bool) usableProbe {
	keep := make([]float64, 0, len(up.fvals))
	for _, v := range up.fvals {
		if admits(v) {
			keep = append(keep, v)
		}
	}
	up.fvals = keep
	up.unsat = len(keep) == 0
	return up
}

// rangeAdmits reports whether v satisfies a (possibly two-sided) range probe's bounds.
func rangeAdmits(rng usableProbe, v float64) bool {
	if len(rng.fvals) == 0 || !cmpFloat(rng.op, v, rng.fvals[0]) {
		return false
	}
	return rng.hiOp == "" || cmpFloat(rng.hiOp, v, rng.hiVal)
}

func containsFloat(vals []float64, v float64) bool {
	for _, w := range vals {
		if w == v {
			return true
		}
	}
	return false
}

// rangeSpan returns the [from,to) index window of a sorted ascending key run that a range
// probe admits. Both bounds are applied, so a two-sided probe touches only the keys inside
// its band.
func rangeSpan(up usableProbe, n int, lowerBound, upperBound func(t float64) int) (from, to int) {
	from, to = 0, n
	switch up.op {
	case ">":
		from = upperBound(up.fvals[0])
	case ">=":
		from = lowerBound(up.fvals[0])
	case "<":
		to = lowerBound(up.fvals[0])
	case "<=":
		to = upperBound(up.fvals[0])
	}
	switch up.hiOp {
	case "<":
		to = lowerBound(up.hiVal)
	case "<=":
		to = upperBound(up.hiVal)
	}
	if to < from {
		to = from // an empty band (lo above hi): no keys match
	}
	return from, to
}

// planIndex matches the query's probes against the configured indexes. Empty means
// no index-usable constraint (the store full-scans).
// ProbeExplain describes how one index-satisfiable conjunct of a query relates
// to the configured indexes.
type ProbeExplain struct {
	Attr    string `json:"attr"`
	Op      string `json:"op"`
	Indexed bool   `json:"indexed"`        // this probe can use an index to prune
	Kind    string `json:"kind,omitempty"` // "categorical" | "value" | "" (attr not indexed)

	// Selectivity and EstCandidates are the planner's estimate of how much this
	// probe prunes, present only when Indexed: EstCandidates is the estimated
	// number of ads the index would visit for this probe (summed over the segment
	// indexes' selectivity stats), and Selectivity is that as a fraction of the
	// total (lower is more selective). HasSelectivity distinguishes a real 0 from
	// "not estimated".
	HasSelectivity bool    `json:"hasSelectivity"`
	Selectivity    float64 `json:"selectivity,omitempty"`
	EstCandidates  int64   `json:"estCandidates,omitempty"`

	// Coalesced marks a conjunct the planner folded into another probe on the same value
	// attribute: the opposite bound (`Memory > 1024 && Memory < 4096` becomes one two-sided
	// probe), a tighter same-side bound that subsumes it (`Memory > 1024 && Memory > 512`
	// probes only `> 1024`), or a range an equality absorbed (`Memory == 2048 && Memory >
	// 1024` probes only the equality). Selectivity/EstCandidates then describe the folded
	// probe, not this conjunct's own reach; the conjunct that IS the probe is left unmarked.
	Coalesced bool `json:"coalesced,omitempty"`
}

// GroupExplain is one disjunct of a disjunctive plan: its index-satisfiable conjuncts and their
// index status, as ProbeExplain reports them for a conjunctive one.
type GroupExplain struct {
	Probes      []ProbeExplain `json:"probes"`
	IndexUsable int            `json:"indexUsable"`
	// Unsatisfiable marks a disjunct whose indexed conjuncts contradict each other: it
	// contributes nothing to the union.
	Unsatisfiable bool `json:"unsatisfiable,omitempty"`
}

// QueryExplain is a description of how the store would execute a query, for the
// diagnostic ".explain" command -- what the planner sees and the access path it
// would choose.
type QueryExplain struct {
	// Native reports wire-native evaluation: the query reads scalar-literal
	// attributes directly from the encoded ads, building no ClassAd per ad.
	Native bool `json:"native"`
	// Probes are the query's index-satisfiable conjuncts and their index status.
	Probes []ProbeExplain `json:"probes"`
	// IndexUsable is how many probes can prune via an index.
	IndexUsable int `json:"indexUsable"`
	// Plan is the chosen access path: "indexed" (visit index candidates), "indexed-union"
	// (visit the union of the candidates of each disjunct in Groups), "parallel-scan",
	// "serial-scan" (full scan), or "empty" (a contradictory conjunct makes the query
	// unsatisfiable, so no records are visited at all).
	Plan string `json:"plan"`
	// Parallelism is the configured per-query worker cap; Shards is the shard count.
	Parallelism int `json:"parallelism"`
	Shards      int `json:"shards"`
	// TotalAds is the live ad count, the denominator for probe selectivity.
	TotalAds int `json:"totalAds"`
	// Groups are, for a filter whose top-level ORs (nested ORs distributed) give more than one
	// disjunct, each disjunct's index-satisfiable conjuncts. Set whether or not the plan reads
	// through them: an "indexed-union" plan does, and a scan's Reason says why it does not.
	Groups []GroupExplain `json:"groups,omitempty"`
	// EstCandidates is, for an indexed plan, the estimated records the index hands the scan:
	// per segment, the smallest of its probes' posting estimates, summed. IndexSegments is how
	// many segments answer from an index, IndexSkippedSegments how many the index proves hold
	// no candidate, and UnindexedSegments how many no index covers (an unsealed segment, or
	// one sealed before the attribute was indexed), which are walked in full.
	EstCandidates        int64 `json:"estCandidates,omitempty"`
	IndexSegments        int   `json:"indexSegments,omitempty"`
	IndexSkippedSegments int   `json:"indexSkippedSegments,omitempty"`
	UnindexedSegments    int   `json:"unindexedSegments,omitempty"`
	// ZonePrunedSegments is how many segments the zone maps rule out before any record or
	// posting is read, on either plan. An indexed plan's segment counts above cover only the
	// segments left after it.
	ZonePrunedSegments int `json:"zonePrunedSegments,omitempty"`
	// Reason says, in a sentence, why the plan is what it is: which index probe and how many
	// candidates, or why the scan won.
	Reason string `json:"reason,omitempty"`
	// CurrentTimeFolded reports a query that reads CurrentTime and has a folded form
	// (vm.Query.Folded): segments proven to hold no record defining CurrentTime evaluate the folded
	// form and may be pruned by its probes. CurrentTimeUnprovenSegments counts the segments not (yet)
	// proven, which evaluate the query as written and are never pruned by a probe on CurrentTime.
	CurrentTimeFolded           bool `json:"currentTimeFolded,omitempty"`
	CurrentTimeUnprovenSegments int  `json:"currentTimeUnprovenSegments,omitempty"`
}

// ExplainQuery reports how the store would execute q: which of its conjuncts are
// index-usable, and the resulting access path (indexed / parallel scan / serial
// scan). It performs no I/O beyond reading the current index spec.
func (c *Collection) ExplainQuery(q *vm.Query) QueryExplain {
	probes := q.Probes()
	usable := c.planIndex(probes)
	total := c.Len()
	ex := QueryExplain{
		Native:      q.Native(),
		IndexUsable: len(usable),
		Parallelism: c.queryPar,
		Shards:      len(c.shards),
		TotalAds:    total,
	}
	var indexedAttrs []string // the usable probes' attributes, as the query spells them
	ex.Probes, indexedAttrs = c.explainProbes(probes, usable, total)
	scan := "serial-scan"
	if c.queryPar > 1 && !c.reverseScan {
		scan = "parallel-scan" // a newest-first archive always walks serially (see queryAs)
	}
	// A disjunctive plan, when the read takes it (planReadQuery): the union of its groups' candidates.
	var dnfReason string
	if plan := q.ProbePlan(); len(plan) > 1 {
		if c.explainGroups(q, plan, &ex) {
			return ex
		}
		dnfReason = ex.Reason
		ex.Reason = ""
	}
	frac, gated := c.overReadGate(usable)
	switch {
	case unsatisfiable(usable):
		// A contradictory conjunct (`Memory < 1024 && Memory > 2048`) makes the whole
		// query impossible, so no shard is even opened. Naming it is the point: this is
		// the answer to "why does my constraint return nothing".
		ex.Plan = "empty"
		ex.Reason = "the filter's indexed conjuncts contradict each other: nothing can match"
	case gated:
		ex.Plan = scan
		ex.Reason = fmt.Sprintf("full scan: the index on %s would hand the scan about %.0f%% of the records, "+
			"over the %.0f%% at which an index walk stops paying", strings.Join(indexedAttrs, " and "), 100*frac, 100*maxPushdownFrac)
	case len(usable) > 0:
		ex.Plan = "indexed"
		c.explainIndexSegments(q, usable, &ex)
		ex.Reason = fmt.Sprintf("index probe on %s: %d of %d segments answer from postings, ~%d candidate records",
			strings.Join(indexedAttrs, " and "), ex.IndexSegments,
			ex.IndexSegments+ex.IndexSkippedSegments+ex.UnindexedSegments+ex.ZonePrunedSegments, ex.EstCandidates)
		if ex.ZonePrunedSegments > 0 {
			ex.Reason += fmt.Sprintf("; %d segments ruled out by their zone maps", ex.ZonePrunedSegments)
		}
		if ex.IndexSkippedSegments > 0 {
			ex.Reason += fmt.Sprintf("; %d segments ruled out by their index", ex.IndexSkippedSegments)
		}
		if ex.UnindexedSegments > 0 {
			ex.Reason += fmt.Sprintf("; %d segments have no index on it and are scanned in full", ex.UnindexedSegments)
		}
	case dnfReason != "":
		ex.Plan = scan
		ex.Reason = dnfReason
	case len(probes) == 0:
		ex.Plan = scan
		ex.Reason = "full scan: the filter has no conjunct an index or zone map can use"
	default:
		ex.Plan = scan
		ex.Reason = "full scan: no conjunct of the filter is on an indexed attribute"
	}
	if ex.Plan == scan {
		if ex.ZonePrunedSegments = c.explainZonePruned(q); ex.ZonePrunedSegments > 0 {
			ex.Reason += fmt.Sprintf("; %d segments ruled out by their zone maps", ex.ZonePrunedSegments)
		}
	}
	if q.Folded() != nil {
		ex.CurrentTimeFolded = true
		ex.CurrentTimeUnprovenSegments = c.ctUnprovenSegments()
		if ex.CurrentTimeUnprovenSegments > 0 {
			ex.Reason += fmt.Sprintf("; %d segments are not proven free of CurrentTime, so they evaluate it as written and are not pruned on it",
				ex.CurrentTimeUnprovenSegments)
		}
	}
	return ex
}

// explainProbes describes probes, the conjuncts of one conjunction, against the configured
// indexes: each probe's index status and selectivity (with usable, its planned form, supplying the
// range merges), and the attributes of the usable ones, as the query spells them.
func (c *Collection) explainProbes(probes []vm.Probe, usable []usableProbe, total int) ([]ProbeExplain, []string) {
	merges := rangeMerges(usable)
	var out []ProbeExplain
	var indexedAttrs []string
	for _, p := range probes {
		pe := ProbeExplain{Attr: p.Attr, Op: p.Op}
		var up usableProbe
		var isUsable bool
		pe.Indexed, pe.Kind, up, isUsable = c.probeIndexKind(p)
		if isUsable && !slices.ContainsFunc(indexedAttrs, func(a string) bool { return strings.EqualFold(a, p.Attr) }) {
			indexedAttrs = append(indexedAttrs, p.Attr)
		}
		up = applyMerge(merges, up, &pe)
		if isUsable {
			if cand, covered := c.estimateCandidates(up); covered {
				pe.HasSelectivity = true
				pe.EstCandidates = clampEst(cand, total)
				if total > 0 {
					pe.Selectivity = math.Min(1, cand/float64(total))
				}
			}
		}
		out = append(out, pe)
	}
	return out, indexedAttrs
}

// explainGroups fills ex for the multi-group DNF plan of q. It reports true when the read takes the
// disjunctive path (dnfReadPlan) -- ex is then complete, as an "indexed-union" or an "empty" plan --
// and false when it does not, leaving ex.Reason saying why for the caller's scan verdict.
func (c *Collection) explainGroups(q *vm.Query, plan []vm.ProbeGroup, ex *QueryExplain) bool {
	var attrs []string // per disjunct, its usable attributes joined with " and "
	unusable := 0      // the first disjunct with no index-usable probe, 1-based; 0 for none
	for i, g := range plan {
		u := c.planIndex(g.Probes)
		ge := GroupExplain{IndexUsable: len(u), Unsatisfiable: len(u) > 0 && unsatisfiable(u)}
		var ga []string
		ge.Probes, ga = c.explainProbes(g.Probes, u, ex.TotalAds)
		ex.Groups = append(ex.Groups, ge)
		attrs = append(attrs, strings.Join(ga, " and "))
		if len(u) == 0 && unusable == 0 {
			unusable = i + 1
		}
	}
	if unusable > 0 {
		ex.Reason = fmt.Sprintf("full scan: disjunct %d of %d has no conjunct an index can use, so it could match any record", unusable, len(plan))
		return false
	}
	groups, ok := c.dnfReadPlan(plan)
	if !ok {
		all, _ := c.planIndexGroups(plan)
		frac, _ := c.planFrac(all)
		ex.Reason = fmt.Sprintf("full scan: the index union over %d disjuncts would hand the scan about %.0f%% of the records, "+
			"over the %.0f%% at which an index walk stops paying", len(plan), 100*frac, 100*maxPushdownFrac)
		return false
	}
	if unsatisfiableGroups(groups) {
		ex.Plan = "empty"
		ex.Reason = "the indexed conjuncts of every disjunct of the filter contradict each other: nothing can match"
		return true
	}
	ex.Plan = "indexed-union"
	c.explainIndexSegmentsGroups(q, plan, groups, ex)
	ex.Reason = fmt.Sprintf("index union over %d disjuncts (%s): %d of %d segments answer from postings, ~%d candidate records",
		len(plan), strings.Join(attrs, " | "), ex.IndexSegments,
		ex.IndexSegments+ex.IndexSkippedSegments+ex.UnindexedSegments+ex.ZonePrunedSegments, ex.EstCandidates)
	if ex.ZonePrunedSegments > 0 {
		ex.Reason += fmt.Sprintf("; %d segments ruled out by their zone maps", ex.ZonePrunedSegments)
	}
	if ex.IndexSkippedSegments > 0 {
		ex.Reason += fmt.Sprintf("; %d segments ruled out by their index", ex.IndexSkippedSegments)
	}
	if ex.UnindexedSegments > 0 {
		ex.Reason += fmt.Sprintf("; %d segments have no index on every disjunct and are scanned in full", ex.UnindexedSegments)
	}
	return true
}

// explainIndexSegmentsGroups is explainIndexSegments for a disjunctive plan: each segment's verdict
// as scanShardIndexedGroups reaches it -- zone-pruned (zoneWalkGroups), walked in full
// (groupsWindowIndex), proven empty by its index (skipsPrefixGroups), or answered from postings --
// and the estimated candidates, per segment the sum over disjuncts of each one's smallest probe
// estimate.
func (c *Collection) explainIndexSegmentsGroups(q *vm.Query, plan []vm.ProbeGroup, groups [][]usableProbe, ex *QueryExplain) {
	probes, ct, zoned := q.Probes(), q.ReadsCurrentTime(), c.hasZones.Load()
	var cand float64
	for _, sh := range c.shards {
		_, wins := sh.snapshot()
		live := sh.liveFractions(wins)
		for i, w := range wins {
			if zoned && (c.zoneExcludes(w, probes, ct) || c.zoneExcludesGroups(w, plan, ct)) {
				ex.ZonePrunedSegments++
				continue
			}
			si := groupsWindowIndex(w, groups, ct)
			switch {
			case si == nil:
				ex.UnindexedSegments++
			case skipsPrefixGroups(si, groups):
				ex.IndexSkippedSegments++
			default:
				ex.IndexSegments++
				var sum float64
				for _, g := range groups {
					least := math.Inf(1)
					for _, up := range g {
						least = math.Min(least, si.estCandidates(up))
					}
					sum += least
				}
				cand += sum * live[i]
			}
		}
		releaseWindows(wins)
	}
	ex.EstCandidates = clampEst(cand, ex.TotalAds)
}

// planRead is the index plan a read uses: planIndex, unless the index would hand the scan
// nearly every record (overReadGate), when walking the records in order is cheaper than
// building and iterating the candidate set -- the candidates are a subset of the same walk,
// so the scan never visits a record the index would have spared it.
func (c *Collection) planRead(probes []vm.Probe) []usableProbe {
	usable := c.planIndex(probes)
	if _, gated := c.overReadGate(usable); gated {
		return nil
	}
	return usable
}

// overReadGate reports the estimated fraction of records usable's candidates cover and
// whether that is over maxPushdownFrac. Not gated when there is nothing to estimate from.
//
// Only an append-only archive is gated. The estimate reads every segment's index stats, which
// is nothing beside an archive read but would be a real share of a point lookup on a mutable
// table, where the candidate walk was never the slow part.
func (c *Collection) overReadGate(usable []usableProbe) (frac float64, gated bool) {
	if len(usable) == 0 || unsatisfiable(usable) || !c.appendOnly() {
		return 0, false
	}
	frac, ok := c.planFrac([][]usableProbe{usable})
	return frac, ok && frac > maxPushdownFrac
}

// explainIndexSegments fills ex's per-segment index breakdown for an indexed plan of q, after
// the zone pruning scanShardIndexed applies first.
func (c *Collection) explainIndexSegments(q *vm.Query, usable []usableProbe, ex *QueryExplain) {
	probes, ct, zoned := q.Probes(), q.ReadsCurrentTime(), c.hasZones.Load()
	var cand float64
	for _, sh := range c.shards {
		_, wins := sh.snapshot()
		live := sh.liveFractions(wins)
		for i, w := range wins {
			si := w.seg.readIdx()
			switch {
			case zoned && c.zoneExcludes(w, probes, ct):
				ex.ZonePrunedSegments++
			case si == nil || !si.covers(usable):
				ex.UnindexedSegments++
			case si.skipsPrefix(usable):
				ex.IndexSkippedSegments++
			default:
				ex.IndexSegments++
				least := math.Inf(1)
				for _, up := range usable {
					least = math.Min(least, si.estCandidates(up))
				}
				cand += least * live[i]
			}
		}
		releaseWindows(wins)
	}
	ex.EstCandidates = clampEst(cand, ex.TotalAds)
}

// explainZonePruned counts the segments a scan of q skips on their zone maps (zoneWalk).
func (c *Collection) explainZonePruned(q *vm.Query) int {
	probes := q.Probes()
	if len(probes) == 0 || !c.hasZones.Load() {
		return 0
	}
	ct, n := q.ReadsCurrentTime(), 0
	for _, sh := range c.shards {
		_, wins := sh.snapshot()
		for _, w := range wins {
			if c.zoneExcludes(w, probes, ct) {
				n++
			}
		}
		releaseWindows(wins)
	}
	return n
}

// probeIndexKind reports whether a probe's attribute is indexed and, if so, its
// index kind, whether this probe's operator can use it (mirrors planIndex's
// per-probe decision), and the resolved usableProbe when usable.
func (c *Collection) probeIndexKind(p vm.Probe) (indexed bool, kind string, up usableProbe, usable bool) {
	spec := c.spec.Load()
	if !spec.any() {
		return false, "", usableProbe{}, false
	}
	var id uint32
	var ok bool
	if spec.inline {
		id, ok = spec.nameToID[strings.ToLower(p.Attr)]
	} else {
		id, ok = c.intern.LookupID(p.Attr)
	}
	if !ok {
		return false, "", usableProbe{}, false
	}
	if _, isCat := spec.cat[id]; isCat {
		up, usable = catUsable(id, p)
		return usable, "categorical", up, usable
	}
	if _, isVal := spec.val[id]; isVal {
		up, usable = valUsable(id, p)
		return usable, "value", up, usable
	}
	return false, "", usableProbe{}, false
}

// estimateCandidates sums, over every segment index that covers up, the segment's
// estimated candidate count for the probe -- the same selectivity estimate the
// planner uses to order intersections. covered reports whether any built segment
// index contributed (false when the matching records are only in an unsealed
// segment with no stats yet, so no estimate is available). Approximate: it counts
// the indexed prefix (not the un-indexed tail).
//
// A segment index counts every record it posted, and in a mutable table most of
// those are versions a later write has superseded: a jobs table of 54k ads
// estimated `JobStatus == 2` at 405k. So each segment's count is scaled by the
// share of the segment that is still live (liveFractions) -- by bytes, which the
// segment keeps exact, standing in for records, which it does not count.
func (c *Collection) estimateCandidates(up usableProbe) (cand float64, covered bool) {
	single := []usableProbe{up}
	for _, sh := range c.shards {
		_, wins := sh.snapshot()
		live := sh.liveFractions(wins)
		for i, w := range wins {
			if si := w.seg.readIdx(); si != nil && si.covers(single) {
				cand += si.estCandidates(up) * live[i]
				covered = true
			}
		}
		releaseWindows(wins)
	}
	return cand, covered
}

// liveFractions returns, for each window, the share of its segment's bytes that
// belong to records no later write has superseded: 1 for a segment of current
// records, 0 for one holding only history. The segment tracks its dead bytes as
// records are superseded, under the shard lock, which is taken here to read them.
func (sh *shard) liveFractions(wins []segWindow) []float64 {
	out := make([]float64, len(wins))
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	for i, w := range wins {
		used := int64(w.seg.used)
		if live := used - w.seg.dead; used > 0 && live > 0 {
			out[i] = float64(live) / float64(used)
		}
	}
	return out
}

// planIndexGroups plans a DNF probe plan (from vm.ProbePlan): each group's probes are
// matched against the configured indexes. prunable is false if any group has no
// index-usable probe -- an unconstrained disjunct means the union covers everything,
// so the caller must full-scan instead. A single-group plan reduces to planIndex.
func (c *Collection) planIndexGroups(plan []vm.ProbeGroup) (groups [][]usableProbe, prunable bool) {
	if len(plan) == 0 {
		return nil, false
	}
	groups = make([][]usableProbe, 0, len(plan))
	for _, g := range plan {
		u := c.planIndex(g.Probes)
		if len(u) == 0 {
			return nil, false // this disjunct is unconstrained: the union can't prune
		}
		groups = append(groups, u)
	}
	return groups, true
}

func (c *Collection) planIndex(probes []vm.Probe) []usableProbe {
	spec := c.spec.Load()
	if !spec.any() {
		return nil
	}
	var out []usableProbe
	for _, p := range probes {
		var id uint32
		var ok bool
		if spec.inline {
			id, ok = spec.nameToID[strings.ToLower(p.Attr)]
		} else {
			id, ok = c.intern.LookupID(p.Attr)
		}
		if !ok {
			continue
		}
		if _, isCat := spec.cat[id]; isCat {
			if up, ok := catUsable(id, p); ok {
				out = append(out, up)
			}
			continue
		}
		if _, isVal := spec.val[id]; isVal {
			if up, ok := valUsable(id, p); ok {
				out = append(out, up)
			}
		}
	}
	return foldEqualities(coalesceRanges(out))
}

func catUsable(id uint32, p vm.Probe) (usableProbe, bool) {
	switch p.Op {
	case "present", "absent":
		return usableProbe{attrID: id, cat: true, op: p.Op}, true // presence needs no value
	case "is", "isnt":
		// =?= / =!= are case-sensitive: carry the exact (unfolded) spelling.
		s, ok := catExact(p.Vals[0])
		if !ok {
			return usableProbe{}, false
		}
		return usableProbe{attrID: id, cat: true, op: p.Op, svals: []string{s}}, true
	case "truthy":
		// bare `attr`: matches boolean true. A categorical index posts booleans as
		// "true"/"false" and routes non-booleans to the exception set, so `== true` (which
		// unions the exceptions) is a sound superset the store re-verifies for truthiness.
		return usableProbe{attrID: id, cat: true, op: "==", svals: []string{"true"}}, true
	case "untruthy": // `!attr`: matches boolean false, same superset argument.
		return usableProbe{attrID: id, cat: true, op: "==", svals: []string{"false"}}, true
	case "==", "!=", "in":
	default:
		return usableProbe{}, false // ranges are not indexed for categoricals
	}
	svals := make([]string, 0, len(p.Vals))
	for _, v := range p.Vals {
		s, ok := catFold(v) // fold to match the index key
		if !ok {
			return usableProbe{}, false
		}
		svals = append(svals, s)
	}
	return usableProbe{attrID: id, cat: true, op: p.Op, svals: svals}, true
}

// catExact returns the categorical index key for a probe value in its exact spelling: a
// string as-is, or a boolean as canonical "true"/"false" (matching how indexRecord posts
// booleans). ok is false for values a categorical index cannot key on (numbers, lists).
func catExact(v classad.Value) (string, bool) {
	if s, err := v.StringValue(); err == nil {
		return s, true
	}
	if b, err := v.BoolValue(); err == nil {
		if b {
			return "true", true
		}
		return "false", true
	}
	return "", false
}

// catFold is catExact folded to the case-insensitive bucket key ("true"/"false" are
// already lowercase, so booleans are unaffected).
func catFold(v classad.Value) (string, bool) {
	s, ok := catExact(v)
	if !ok {
		return "", false
	}
	return strings.ToLower(s), true
}

func valUsable(id uint32, p vm.Probe) (usableProbe, bool) {
	op := p.Op
	switch p.Op {
	case "present", "absent":
		return usableProbe{attrID: id, cat: false, op: p.Op}, true // presence needs no value
	case "is":
		// numeric =?= plans as == (a superset: int/real fold into one float key; the
		// store re-verifies). =!= ("isnt") is NOT indexed for values -- int/real
		// type-strictness (5 =?= 5.0 is false) means the != posting path would drop the
		// other-typed records =!= must keep -- so it falls through to a scan.
		op = "=="
	case "isnt":
		return usableProbe{}, false
	case "truthy":
		// bare `attr`: truthy iff the value is non-zero (a boolean false and a numeric 0
		// both post at 0.0). `!= 0` unions the exception set, so it is a sound superset the
		// store re-verifies -- and prunes, unlike categorical == true which would miss
		// numeric-nonzero-other-than-1.
		return usableProbe{attrID: id, cat: false, op: "!=", fvals: []float64{0}}, true
	case "untruthy": // `!attr`: falsy iff the value is 0.
		return usableProbe{attrID: id, cat: false, op: "==", fvals: []float64{0}}, true
	case "==", "!=", "in", "<", "<=", ">", ">=":
	default:
		return usableProbe{}, false
	}
	fvals := make([]float64, 0, len(p.Vals))
	for _, v := range p.Vals {
		f, ok := numericFloat(v)
		if !ok {
			return usableProbe{}, false
		}
		fvals = append(fvals, f)
	}
	op, inexact, ok := floatKeyOp(op, p.Vals)
	if !ok {
		return usableProbe{}, false
	}
	return usableProbe{attrID: id, cat: false, op: op, fvals: fvals, inexact: inexact}, true
}

// floatExactLimit bounds the integers whose float64 image identifies them: every integer in
// (-2^53, 2^53) converts exactly, and no other integer converts to the same float64. 2^53
// itself is excluded because 2^53+1 rounds onto it.
const floatExactLimit = 1 << 53

// intBeyondFloat reports whether v is an integer that float64 cannot keep distinct from its
// neighbors.
func intBeyondFloat(v classad.Value) bool {
	i, err := v.IntValue()
	return err == nil && (i >= floatExactLimit || i <= -floatExactLimit)
}

// floatKeyOp adapts a numeric probe to structures that hold values as float64 -- value-index
// keys, segment stats, segment and block zones -- and reports whether the probe is inexact there.
//
// Comparing float64(x) with float64(c) gives exactly what the comparison operators give
// whenever the probe's integers lie in (-2^53, 2^53): a real operand is compared as a double by
// the operators too, and an integer x on either side of such a c lands on the same side of it
// after conversion, because conversion is monotonic and c and its neighbors c-1 and c+1 are all
// exact. For a larger integer c, distinct integers share c's float64, so x > c only implies
// float64(x) >= float64(c). The strict orderings are therefore widened to their non-strict forms,
// which keeps every match; == and in already keep every match of the shared key; and != is
// refused (ok=false), since excluding the shared key would drop integers that are not c.
//
// A widened probe is a candidate superset that the caller must re-verify. A path that answers
// from the probe without re-verifying must refuse an inexact probe outright.
func floatKeyOp(op string, vals []classad.Value) (newOp string, inexact, ok bool) {
	for _, v := range vals {
		if intBeyondFloat(v) {
			inexact = true
			break
		}
	}
	if !inexact {
		return op, false, true
	}
	switch op {
	case ">":
		return ">=", true, true
	case "<":
		return "<=", true, true
	case "!=":
		return "", true, false
	// A negated ordering probe widens through the comparison its negation IS over a NaN-free
	// range: `!(x <= c)` is `x > c` there, which widens to `x >= c`, i.e. `!(x < c)`. The other
	// two already negate to a non-strict comparison (`!(x < c)` is `x >= c`), so the fallthrough
	// leaves them alone. Only a consumer that has proven NaN-freeness may use either (see
	// negOrdering); one that evaluates the stored value refuses an inexact probe outright.
	case "!<=":
		return "!<", true, true
	case "!>=":
		return "!>", true, true
	}
	return op, true, true
}

// negOrderingBase maps a negated-ordering probe operator (vm.Probe's "!<", "!<=", "!>", "!>=") to
// the comparison inside the negation, and reports whether op was one. See vm's negOrdering for
// why a negated ordering comparison is not the flipped one: every comparison with a NaN is false,
// so the negation is TRUE of a NaN where the flipped comparison is false.
func negOrderingBase(op string) (string, bool) {
	switch op {
	case "!<":
		return "<", true
	case "!<=":
		return "<=", true
	case "!>":
		return ">", true
	case "!>=":
		return ">=", true
	}
	return "", false
}

// orderingFlip maps an ordering comparison to the one its negation is equivalent to OVER VALUES
// THAT ARE NOT NaN. Reaching for it means asserting that: on a zone proven Full (every record
// holds a number, so none holds a NaN) or on an integer column, where NaN cannot be stored.
var orderingFlip = map[string]string{"<": ">=", "<=": ">", ">": "<=", ">=": "<"}

// numPredProbe is valUsable for a path that evaluates the probe as a float predicate over each
// stored value and negates the RESULT, rather than looking the probe up in an index.
//
// That is the one place a negated ordering probe needs no extra proof: `!(v <= c)` computed as
// the negation of the float comparison is right for every v, NaN included, and costs the same as
// the comparison. valUsable itself must keep refusing these, because an index posting lookup
// reasons about the comparison's RANGE and a NaN lies outside every range -- so a range scan over
// the postings would silently drop exactly the records the negation keeps.
//
// An integer literal past 2^53 is refused rather than widened: a widened probe is a candidate
// superset, and these callers answer from the probe without re-verifying.
func numPredProbe(id uint32, p vm.Probe) (usableProbe, bool) {
	if _, neg := negOrderingBase(p.Op); !neg {
		return valUsable(id, p)
	}
	if len(p.Vals) != 1 || intBeyondFloat(p.Vals[0]) {
		return usableProbe{}, false
	}
	f, ok := numericFloat(p.Vals[0])
	if !ok {
		return usableProbe{}, false
	}
	return usableProbe{attrID: id, cat: false, op: p.Op, fvals: []float64{f}}, true
}

func numericFloat(v classad.Value) (float64, bool) {
	if f, err := v.NumberValue(); err == nil {
		return f, true
	}
	if b, err := v.BoolValue(); err == nil {
		if b {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// coversGroups reports whether a segment index covers every probe of every group. If
// it does not cover some group, that disjunct is unconstrained for this segment, so
// the caller must full-scan the window rather than use the (incomplete) union.
func (si *segIndex) coversGroups(groups [][]usableProbe) bool { return indexCoversGroups(si, groups) }

// candidateOffsetsGroups returns the union over groups of each group's candidate
// intersection -- the DNF `OR of (AND of probes)`. Callers pass only prunable plans
// (every group has at least one usable probe), so no group widens to everything.
func (si *segIndex) candidateOffsetsGroups(groups [][]usableProbe) *roaring.Bitmap {
	return indexCandidateOffsetsGroups(si, groups)
}

// covers reports whether a segment index has postings for every usable probe's
// attribute (a segment indexed before an attribute was added would not).
func (si *segIndex) covers(usable []usableProbe) bool { return indexCovers(si, usable) }

// coversProbe reports whether this segment indexes one probe's attribute.
func (si *segIndex) coversProbe(up usableProbe) bool {
	if up.cat {
		return si.cat[up.attrID] != nil
	}
	return si.val[up.attrID] != nil
}

// candidateOffsets returns the segment-record offsets satisfying every usable
// probe (a superset; the store re-verifies). Probes are applied most-selective
// first (by the per-attribute stats estimate) so the roaring intersection shrinks
// fastest and the widest probes touch the smallest accumulator. nil means "no
// candidates".
func (si *segIndex) candidateOffsets(usable []usableProbe) *roaring.Bitmap {
	return indexCandidateOffsets(si, usable)
}

// selectivityOrder returns indices into usable ordered by ascending estimated
// candidate count (most selective first). It is a pure ordering heuristic: the
// AND is commutative, so this never changes the result, only the work. Ties and
// missing stats fall back to input order for a deterministic plan.
func (si *segIndex) selectivityOrder(usable []usableProbe) []int {
	order, _ := indexSelectivityOrder(si, usable)
	return order
}

// statsFor returns the segStats for a probe's attribute, or nil if that segment
// does not index it (covers() has already been checked in the query path, so this
// is non-nil there; the guard keeps the estimators safe in isolation).
func (si *segIndex) statsFor(up usableProbe) *segStats {
	if up.cat {
		if cp := si.cat[up.attrID]; cp != nil {
			return &cp.stats
		}
		return nil
	}
	if vp := si.val[up.attrID]; vp != nil {
		return &vp.stats
	}
	return nil
}

// canSkip reports whether the segment's indexed prefix provably holds no record
// satisfying this probe — so a query whose conjunction includes it can skip the
// whole prefix and only full-scan the un-indexed tail. It is correctness-critical:
// it must return true only when certain. Exceptional records (value present but
// not the indexed literal type) are re-verified candidates, so any exception
// forbids a skip. Equality/range/membership can skip; `!=` never does.
func (si *segIndex) canSkip(up usableProbe) bool { return indexCanSkip(si, up) }

// bloomAbsent reports whether a categorical ==/in probe's values are all provably absent
// via the in-RAM per-segment bloom.
func (si *segIndex) bloomAbsent(up usableProbe) bool {
	if !up.cat || (up.op != "==" && up.op != "in") {
		return false
	}
	s := si.statsFor(up)
	if s == nil || s.bloom == nil {
		return false
	}
	for _, v := range up.svals {
		if s.bloom.mayContain(hashString(v)) {
			return false // a value might be present: cannot prove absent
		}
	}
	return true
}

// estCandidates estimates how many records in the indexed prefix this probe would
// admit (its candidate-bitmap cardinality). Used only to order probes, so a rough
// estimate is fine; it never affects correctness.
func (si *segIndex) estCandidates(up usableProbe) float64 { return indexEstCandidates(si, up) }

// estEqualStr / estEqualFloat estimate the record count for one equality value:
// its exact top-N count if it is a heavy hitter, else the average tail count (0 if
// the bloom, for a categorical, proves the value absent). Kept as two typed
// helpers so the hot ordering path boxes nothing.
func (s *segStats) estEqualStr(v string) float64 {
	for _, e := range s.top {
		if e.skey == v {
			return float64(e.count)
		}
	}
	if s.bloom != nil && !s.bloom.mayContain(hashString(v)) {
		return 0
	}
	return s.avgTailCount()
}

func (s *segStats) estEqualFloat(v float64) float64 {
	for _, e := range s.top {
		if e.fkey == v {
			return float64(e.count)
		}
	}
	return s.avgTailCount()
}

// estRange returns the estimated fraction of indexable records passing a range comparison
// against threshold t. It uses the equi-depth histogram when present (skew-aware), falling
// back to linear interpolation over [min,max] for a pre-v9 sidecar or a degenerate span.
func (s *segStats) estRange(op string, t float64) float64 {
	// A NaN THRESHOLD is reachable from an ordinary query -- `Rate >= 0.0/0.0` folds to a probe
	// whose value is a NaN -- and no stored value satisfies any ordering comparison against it,
	// so the indexable records contribute nothing. Caught here because a NaN is no more usable
	// as a search key than as a stored one: the histogram's boundary search matches no bucket
	// (`bound[j] >= NaN` is false for every j), so it indexed one past the last bucket and took
	// the process down, and the [min,max] fallback below divides into a NaN that then poisons
	// the comparison the selectivity ordering sorts by. The caller still adds the exception
	// count, so the probe keeps a candidate estimate to be ordered on.
	if math.IsNaN(t) {
		return 0
	}
	if s.hist != nil {
		return s.hist.estRange(op, t)
	}
	if !s.hasRange || s.max <= s.min {
		if cmpFloat(op, s.min, t) {
			return 1
		}
		return 0
	}
	frac := (t - s.min) / (s.max - s.min)
	if frac < 0 {
		frac = 0
	} else if frac > 1 {
		frac = 1
	}
	switch op {
	case "<", "<=":
		return frac
	case ">", ">=":
		return 1 - frac
	}
	return 1
}

// skipsPrefix reports whether the whole indexed prefix can be skipped for this
// query: the candidate set is the intersection of the per-probe candidate sets, so
// if any single probe provably has no candidate (canSkip), the intersection is
// empty. Cheaper than candidateOffsets for range probes (no key iteration) and the
// only skip path once postings are dropped from an immutable segment.
func (si *segIndex) skipsPrefix(usable []usableProbe) bool { return indexSkipsPrefix(si, usable) }
func (si *segIndex) coveredUpto() uint32                   { return si.upto }

func (si *segIndex) probeOffsets(up usableProbe) *roaring.Bitmap {
	if up.cat {
		cp := si.cat[up.attrID]
		switch up.op {
		case "==", "in":
			bm := unionPostings(len(up.svals), func(i int) *roaring.Bitmap { return cp.post[up.svals[i]] })
			bm.Or(cp.exc)
			return bm
		case "!=":
			bm := si.all.Clone()
			if p := cp.post[up.svals[0]]; p != nil {
				bm.AndNot(p)
			}
			return bm
		case "present": // attr isnt undefined: posted a value, or present-but-exceptional
			bm := cp.posted.Clone()
			bm.Or(cp.exc)
			return bm
		case "absent": // attr is undefined: everything but the definitely-posted records
			bm := si.all.Clone()
			bm.AndNot(cp.posted)
			return bm
		case "is": // =?= exact (case-sensitive)
			// The exceptions too: an attribute holding an expression rather than a string literal
			// (`strcat("al", "ice")`) is in exc, not in any exact bucket, and can still evaluate
			// to the probe's value.
			bm := roaring.New()
			if e := cp.exactBitmap(up.svals[0]); e != nil {
				bm.Or(e)
			}
			bm.Or(cp.exc)
			return bm
		case "isnt": // =!= exact: everything but the exact-case matches
			bm := si.all.Clone()
			if e := cp.exactBitmap(up.svals[0]); e != nil {
				bm.AndNot(e)
			}
			return bm
		}
		return roaring.New()
	}
	vp := si.val[up.attrID]
	switch up.op {
	case "==", "in":
		bm := unionPostings(len(up.fvals), func(i int) *roaring.Bitmap { return vp.post[up.fvals[i]] })
		bm.Or(vp.exc)
		return bm
	case "!=":
		bm := si.all.Clone()
		if p := vp.post[up.fvals[0]]; p != nil {
			bm.AndNot(p)
		}
		return bm
	case "present":
		bm := vp.posted.Clone()
		bm.Or(vp.exc)
		return bm
	case "absent":
		bm := si.all.Clone()
		bm.AndNot(vp.posted)
		return bm
	case "<", "<=", ">", ">=":
		// Boundary-search the sorted keys to the matching run [from,to), then union only
		// those keys' bitmaps -- O(log n + matches) instead of scanning every key. A
		// two-sided probe bounds the run on both ends, so a narrow band touches few keys
		// however wide either half-open side would have been.
		keys := vp.sortedKeys
		from, to := rangeSpan(up, len(keys),
			func(t float64) int { return lowerBoundF(keys, t) },
			func(t float64) int { return upperBoundF(keys, t) })
		bm := unionPostings(to-from, func(i int) *roaring.Bitmap { return vp.post[keys[from+i]] })
		bm.Or(vp.exc)
		return bm
	}
	return roaring.New()
}

// lowerBoundF returns the first index i with keys[i] >= t (keys sorted ascending).
func lowerBoundF(keys []float64, t float64) int {
	return sort.Search(len(keys), func(i int) bool { return keys[i] >= t })
}

// upperBoundF returns the first index i with keys[i] > t.
func upperBoundF(keys []float64, t float64) int {
	return sort.Search(len(keys), func(i int) bool { return keys[i] > t })
}

func cmpFloat(op string, k, t float64) bool {
	switch op {
	case "<":
		return k < t
	case "<=":
		return k <= t
	case ">":
		return k > t
	case ">=":
		return k >= t
	}
	return false
}

// scanShardIndexed yields the shard's matching ads using each segment's index to
// visit only candidate records, and full-scanning any records the index does not
// cover (a segment with no index, or the tail beyond its build watermark). Each
// candidate gets exactly what a full scan gives every record -- the columnar
// prefilter, column projection, reassembly and the full-query match (refVisitor) --
// so the result is identical to a full scan, and a candidate
// the columns reject is never rebuilt. That last part is what makes a lookup into a
// big cluster cheap: an archived job's ClusterId posting can hold hundreds of
// thousands of records, and rebuilding each one to test its ProcId cost seconds.
// Returns false if the consumer stopped.
func (c *Collection) scanShardIndexed(sh *shard, usable []usableProbe, qp queryPlan, emit scanEmit) bool {
	if unsatisfiable(usable) {
		// The conjunction is impossible on its face; nothing in this shard -- indexed
		// prefix, un-indexed tail, or exception set -- can match. Skip the snapshot too,
		// so an impossible query costs no pins, no page faults, and no decompression.
		return true
	}
	s0, wins := sh.snapshot()
	defer releaseWindows(wins)
	// Zone-prune before walking candidates, exactly as the scan does. The index's own
	// per-segment skip (skipsPrefix) only reaches segments whose index covers the probe; a
	// segment sealed before the attribute was indexed has no postings to consult and would
	// otherwise be walked record by record, even when its zone map proves it holds no match --
	// on an archive whose index was added late, that is nearly every segment. A zone map adopted
	// from a sidecar is safe to apply here for the same reason it is on the scan: the section is
	// keyed by attribute name and bound to this process's ids on load (parseZoneBlob), so it
	// describes the attribute it names whatever the zone or index configuration has become.
	walk := c.zoneWalk(wins, qp)
	visit, stopped := c.refVisitor(s0, qp, emit)
	ct := qp.prunesCT()
	src := conjCandidates{usable: usable, ds: c.newDeltaScope(sh, s0, walk, usable, ct), ct: ct}
	c.forEachCandidateRef(s0, walk, src, c.reverseScan, qp.stats, visit)
	return !stopped()
}

// forEachCandidateRef calls fn for each keyed record of wins, visible at s0, that the segment
// indexes cannot rule out for src: a covering segment's candidate offsets (src.prefix), the
// tail written after its index was built, and every record of a segment src finds no index
// for. fn sees a superset of the true matches and must re-verify; it returns false to stop. Markers -- time checkpoints, a segment's dictionary and
// columnar records -- are never handed to fn: they are not ads, and the full walks of
// uncovered windows and tails pass over them.
//
// When reverse is set (an append-only, newest-first collection), candidates are visited
// newest-first: segments from the last backward, and within a segment the un-indexed tail
// (written last) before the indexed prefix, each in descending offset order. A consumer
// that stops early then gets the newest matches -- a pushed-down LIMIT through the index.
func (c *Collection) forEachCandidateRef(s0 uint64, wins []segWindow, src candidateSource, reverse bool, st *ScanStats, fn func(recRef) bool) {
	var offs []uint32 // reused across reversed ranges
	visible := func(w segWindow, o uint32) bool {
		return !recIsMarker(w.data, o) && w.visible(o, s0)
	}
	// scanRange visits the records in [from, to), in walk order.
	scanRange := func(w segWindow, d *segDictHandle, from, to int) bool {
		if !reverse {
			for off := from; off < to; {
				o := uint32(off)
				if end, ok := w.dmg.skip(o); ok {
					off = int(end)
					continue
				}
				total := recTotalLen(w.data, o)
				if total == 0 {
					break
				}
				if visible(w, o) && !fn(recRef{w: w, off: o, dict: d}) {
					return false
				}
				off += int(total)
			}
			return true
		}
		// Records are variable-length and self-describe only their forward length, so a
		// range walked in reverse is collected forward, then replayed backward.
		offs = offs[:0]
		for off := from; off < to; {
			o := uint32(off)
			if end, ok := w.dmg.skip(o); ok {
				off = int(end)
				continue
			}
			total := recTotalLen(w.data, o)
			if total == 0 {
				break
			}
			offs = append(offs, o)
			off += int(total)
		}
		for i := len(offs) - 1; i >= 0; i-- {
			if visible(w, offs[i]) && !fn(recRef{w: w, off: offs[i], dict: d}) {
				return false
			}
		}
		return true
	}
	// window visits one window's candidates: the indexed prefix's postings and the tail
	// beyond the index, the tail first when walking newest-first.
	window := func(w segWindow) bool {
		d := w.dict()
		si := src.index(w)
		if si == nil {
			st.indexSegment(0, false, true)
			return scanRange(w, d, 0, w.used) // no usable index: walk the window in full
		}
		upto := int(si.coveredUpto())
		tail := func() bool { return upto >= w.used || scanRange(w, d, upto, w.used) }
		if reverse && !tail() {
			return false
		}
		if cand := src.prefix(w, si, st); cand != nil {
			var it roaring.IntIterable
			if reverse {
				it = cand.ReverseIterator()
			} else {
				it = cand.Iterator()
			}
			for it.HasNext() {
				o := it.Next()
				if visible(w, o) && !fn(recRef{w: w, off: o, dict: d}) {
					return false
				}
			}
		}
		return reverse || tail()
	}
	if reverse {
		for wi := len(wins) - 1; wi >= 0; wi-- {
			if !window(wins[wi]) {
				return
			}
		}
		return
	}
	for _, w := range wins {
		if !window(w) {
			return
		}
	}
}

// candidateSource narrows one window of a candidate walk (forEachCandidateRef) to the records its
// segment index cannot rule out.
type candidateSource interface {
	// index returns the index that can narrow w, or nil when w has to be walked in full.
	index(w segWindow) readIndex
	// prefix returns the candidates in si's covered prefix of w as a bitmap of offsets (nil for
	// none) and records the segment's index verdict in st.
	prefix(w segWindow, si readIndex, st *ScanStats) *roaring.Bitmap
}

// conjCandidates is the candidate source for one conjunction of probes: the postings'
// intersection plus the delta records ds admits. ct marks probes computed with CurrentTime folded
// to the query's pinned time, which hold only for records without CurrentTime, so a window not
// proven free of it (segWindow.ctFree) is walked in full.
type conjCandidates struct {
	usable []usableProbe
	ds     *deltaScope
	ct     bool
}

func (cc conjCandidates) index(w segWindow) readIndex { return windowIndex(w, cc.usable, cc.ct) }

func (cc conjCandidates) prefix(w segWindow, si readIndex, st *ScanStats) *roaring.Bitmap {
	return prefixCandidates(w, si, cc.usable, cc.ds, st)
}

// groupCandidates is the candidate source for a DNF plan: the union over groups of each group's
// postings intersection plus the delta records that group's scope admits (ds[i] for groups[i]).
// A delta record that satisfies the query satisfies some group, so the union of the groups'
// scopes keeps every delta that can match. A window is served only by an index covering every
// probe of every group -- one disjunct it cannot answer could match any record -- and, with ct as
// for conjCandidates, only when proven free of CurrentTime.
type groupCandidates struct {
	groups [][]usableProbe
	ds     []*deltaScope
	ct     bool
}

// newGroupCandidates builds the candidate source for groups over the windows wins of sh, visible
// at s0, with one delta scope per group.
func (c *Collection) newGroupCandidates(sh *shard, s0 uint64, wins []segWindow, groups [][]usableProbe, ct bool) groupCandidates {
	gc := groupCandidates{groups: groups, ct: ct}
	if c.deltaRead {
		gc.ds = make([]*deltaScope, len(groups))
		for i, g := range groups {
			gc.ds[i] = c.newDeltaScope(sh, s0, wins, g, ct)
		}
	}
	return gc
}

func (gc groupCandidates) index(w segWindow) readIndex { return groupsWindowIndex(w, gc.groups, gc.ct) }

// groupsWindowIndex is windowIndex for a DNF plan: the index that can narrow w to every group's
// candidates, or nil when w has to be walked in full.
func groupsWindowIndex(w segWindow, groups [][]usableProbe, ct bool) readIndex {
	si := w.idx
	if si == nil || !si.coversGroups(groups) || (ct && !w.ctFree) {
		return nil
	}
	return si
}

// skipsPrefixGroups reports whether si proves that no record it posts satisfies any group: the
// union is empty exactly when every disjunct is.
func skipsPrefixGroups(si readIndex, groups [][]usableProbe) bool {
	for _, g := range groups {
		if !si.skipsPrefix(g) {
			return false
		}
	}
	return len(groups) > 0
}

func (gc groupCandidates) prefix(w segWindow, si readIndex, st *ScanStats) *roaring.Bitmap {
	var cand *roaring.Bitmap
	if skipsPrefixGroups(si, gc.groups) {
		st.indexSegment(0, true, false)
	} else if cand = si.candidateOffsetsGroups(gc.groups); cand != nil {
		st.indexSegment(int(cand.GetCardinality()), false, false)
	}
	for _, ds := range gc.ds {
		cand = ds.withDeltas(w, cand)
	}
	return cand
}

// liveGroups drops the disjuncts the planner proved impossible -- they contribute nothing to the
// union -- and reports none when every disjunct is. groups may be SHARED (the match path fans a
// scan out one goroutine per shard over a single plan), so a filtered slice is freshly allocated,
// never compacted in place; the common case allocates nothing.
func liveGroups(groups [][]usableProbe) (live [][]usableProbe, none bool) {
	if unsatisfiableGroups(groups) {
		return nil, true
	}
	if !anyUnsatisfiableGroup(groups) {
		return groups, false
	}
	live = make([][]usableProbe, 0, len(groups))
	for _, g := range groups {
		if !unsatisfiable(g) {
			live = append(live, g)
		}
	}
	return live, false
}

// windowIndex returns the index that can narrow w to usable's candidates, or nil when w has to be
// walked in full: it has no index, its index does not cover every probe's attribute, or the probes
// fold CurrentTime (ct) and w is not proven free of it.
func windowIndex(w segWindow, usable []usableProbe, ct bool) readIndex {
	si := w.idx
	if si == nil || !si.covers(usable) || (ct && !w.ctFree) {
		return nil
	}
	return si
}

// prefixCandidates returns the records of si's covered prefix of w that the index cannot rule out
// for usable -- the postings' intersection, plus the delta records ds admits (index_deltas.go) --
// in a bitmap of offsets, nil for none. The rest of w, from si.coveredUpto(), is not covered and is the
// caller's to walk.
//
// Segment skip: if any probe provably has no candidate in the indexed prefix (min/max out of range,
// bloom miss), the conjunction is empty there -- among the records the index could post. A delta
// record it could not is a candidate regardless.
func prefixCandidates(w segWindow, si readIndex, usable []usableProbe, ds *deltaScope, st *ScanStats) *roaring.Bitmap {
	var cand *roaring.Bitmap
	if si.skipsPrefix(usable) {
		st.indexSegment(0, true, false)
	} else if cand = si.candidateOffsets(usable); cand != nil {
		st.indexSegment(int(cand.GetCardinality()), false, false)
	}
	return ds.withDeltas(w, cand)
}

// scanShardIndexedGroups is scanShardIndexed for a DNF plan: it visits the union of the groups'
// candidates (groupCandidates) and re-verifies each against the full query with the same visitor
// as the conjunctive path (refVisitor), in the same order a scan visits them.
//
// plan is the query's DNF (vm.Query.ProbePlan) that groups was planned from. A window is
// zone-pruned when its zone map rules out the query's required conjuncts, or every disjunct of
// plan: a record satisfying the query satisfies every probe of some group, so a window no group
// can match holds no match either.
func (c *Collection) scanShardIndexedGroups(sh *shard, groups [][]usableProbe, plan []vm.ProbeGroup, qp queryPlan, emit scanEmit) bool {
	groups, none := liveGroups(groups)
	if none {
		return true // every disjunct is impossible: the union is empty
	}
	s0, wins := sh.snapshot()
	defer releaseWindows(wins)
	walk := c.zoneWalkGroups(wins, qp, plan)
	visit, stopped := c.refVisitor(s0, qp, emit)
	src := c.newGroupCandidates(sh, s0, walk, groups, qp.prunesCT())
	c.forEachCandidateRef(s0, walk, src, c.reverseScan, qp.stats, visit)
	return !stopped()
}

// zoneWalkGroups is zoneWalk for a DNF plan: it drops the windows whose zone maps rule out the
// query's required conjuncts or every disjunct of plan, and records the counts in qp.stats.
func (c *Collection) zoneWalkGroups(wins []segWindow, qp queryPlan, plan []vm.ProbeGroup) []segWindow {
	walk := wins
	if c.hasZones.Load() {
		ct := qp.prunesCT()
		walk = make([]segWindow, 0, len(wins))
		for _, w := range wins {
			if c.zoneExcludes(w, qp.probes, ct) || c.zoneExcludesGroups(w, plan, ct) {
				continue
			}
			walk = append(walk, w)
		}
	}
	qp.stats.segments(len(wins), len(wins)-len(walk))
	return walk
}

// scanShardCandidatesGroups visits a DNF plan's (vm.ProbePlan with more than one group)
// candidates in one shard for matchmaking: per window, the union over groups of each group's
// candidate intersection (groupCandidates), so onCand sees a superset of the union's true matches
// and the caller re-verifies; onCand returns false to stop. A window no index serves for every
// group, and every served window's un-indexed tail, are walked in full. No zone pruning: only an
// append-only collection keeps zone maps, and a collection of slots is mutable.
func (c *Collection) scanShardCandidatesGroups(sh *shard, groups [][]usableProbe, onCand func(w []byte, dict *segDictHandle) bool) {
	groups, none := liveGroups(groups)
	if none {
		return // every disjunct is impossible: the union is empty
	}
	s0, wins := sh.snapshot()
	defer releaseWindows(wins)
	var dbuf []byte
	src := c.newGroupCandidates(sh, s0, wins, groups, false)
	c.forEachCandidateRef(s0, wins, src, false, nil, func(r recRef) bool {
		// Internal system records carry no normal indexed attributes and are hidden from
		// client results.
		if isSystemKeyBytes(r.key()) {
			return true
		}
		ww, err := c.wireAt(r, s0, dbuf)
		if err != nil {
			return true // counted by wireAt; skip it, as every reader does
		}
		dbuf = ww
		return onCand(ww, r.dict)
	})
}

// backfillWindow returns the segments within the newest Options.IndexBackfillBytes of a
// shard, or nil when no bound is configured (carry a configuration change everywhere).
//
// Newest-first, because that is the end that gets queried: an archive is read newest-first
// with a limit, so the oldest segments may never be reached, and re-indexing them costs the
// whole archive to speed up reads that do not happen. Caller holds sh.mu.
func (c *Collection) backfillWindow(sh *shard) map[*segment]bool {
	if c.indexBackfillBytes <= 0 {
		return nil
	}
	window := map[*segment]bool{}
	var acc int64
	for i := len(sh.segs) - 1; i >= 0; i-- {
		seg := sh.segs[i]
		if seg == nil || seg.used == 0 {
			continue
		}
		window[seg] = true
		acc += int64(seg.used)
		if acc >= c.indexBackfillBytes {
			break
		}
	}
	return window
}

// recordCount is the number of records this segment's index covers. The all-records bitmap is
// already materialized for this tier, so it is a cardinality read.
func (si *segIndex) recordCount() uint64 { return si.all.GetCardinality() }
