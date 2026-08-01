package collections

import (
	"math"
	"sort"
	"strings"
	"time"

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
func (c *Collection) Reindex() {
	// Serialize concurrent reindexers so two do not build and install the same segment's
	// sidecar at once (the eager per-seal reindex vs. the periodic one). Idempotent and
	// cheap when there is nothing new to build.
	c.reindexMu.Lock()
	defer c.reindexMu.Unlock()
	start := time.Now()
	defer func() { c.opm.reindex.observe(time.Since(start)) }()
	spec := c.spec.Load()
	persistent := c.dir != ""
	for _, sh := range c.shards {
		// Snapshot segments + the active append target under the read lock, then build
		// off-lock. Every segment except sh.act is sealed (immutable until compacted), so
		// safe to convert to the pageable mmap sidecar; the active segment stays in-RAM.
		sh.mu.RLock()
		act := sh.act
		sealRAM := sh.sealRAM
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
				if spec.any() && seg.used > 0 && mm.specGen != spec.gen {
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
		sh.mu.RUnlock()
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
				si = buildSegIndex(t.seg.data, t.used, t.seg.codec, spec)
				t.seg.idx.Store(si)
			}
			// Sealed segment: move its index off the heap into the mmap sidecar (reclaimable,
			// GC-invisible) -- a file sidecar for a persistent segment, an anonymous mapping
			// for a RAM one. Best-effort; on failure the in-RAM index stays.
			if t.seal {
				if persistent {
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

// unsatisfiable reports whether a conjunction can never be satisfied, because one of its
// probes is an empty band. Every probe in a group must hold, so one impossible conjunct
// empties the group -- no segment need be opened at all.
func unsatisfiable(usable []usableProbe) bool {
	for _, up := range usable {
		if up.emptyBand() {
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
	switch {
	case hasLo && hasHi:
		return usableProbe{attrID: a.attrID, op: loOp, fvals: []float64{loVal}, hiOp: hiOp, hiVal: hiVal}
	case hasLo:
		return usableProbe{attrID: a.attrID, op: loOp, fvals: []float64{loVal}}
	default:
		return usableProbe{attrID: a.attrID, op: hiOp, fvals: []float64{hiVal}}
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
		if !up.cat && isRangeOp(up.op) {
			if merges == nil {
				merges = make(map[uint32]usableProbe, 1)
			}
			merges[up.attrID] = up
		}
	}
	return merges
}

// applyMerge redirects an explain's selectivity estimate for a range conjunct to the probe
// the planner actually runs for that attribute. A conjunct that IS that probe passes through
// unmarked; one that was folded into it -- the other half of a band, or a looser same-side
// bound the tighter one subsumed -- is marked, because the numbers beside it then describe
// the merged probe rather than the conjunct's own reach.
func applyMerge(merges map[uint32]usableProbe, up usableProbe, pe *ProbeExplain) usableProbe {
	if merges == nil || up.cat || !isRangeOp(up.op) {
		return up
	}
	m, ok := merges[up.attrID]
	if !ok || sameRange(m, up) {
		return up
	}
	pe.Coalesced = true
	return m
}

// sameRange reports whether two range probes express the identical bound(s).
func sameRange(a, b usableProbe) bool {
	if a.op != b.op || a.hiOp != b.hiOp || a.hiVal != b.hiVal || len(a.fvals) != len(b.fvals) {
		return false
	}
	return len(a.fvals) == 0 || a.fvals[0] == b.fvals[0]
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

	// Coalesced marks a range conjunct the planner folded into another range probe on the
	// same attribute -- the opposite bound (`Memory > 1024 && Memory < 4096` becomes one
	// two-sided probe) or a tighter same-side bound that subsumes it (`Memory > 1024 &&
	// Memory > 512` probes only `> 1024`). Selectivity/EstCandidates then describe the
	// merged probe, not this conjunct's own reach; the conjunct that IS the probe is left
	// unmarked.
	Coalesced bool `json:"coalesced,omitempty"`
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
	// Plan is the chosen access path: "indexed" (visit index candidates),
	// "parallel-scan", "serial-scan" (full scan), or "empty" (a contradictory conjunct
	// makes the query unsatisfiable, so no records are visited at all).
	Plan string `json:"plan"`
	// Parallelism is the configured per-query worker cap; Shards is the shard count.
	Parallelism int `json:"parallelism"`
	Shards      int `json:"shards"`
	// TotalAds is the live ad count, the denominator for probe selectivity.
	TotalAds int `json:"totalAds"`
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
	merges := rangeMerges(usable)
	for _, p := range probes {
		pe := ProbeExplain{Attr: p.Attr, Op: p.Op}
		var up usableProbe
		var isUsable bool
		pe.Indexed, pe.Kind, up, isUsable = c.probeIndexKind(p)
		up = applyMerge(merges, up, &pe)
		if isUsable {
			if cand, covered := c.estimateCandidates(up); covered {
				pe.HasSelectivity = true
				pe.EstCandidates = int64(cand + 0.5)
				if total > 0 {
					pe.Selectivity = math.Min(1, cand/float64(total))
				}
			}
		}
		ex.Probes = append(ex.Probes, pe)
	}
	switch {
	case unsatisfiable(usable):
		// A contradictory conjunct (`Memory < 1024 && Memory > 2048`) makes the whole
		// query impossible, so no shard is even opened. Naming it is the point: this is
		// the answer to "why does my constraint return nothing".
		ex.Plan = "empty"
	case len(usable) > 0:
		ex.Plan = "indexed"
	case c.queryPar > 1:
		ex.Plan = "parallel-scan"
	default:
		ex.Plan = "serial-scan"
	}
	return ex
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
// the indexed prefix (not the un-indexed tail) and may include superseded records.
func (c *Collection) estimateCandidates(up usableProbe) (cand float64, covered bool) {
	single := []usableProbe{up}
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			if si := w.seg.readIdx(); si != nil && si.covers(single) {
				cand += si.estCandidates(up)
				covered = true
			}
		}
		releaseWindows(wins)
		_ = s0
	}
	return cand, covered
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
	return coalesceRanges(out)
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
	return usableProbe{attrID: id, cat: false, op: op, fvals: fvals}, true
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
	return indexSelectivityOrder(si, usable)
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

// estRange returns the estimated fraction of indexable records passing a range
// comparison against threshold t, by linear interpolation over [min,max].
func (s *segStats) estRange(op string, t float64) float64 {
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

// probeOffsets returns a fresh, mutable offset bitmap for one probe.
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
			if e := cp.exactBitmap(up.svals[0]); e != nil {
				return e.Clone()
			}
			return roaring.New()
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
// cover (a segment with no index, or the tail beyond its build watermark). Every
// visited record is MVCC-visibility filtered and full-query re-verified, so the
// result is identical to a full scan. Returns false if the consumer stopped.
func (c *Collection) scanShardIndexed(sh *shard, usable []usableProbe, qp queryPlan, emit func(w []byte) bool) bool {
	return c.scanShardCandidates(sh, usable, c.reverseScan, func(w []byte) bool {
		if !matchWire(w, qp) {
			return true // not a match: keep scanning
		}
		return emit(w)
	})
}

// scanShardCandidates visits the candidate records for `usable` in one shard,
// handing each candidate's decompressed wire bytes to onCand (which returns false to
// stop the whole scan). Windows whose per-segment index does not cover the probes,
// and the un-indexed tail of those that do, are full-scanned -- so onCand sees a
// superset of the true candidates and the caller must re-verify. Returns false if
// onCand asked to stop. This is the shared candidate-enumeration used by the indexed
// Query path (scanShardIndexed).
//
// When reverse is set (an append-only, newest-first collection), candidates are visited
// newest-first: segments from the last backward, and within a segment the un-indexed tail
// (written last) before the indexed prefix, each in descending offset order. A consumer
// that stops early then gets the newest matches -- a pushed-down LIMIT through the index.
func (c *Collection) scanShardCandidates(sh *shard, usable []usableProbe, reverse bool, onCand func(w []byte) bool) bool {
	if unsatisfiable(usable) {
		// The conjunction is impossible on its face; nothing in this shard -- indexed
		// prefix, un-indexed tail, or exception set -- can match. Skip the snapshot too,
		// so an impossible query costs no pins, no page faults, and no decompression.
		return true
	}
	s0, wins := sh.snapshot()
	defer releaseWindows(wins)
	var dbuf []byte
	// visit tests one record's visibility and hands its decompressed wire bytes to
	// onCand; returns stop = true when the consumer asked to stop.
	visit := func(w segWindow, o uint32) (stop bool) {
		if !(recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0) {
			return false
		}
		// Defensive: an internal system record carries no normal indexed attributes,
		// so it can never appear as an index candidate; the tail full-scan could still
		// reach one, and Match reuses this primitive, so skip it here too.
		if isSystemKeyBytes(recKey(w.data, o)) {
			return false
		}
		ww, err := w.codec.Decompress(dbuf[:0], recAd(w.data, o))
		if err != nil {
			return false
		}
		dbuf = ww
		return !onCand(ww)
	}
	// scanRange full-scans records in [from, to).
	scanRange := func(w segWindow, from, to int) bool {
		for off := from; off < to; {
			o := uint32(off)
			total := recTotalLen(w.data, o)
			if total == 0 {
				break
			}
			if visit(w, o) {
				return false
			}
			off += int(total)
		}
		return true
	}

	if reverse {
		return scanShardCandidatesReverse(wins, usable, visit)
	}

	for _, w := range wins {
		si := w.seg.readIdx()
		if si == nil || !si.covers(usable) {
			if !scanRange(w, 0, w.used) { // no usable index: full-scan the window
				return false
			}
			continue
		}
		// Segment skip: if any probe provably has no candidate in the indexed prefix
		// (min/max out of range, bloom miss), the conjunction is empty there — skip
		// the prefix and only full-scan the tail written after the index was built.
		if si.skipsPrefix(usable) {
			if int(si.coveredUpto()) < w.used {
				if !scanRange(w, int(si.coveredUpto()), w.used) {
					return false
				}
			}
			continue
		}
		// Indexed prefix [0, upto): visit only candidate offsets.
		if cand := si.candidateOffsets(usable); cand != nil {
			it := cand.Iterator()
			for it.HasNext() {
				if visit(w, it.Next()) {
					return false
				}
			}
		}
		// Tail [upto, used): written after the index was built — full-scan it.
		if int(si.coveredUpto()) < w.used {
			if !scanRange(w, int(si.coveredUpto()), w.used) {
				return false
			}
		}
	}
	return true
}

// scanShardCandidatesReverse is scanShardCandidates' newest-first traversal: segments from
// the last backward, and within each the tail (newest records, written after the index)
// before the indexed prefix, both in descending offset order. visit returns true to stop.
// Records are variable-length (forward-length only), so a range walked in reverse is
// collected forward into a reused slice, then replayed backward.
func scanShardCandidatesReverse(wins []segWindow, usable []usableProbe, visit func(w segWindow, o uint32) bool) bool {
	var offs []uint32 // reused across ranges
	scanRangeReverse := func(w segWindow, from, to int) bool {
		offs = offs[:0]
		for off := from; off < to; {
			o := uint32(off)
			total := recTotalLen(w.data, o)
			if total == 0 {
				break
			}
			offs = append(offs, o)
			off += int(total)
		}
		for i := len(offs) - 1; i >= 0; i-- {
			if visit(w, offs[i]) {
				return false
			}
		}
		return true
	}
	for wi := len(wins) - 1; wi >= 0; wi-- {
		w := wins[wi]
		si := w.seg.readIdx()
		if si == nil || !si.covers(usable) {
			if !scanRangeReverse(w, 0, w.used) {
				return false
			}
			continue
		}
		// Tail (newest, written after the index) first, descending.
		if int(si.coveredUpto()) < w.used {
			if !scanRangeReverse(w, int(si.coveredUpto()), w.used) {
				return false
			}
		}
		if si.skipsPrefix(usable) {
			continue // prefix provably has no candidate
		}
		// Then the indexed prefix candidates, descending (newest of the prefix first).
		if cand := si.candidateOffsets(usable); cand != nil {
			it := cand.ReverseIterator()
			for it.HasNext() {
				if visit(w, it.Next()) {
					return false
				}
			}
		}
	}
	return true
}

// scanShardIndexedGroups is scanShardIndexed for a DNF plan: it visits the union of
// the groups' candidates and re-verifies each against the full query.
func (c *Collection) scanShardIndexedGroups(sh *shard, groups [][]usableProbe, qp queryPlan, emit func(w []byte) bool) bool {
	return c.scanShardCandidatesGroups(sh, groups, c.reverseScan, func(w []byte) bool {
		if !matchWire(w, qp) {
			return true
		}
		return emit(w)
	})
}

// scanShardCandidatesGroups is scanShardCandidates for a DNF plan (vm.ProbePlan with
// more than one group): per window it visits the union over groups of each group's
// candidate intersection, so onCand sees a superset of the union's true matches (the
// caller re-verifies). Segment skipping is not applied here -- a segment is skippable
// only when EVERY disjunct is empty, and visiting a few extra candidates is cheaper
// than proving that per group -- but a window whose index does not cover all groups,
// and every covered window's un-indexed tail, are still full-scanned.
func (c *Collection) scanShardCandidatesGroups(sh *shard, groups [][]usableProbe, reverse bool, onCand func(w []byte) bool) bool {
	if unsatisfiableGroups(groups) {
		return true // every disjunct is impossible: the union is empty (see scanShardCandidates)
	}
	// An impossible disjunct contributes nothing to the union, so drop it rather than
	// building its (empty) candidate set per window.
	if kept := groups[:0]; true {
		for _, g := range groups {
			if !unsatisfiable(g) {
				kept = append(kept, g)
			}
		}
		groups = kept
	}
	s0, wins := sh.snapshot()
	defer releaseWindows(wins)
	var dbuf []byte
	visit := func(w segWindow, o uint32) (stop bool) {
		if !(recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0) {
			return false
		}
		// Defensive: internal system records carry no normal indexed attributes and
		// are hidden from client results (see scanShardCandidates).
		if isSystemKeyBytes(recKey(w.data, o)) {
			return false
		}
		ww, err := w.codec.Decompress(dbuf[:0], recAd(w.data, o))
		if err != nil {
			return false
		}
		dbuf = ww
		return !onCand(ww)
	}
	scanRange := func(w segWindow, from, to int) bool {
		for off := from; off < to; {
			o := uint32(off)
			total := recTotalLen(w.data, o)
			if total == 0 {
				break
			}
			if visit(w, o) {
				return false
			}
			off += int(total)
		}
		return true
	}

	if reverse {
		return scanShardCandidatesGroupsReverse(wins, groups, visit)
	}

	for _, w := range wins {
		si := w.seg.idx.Load()
		if si == nil || !si.coversGroups(groups) {
			if !scanRange(w, 0, w.used) { // index can't serve every disjunct: full-scan
				return false
			}
			continue
		}
		// Single group (a conjunctive plan): reuse the segment-skip fast path -- if a
		// probe provably has no candidate in the indexed prefix, only the tail needs a
		// scan. A union of groups cannot skip this cheaply, so it visits candidates.
		if len(groups) == 1 && si.skipsPrefix(groups[0]) {
			if int(si.upto) < w.used {
				if !scanRange(w, int(si.upto), w.used) {
					return false
				}
			}
			continue
		}
		if cand := si.candidateOffsetsGroups(groups); cand != nil {
			it := cand.Iterator()
			for it.HasNext() {
				if visit(w, it.Next()) {
					return false
				}
			}
		}
		if int(si.upto) < w.used {
			if !scanRange(w, int(si.upto), w.used) {
				return false
			}
		}
	}
	return true
}

// scanShardCandidatesGroupsReverse is the newest-first traversal of a DNF (disjunctive)
// candidate set: segments from the last backward, and within each the un-indexed tail
// before the union of the groups' candidate offsets, both in descending offset order. It
// reads the segment index via readIdx (so a sealed segment's mmap sidecar is used, not only
// an in-RAM index), which the forward path does not yet do. visit returns true to stop.
func scanShardCandidatesGroupsReverse(wins []segWindow, groups [][]usableProbe, visit func(w segWindow, o uint32) bool) bool {
	var offs []uint32
	scanRangeReverse := func(w segWindow, from, to int) bool {
		offs = offs[:0]
		for off := from; off < to; {
			o := uint32(off)
			total := recTotalLen(w.data, o)
			if total == 0 {
				break
			}
			offs = append(offs, o)
			off += int(total)
		}
		for i := len(offs) - 1; i >= 0; i-- {
			if visit(w, offs[i]) {
				return false
			}
		}
		return true
	}
	for wi := len(wins) - 1; wi >= 0; wi-- {
		w := wins[wi]
		si := w.seg.readIdx()
		if si == nil || !si.coversGroups(groups) {
			if !scanRangeReverse(w, 0, w.used) { // index can't serve every disjunct: full-scan
				return false
			}
			continue
		}
		// Tail (newest, written after the index) first, descending.
		if int(si.coveredUpto()) < w.used {
			if !scanRangeReverse(w, int(si.coveredUpto()), w.used) {
				return false
			}
		}
		// Single-group segment skip (a conjunctive plan routed here): if the prefix
		// provably has no candidate, only the tail (already scanned) applied.
		if len(groups) == 1 && si.skipsPrefix(groups[0]) {
			continue
		}
		// Then the union of the groups' indexed-prefix candidates, descending.
		if cand := si.candidateOffsetsGroups(groups); cand != nil {
			it := cand.ReverseIterator()
			for it.HasNext() {
				if visit(w, it.Next()) {
					return false
				}
			}
		}
	}
	return true
}
