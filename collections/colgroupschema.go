package collections

import (
	"sort"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// GROUP SCHEMAS, phase 1: derive and report. Nothing here changes how a record is stored or read.
//
// The base schema carries the attributes present in ~every ad. That threshold is what makes it
// cheap -- a slot costs fixed bytes in EVERY record -- and also what makes its coverage hostage
// to the mix of ads in the table. Measured on a real AP: a history table of completed jobs has
// 154 base attributes covering 90% of all attribute occurrences, but mixing in jobs that were
// removed before ever running drops the base to 80 attributes and 51% coverage, because the
// run-acquired attributes fall below the floor and leave together. Coverage swings on what the
// table happens to hold.
//
// A group schema is a set of attributes that co-occur: in any given ad they are all present or
// all absent. Such a set can be stored columnar for the ads that HAVE it without spending a slot
// in the ads that do not -- so the run-acquired attributes stay columnar for the jobs that ran,
// whatever fraction of the table never ran.
//
// WHY CO-OCCURRENCE, and not clustering. Grouping by identical presence is a single deterministic
// pass with no seeds, no distance metric, and no tuning; k-means over the same data varied by 6
// points of coverage across random inits. More importantly it makes the PARTIAL case -- an ad
// holding some but not all of a group -- impossible within the sample, by construction. Partial
// is the expensive state: an ad cannot be stored under the group (values would be missing) and
// cannot claim the group is absent either, so it falls back to a row decode. If the columnar path
// is F times faster, a partial fraction p costs about p*F of the benefit, so p wants to be well
// under 1/F -- 0.1% at F=1000. Deriving groups from exact co-occurrence starts p at zero and
// leaves drift as the only way it rises, which is what the reported counts are for.
//
// Nothing here reads an attribute NAME. A group is a fact about which records carry which
// attributes, so the derivation is independent of ad type -- no knowledge of jobs, slots, or any
// lifecycle.
//
// PHASE 2 NOTE, recorded here because it constrains the format this report is meant to justify.
// Group columns have to be readable by the VECTORIZED evaluator, not just the per-record resolver,
// since that is where much of the columnar speedup comes from. That works out if a group block is
// a SELECTION of one base block -- same record order, membership bitmap giving base index -> group
// index by rank -- so a record's index stays canonical across both. blockVecSource.LoadColumn can
// then scatter a group column into a full-length vm.Vec with non-members undefined, which is the
// same shape as fixEscapes patching records whose value is not in its slot. A group block batched
// independently of the base block would instead need a per-record indirection and would break the
// record-order assumptions the ordered scans rely on.

// groupSchema is one derived group: the attributes, and how the sample distributed over it.
type groupSchema struct {
	schema *adSchema // the group's own layout, built like any other schema
	ids    []uint32  // member attribute ids, sorted (schema.fields is layout-ordered)

	// Sample counts. In means every member attribute was present, None that not one was, and
	// Partial that some were and some were not.
	in, none, partial int
	// cells is In * len(ids): the attribute occurrences this group would make columnar.
	cells int
}

// GroupSchemaInfo reports the derived group schemas, for diagnostics. Phase 1 is report-only:
// these are what WOULD be stored columnar per group, not what is.
type GroupSchemaInfo struct {
	// Sampled is how many ads the derivation looked at, and BaseFields the size of the base
	// schema it derived against.
	Sampled    int `json:"sampled"`
	BaseFields int `json:"baseFields"`
	// BaseCells / TotalCells are attribute occurrences the base schema covers, out of all
	// occurrences in the sample -- the coverage the groups are measured against.
	BaseCells  int                `json:"baseCells"`
	TotalCells int                `json:"totalCells"`
	Groups     []GroupSchemaEntry `json:"groups,omitempty"`
}

// GroupSchemaEntry is one group in a report.
type GroupSchemaEntry struct {
	Attrs []string `json:"attrs"`
	// InFrac / NoneFrac / PartialFrac are the sample's split over this group. PartialFrac is
	// the one to watch: it is the fraction that would fall back to a row decode, and it is 0
	// by construction at derivation, so any growth is drift.
	InFrac      float64 `json:"inFrac"`
	NoneFrac    float64 `json:"noneFrac"`
	PartialFrac float64 `json:"partialFrac"`
	// Cells is the attribute occurrences this group would make columnar, and CellsFrac that
	// as a fraction of every occurrence in the sample.
	Cells     int     `json:"cells"`
	CellsFrac float64 `json:"cellsFrac"`
}

// defaultGroupSchemas is how many groups to derive. Measured coverage gain flattens after about
// four on both a live queue and a history table; each one costs a schema pointer and a block per
// segment block, so the default stops where the curve does.
const defaultGroupSchemas = 4

// deriveGroupSchemas derives up to k group schemas from a sample, against the base schema.
//
// The candidates are the attributes the base schema does NOT carry -- an attribute already in the
// base needs no group. They are grouped by identical presence across the sample, and the groups
// are ranked by the occurrences they would make columnar (members x holders), because that is the
// quantity a group exists to increase. Singleton groups are kept: a single attribute present in
// half the ads is still worth a column for that half, and it costs one schema pointer either way.
func (c *Collection) deriveGroupSchemas(samples [][]byte, base *adSchema, k int) []groupSchema {
	if len(samples) == 0 || k <= 0 {
		return nil
	}
	// present[id] is the set of sample indices carrying id, as a bitmap in a byte slice --
	// two attributes are grouped when these are equal, so the representation is also the key.
	stride := (len(samples) + 7) / 8
	present := map[uint32][]byte{}
	occ := 0
	for i, w := range samples {
		wire.Ad(w).ForEach(func(id uint32, _ []byte) bool {
			occ++
			if base != nil && base.hasField(id) {
				return true // already columnar in every record
			}
			b := present[id]
			if b == nil {
				b = make([]byte, stride)
				present[id] = b
			}
			b[i>>3] |= 1 << uint(i&7)
			return true
		})
	}
	// Group by identical presence bitmap.
	byPattern := map[string][]uint32{}
	for id, b := range present {
		byPattern[string(b)] = append(byPattern[string(b)], id)
	}
	var cands []cand
	for pat, ids := range byPattern {
		n := 0
		for i := 0; i < len(pat); i++ {
			n += popcount8(pat[i])
		}
		if n == 0 {
			continue
		}
		sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
		cands = append(cands, cand{ids: ids, holder: n, pat: []byte(pat)})
	}
	// Rank by cells recovered, then by size and lowest id so the order is total and stable
	// across runs on the same sample -- a group's identity is its members, and a report that
	// reshuffles equal-scoring groups looks like drift when nothing moved.
	sort.Slice(cands, func(a, b int) bool {
		ca, cb := len(cands[a].ids)*cands[a].holder, len(cands[b].ids)*cands[b].holder
		if ca != cb {
			return ca > cb
		}
		if len(cands[a].ids) != len(cands[b].ids) {
			return len(cands[a].ids) > len(cands[b].ids)
		}
		return cands[a].ids[0] < cands[b].ids[0]
	})
	// Optionally widen candidates by absorbing others whose presence pattern is nearly the same,
	// and keep the result only if the TOP-K of it recovers more than the top-k of the exact
	// patterns.
	//
	// The comparison has to be at the top-k level, because a merge is never profitable on its own:
	// both patterns are supersets of their intersection, so
	//
	//	apart = |P1|*a + |P2|*b  >=  |I|*(a+b) = merged
	//
	// always. What widening buys is FIT -- a fixed number of group slots covering more attributes,
	// where the patterns left outside the top k would otherwise be covered by nothing at all. So it
	// pays exactly when there are more distinct patterns than slots, which is the normal case, and
	// costs when there are not.
	if c.groupJac > 0 {
		if m := mergeNearPatterns(cands, len(samples), c.groupJac, c.groupMaxPartial()); len(m) > 0 {
			sortCands(m)
			if topKCells(m, k) > topKCells(cands, k) {
				cands = m
			}
		}
	}
	if len(cands) > k {
		cands = cands[:k]
	}

	out := make([]groupSchema, 0, len(cands))
	for _, cd := range cands {
		// Build the schema FIRST and take the membership ids from it. An attribute with no
		// dominant storable kind is dropped from the schema, and if it stayed in the membership
		// test a record could be "in" the group while one of its attributes was stored nowhere --
		// so the two must be the same set by construction, not by agreement.
		sch := buildAdSchemaFor(samples, cd.ids)
		if sch == nil || len(sch.fields) == 0 {
			continue
		}
		ids := make([]uint32, 0, len(sch.fields))
		for _, f := range sch.fields {
			ids = append(ids, f.id)
		}
		sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
		g := groupSchema{ids: ids, schema: sch}
		// Count the three states explicitly rather than trusting the grouping. They are
		// equal by construction here (identical presence => in or none, never partial), and
		// the point of measuring anyway is that the same counts, recomputed on a later
		// sample, are the drift signal -- so the code that produces them must be the code
		// that would show a nonzero partial.
		want := len(ids)
		idset := make(map[uint32]struct{}, want)
		for _, id := range ids {
			idset[id] = struct{}{}
		}
		for _, w := range samples {
			have := 0
			wire.Ad(w).ForEach(func(id uint32, _ []byte) bool {
				if _, ok := idset[id]; ok {
					have++
				}
				return true
			})
			switch have {
			case 0:
				g.none++
			case want:
				g.in++
			default:
				g.partial++
			}
		}
		g.cells = g.in * want
		out = append(out, g)
	}
	return out
}

func popcount8(b byte) int {
	n := 0
	for ; b != 0; b >>= 1 {
		n += int(b & 1)
	}
	return n
}

// buildAdSchemaFor builds a schema over exactly the named attribute ids, choosing each int's
// width from the sampled values as buildAdSchema does. Presence is not re-tested: membership is
// the caller's decision, and a group's members are present together by construction.
func buildAdSchemaFor(samples [][]byte, ids []uint32) *adSchema {
	want := make(map[uint32]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	kinds := map[uint32]*[5]int{}
	intVals := map[uint32][]int64{}
	for _, w := range samples {
		wire.Ad(w).ForEach(func(id uint32, node []byte) bool {
			if _, ok := want[id]; !ok {
				return true
			}
			k, lit := nodeKind(node)
			ks := kinds[id]
			if ks == nil {
				ks = &[5]int{}
				kinds[id] = ks
			}
			ks[k]++
			if k == akInt {
				intVals[id] = append(intVals[id], lit.Int)
			}
			return true
		})
	}
	var fields []adField
	for id := range want {
		ks := kinds[id]
		if ks == nil {
			continue
		}
		domK, domC := akNone, 0
		for k := akBool; k <= akString; k++ {
			if ks[k] > domC {
				domC, domK = ks[k], adKind(k)
			}
		}
		if domK == akNone {
			continue
		}
		f := adField{id: id, kind: domK}
		switch domK {
		case akInt:
			f.width, f.unsigned = chooseIntWidth(intVals[id], defaultGroupFit)
		case akReal:
			f.width = 8
		}
		fields = append(fields, f)
	}
	return layoutSchema(fields)
}

// defaultGroupFit is the int-width fit for a group column. It matches the base schema's Fit
// rather than the hot-column refit: a group is derived from presence, so nothing yet says which
// of its columns queries read.
const defaultGroupFit = 0.95

// hasField reports whether the schema carries an attribute id.
func (s *adSchema) hasField(id uint32) bool {
	if s == nil {
		return false
	}
	_, ok := s.byID[id]
	return ok
}

// groupSchemasFor derives the group schemas to build alongside base schema s.
//
// Pinned with the base schema at enable time, for the same reason the base schema is: every block
// of a segment is built against ONE set, so re-deriving between segments would leave earlier
// segments' selections unmatched. A change of groups is therefore a re-schema, not a refresh.
//
// Off by default. A group costs a schema pointer and a block per base block, and phase 1 exists to
// establish -- per table, on real data -- that a group's members keep co-occurring before anything
// commits storage to them. GroupSchemaCount is that opt-in.
func (c *Collection) groupSchemasFor(s *adSchema) []*colGroup {
	k := c.groupSchemaCount
	if k <= 0 {
		return nil
	}
	samples := c.normalizeSamples(c.CollectSamplesRecentN(maxDistinctSample))
	if len(samples) == 0 {
		return nil
	}
	// Only groups whose members have kept co-occurring across successive derivations. A group
	// derived once is a property of one sample; storage should follow evidence that it is a
	// property of the data. See stableGroupKeys for why the gate is over time rather than over a
	// holdout of the current sample.
	stable := c.stableGroupKeys(c.groupStabilityRuns())
	var out []*colGroup
	for _, g := range c.deriveGroupSchemas(samples, s, k) {
		if g.schema == nil || len(g.schema.fields) == 0 {
			continue
		}
		if stable != nil {
			var names []string
			for _, id := range g.ids {
				if n, ok := c.schemaFieldName(id); ok {
					names = append(names, n)
				}
			}
			sort.Strings(names)
			key := ""
			for _, n := range names {
				key += n + "\x00"
			}
			if !stable[key] {
				continue
			}
		}
		out = append(out, &colGroup{schema: g.schema, ids: g.ids})
	}
	return out
}

// groupStabilityRuns is how many consecutive derivations a group must appear in before its blocks
// are built. Default defaultGroupStabilityRuns; a value of 1 disables the gate (for a caller that
// has its own evidence, and for tests).
func (c *Collection) groupStabilityRuns() int {
	if c.groupStability != 0 {
		return c.groupStability
	}
	return defaultGroupStabilityRuns
}

// defaultGroupStabilityRuns is deliberately small. The cost of waiting is that a good group is not
// built for a few maintenance passes; the cost of not waiting is storage committed to a set that
// stops co-occurring, which then shows up as a partial rate and a slow path.
const defaultGroupStabilityRuns = 3

// cand is one candidate group: its member attribute ids, how many sampled ads hold the pattern,
// and the presence pattern itself (one bit per sampled ad).
type cand struct {
	ids    []uint32
	holder int
	pat    []byte
}

// mergeNearPatterns widens candidates by absorbing others whose presence pattern is NEARLY the
// same, which is what buys coverage beyond exact co-occurrence.
//
// Exact grouping keeps the partial state -- an ad holding some but not all of a group -- impossible
// within the sample. That is a strong guarantee and it leaves coverage on the table: measured on a
// production queue, relaxing to a Jaccard of 0.99 recovered 17% more columnar cells at a partial
// rate of 0.071%.
//
// Partial is not free. A partial record's values are not in the group block, so reading one of its
// group attributes costs a base cold-tail lookup, and if the columnar path is F times faster a
// partial fraction p forfeits roughly p*F of the benefit -- so p wants to be well under 1/F. That
// is what maxPartial bounds, and a merge that would breach it is rejected rather than scored: a
// group is not worth having at any coverage if reading it is usually the slow way.
//
// Greedy from the highest-ranked candidate down, so a widened group grows around the pattern that
// already recovers the most, and each absorbed pattern leaves the pool.
//
// MEASURED ACROSS SNAPSHOTS. Widening holds up, but not in the way the in-sample numbers suggest.
// Deriving on one production snapshot and scoring against another taken hours later:
//
//	coverage   exact 16.46% -> 23.53%      widened 18.86% -> 25.14%
//	partial    exact  0.000% -> 0.000%     widened worst group 0.075% -> 45.792%
//
// Widening still recovers MORE than exact on the holdout. The partial rate is not a penalty
// against that: a partial ad reads its group attributes from the base block's cold tail, which is
// exactly where they would be if the group did not exist -- so partial is benefit not realized,
// never harm. What the holdout does say is that an in-sample partial ceiling predicts nothing,
// because a merge is fitted to the sample that suggested it: the group that read 0.075% when
// derived read 45.8% later, when one member's presence moved the opposite way from the rest
// (ReqPeriodicRelease rose 56.5% -> 72.0% while the other three fell to 26.3%).
//
// Two consequences worth keeping. The exception lookup must not assume a small list -- it binary
// searches for this reason. And widened member sets are sample-dependent: across three snapshots
// 3 of 4 EXACT groups reproduced their member set and 0 of 4 widened ones did, so the stability
// gate refuses to build any widened group. That gate is currently the binding constraint on this
// feature, not the partial rate.
func mergeNearPatterns(cands []cand, nSamples int, minJaccard, maxPartial float64) []cand {
	used := make([]bool, len(cands))
	out := make([]cand, 0, len(cands))
	for i := range cands {
		if used[i] {
			continue
		}
		used[i] = true
		merged := cands[i]
		// The union and intersection of the absorbed patterns give the merged group's counts
		// directly: intersection = ads holding EVERY member, union minus that = partial.
		union := append([]byte(nil), merged.pat...)
		inter := append([]byte(nil), merged.pat...)
		for j := i + 1; j < len(cands); j++ {
			if used[j] || jaccardBits(merged.pat, cands[j].pat) < minJaccard {
				continue
			}
			nu := orBytes(union, cands[j].pat)
			ni := andBytes(inter, cands[j].pat)
			partial := popcountBytes(nu) - popcountBytes(ni)
			if nSamples > 0 && float64(partial)/float64(nSamples) > maxPartial {
				continue // this absorption would cost more slow-path reads than it buys columns
			}
			union, inter = nu, ni
			merged.ids = append(merged.ids, cands[j].ids...)
			used[j] = true
		}
		if len(merged.ids) > len(cands[i].ids) {
			sort.Slice(merged.ids, func(a, b int) bool { return merged.ids[a] < merged.ids[b] })
			merged.pat = inter // the ads holding every member of the widened group
			merged.holder = popcountBytes(inter)
		}
		out = append(out, merged)
	}
	return out
}

// jaccardBits is |a AND b| / |a OR b| over two presence bitmaps.
func jaccardBits(a, b []byte) float64 {
	u := popcountBytes(orBytes(a, b))
	if u == 0 {
		return 0
	}
	return float64(popcountBytes(andBytes(a, b))) / float64(u)
}

func orBytes(a, b []byte) []byte {
	out := make([]byte, max(len(a), len(b)))
	copy(out, a)
	for i := range b {
		if i < len(out) {
			out[i] |= b[i]
		}
	}
	return out
}

func andBytes(a, b []byte) []byte {
	n := min(len(a), len(b))
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = a[i] & b[i]
	}
	return out
}

func popcountBytes(b []byte) int {
	n := 0
	for _, x := range b {
		n += popcount8(x)
	}
	return n
}

// groupMaxPartial bounds the fraction of ads that may hold only part of a widened group.
func (c *Collection) groupMaxPartial() float64 {
	if c.groupMaxPart > 0 {
		return c.groupMaxPart
	}
	return defaultGroupMaxPartial
}

// defaultGroupMaxPartial is the partial-rate ceiling for a widened group. A tenth of a percent
// forfeits well under 1% of the benefit at the columnar speedups measured here; the ceiling exists
// to stop a merge trading a large slow-path fraction for a little coverage.
const defaultGroupMaxPartial = 0.001

// sortCands orders candidates by the cells they recover, then by size and lowest id so the order is
// total and stable across runs on the same sample.
func sortCands(cands []cand) {
	sort.Slice(cands, func(a, b int) bool {
		ca, cb := len(cands[a].ids)*cands[a].holder, len(cands[b].ids)*cands[b].holder
		if ca != cb {
			return ca > cb
		}
		if len(cands[a].ids) != len(cands[b].ids) {
			return len(cands[a].ids) > len(cands[b].ids)
		}
		return cands[a].ids[0] < cands[b].ids[0]
	})
}

// topKCells is what the best k of these candidates would make columnar.
func topKCells(cands []cand, k int) int {
	total := 0
	for i := 0; i < len(cands) && i < k; i++ {
		total += len(cands[i].ids) * cands[i].holder
	}
	return total
}
