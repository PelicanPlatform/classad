package collections

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Auto-index detection, step 1 (advisory). Two signals drive index suggestions:
//
//   - Query demand: how often queries filter on each attribute, split by operator
//     class (equality/membership vs range). Recorded cheaply on every Query from
//     the planner's probes, so it accrues even before any index exists.
//   - Data profile: a sample of live ads, giving each attribute's presence, its
//     dominant literal kind (string vs numeric vs non-literal), and a bounded
//     distinct-value count.
//
// SuggestIndexes combines them into recommended CategoricalAttrs / ValueAttrs.
// Applying suggestions is left to the caller (via New); dynamic add/drop is a
// later step.

// demandCounts tallies how often queries probe one attribute.
type demandCounts struct {
	eq  atomic.Int64 // == / != / in
	rng atomic.Int64 // < <= > >=
	// reads counts how often a query or match READS this attribute (its Requirements /
	// Rank / WHERE reference it), which drives the hot set -- an attribute the workload
	// actually evaluates is worth front-loading, unlike one that is merely present in
	// every ad (which would make all attributes look equally "hot").
	reads atomic.Int64
}

// demandTracker records per-attribute query demand, keyed by folded (case-
// insensitive) attribute name so it needs no interning and is safe for concurrent
// queries.
type demandTracker struct {
	m sync.Map // foldedName(string) -> *demandCounts
}

func newDemandTracker() *demandTracker { return &demandTracker{} }

func (d *demandTracker) record(probes []vm.Probe) {
	for _, p := range probes {
		v, _ := d.m.LoadOrStore(strings.ToLower(p.Attr), &demandCounts{})
		c := v.(*demandCounts)
		switch p.Op {
		case "<", "<=", ">", ">=":
			c.rng.Add(1)
		default: // ==, !=, in
			c.eq.Add(1)
		}
	}
}

// recordReads tallies one access to each named attribute (a query/match evaluated a
// reference to it), for hot-set ranking.
func (d *demandTracker) recordReads(names []string) {
	for _, n := range names {
		v, _ := d.m.LoadOrStore(strings.ToLower(n), &demandCounts{})
		v.(*demandCounts).reads.Add(1)
	}
}

// maxDistinctSample bounds the per-attribute distinct-value set kept while
// profiling, so a high-cardinality attribute cannot blow up memory. Beyond it the
// profile is marked Capped and the exact count is a lower bound.
const maxDistinctSample = 4096

// attrProfile is one attribute's shape across the sampled ads.
type attrProfile struct {
	name     string // first-seen original-case spelling (for display)
	present  int
	strCount int
	numCount int // int/real/bool literals (index-normalized to float64)
	other    int // undefined/error literals, or non-literal (expression/list/record)
	distinct map[string]struct{}
	capped   bool
}

func (p *attrProfile) addDistinct(k string) {
	if p.capped {
		return
	}
	if len(p.distinct) >= maxDistinctSample {
		p.capped = true
		return
	}
	p.distinct[k] = struct{}{}
}

// profileAttrs samples up to sampleMax live ads and profiles every attribute seen,
// keyed by folded (case-insensitive) name so it joins directly with the name-keyed
// demand map. ForEachNamed reads both encodings -- interned ids (in-memory store) and
// inline names (persistent store) -- so profiling works on a recovered collection,
// which carries no intern ids (the id-based ForEach would have yielded nothing).
func (c *Collection) profileAttrs(sampleMax int) map[string]*attrProfile {
	profiles := map[string]*attrProfile{}
	for _, w := range c.CollectSamples(sampleMax) {
		wire.Ad(w).ForEachNamed(c.intern, func(name string, node []byte) bool {
			fold := strings.ToLower(name)
			p := profiles[fold]
			if p == nil {
				p = &attrProfile{name: name, distinct: map[string]struct{}{}}
				profiles[fold] = p
			}
			p.present++
			lit, ok := wire.LiteralValue(node)
			if !ok {
				p.other++ // expression / list / record
				return true
			}
			switch lit.Kind {
			case wire.LitString:
				p.strCount++
				p.addDistinct(strings.ToLower(lit.Str))
			case wire.LitInt, wire.LitReal, wire.LitBool:
				p.numCount++
				p.addDistinct(numKey(lit))
			default: // undefined / error literal
				p.other++
			}
			return true
		})
	}
	return profiles
}

// numKey canonicalizes a numeric literal to the float64 the value index would key
// on (matching int/real/bool normalization), for distinct counting.
func numKey(lit wire.Literal) string {
	var f float64
	switch lit.Kind {
	case wire.LitInt:
		f = float64(lit.Int)
	case wire.LitReal:
		f = lit.Real
	case wire.LitBool:
		if lit.Bool {
			f = 1
		}
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// IndexSuggestion recommends indexing one attribute, with the rationale (the two
// signals) so the decision is explainable.
type IndexSuggestion struct {
	Attr string // canonical (first-seen) attribute name
	Kind string // "categorical" (string equality/membership) or "value" (numeric + range)

	QueriesEq    int64 // equality/membership probes observed
	QueriesRange int64 // range probes observed

	SampledPresent int     // sampled ads with this attribute present
	StringFrac     float64 // fraction of present values that are string literals
	NumericFrac    float64 // fraction that are numeric literals
	DistinctValues int     // distinct values in the sample (a lower bound if Capped)
	Capped         bool
}

// suggestion policy thresholds.
const (
	// a value must be this dominant a literal kind for the attribute to be indexable
	// as that kind; below it the attribute is too mixed / too many exceptions.
	kindDominanceFrac = 0.8

	// defaultBudgetSlackBytes is the absolute leeway over the index memory high
	// watermark before AutoTune trims -- so a small overage does not cause churn. Used
	// when AutoTuneOptions.BudgetSlackBytes is 0.
	defaultBudgetSlackBytes = 10 << 20 // 10 MiB
)

// RecordDemand notes the attributes a constraint filters on (for SuggestIndexes)
// without running a scan. Query records demand automatically; this is for callers that
// filter outside the normal scan path -- e.g. cross-table MATCH applying a resource-side
// (WHERE TARGET) constraint to already-matched candidates -- so those attributes still
// surface as index suggestions.
func (c *Collection) RecordDemand(probes []vm.Probe) { c.demand.record(probes) }

// SuggestIndexes recommends value/categorical indexes from observed query demand
// and a sample of up to sampleMax live ads. It is advisory: apply the returned
// CategoricalAttrs/ValueAttrs via New (or a future dynamic Reindex). Attributes
// already indexed are not re-suggested. Results are ordered by demand, most first.
func (c *Collection) SuggestIndexes(sampleMax int) []IndexSuggestion {
	profiles := c.profileAttrs(sampleMax)
	var out []IndexSuggestion
	c.demand.m.Range(func(k, v any) bool {
		d := v.(*demandCounts)
		eq, rng := d.eq.Load(), d.rng.Load()
		if eq == 0 && rng == 0 {
			return true
		}
		fold := k.(string) // demand keys are already folded
		p := profiles[fold]
		if p == nil || p.present == 0 {
			return true // queried a name no sampled ad had -> nothing to index
		}
		if c.alreadyIndexedName(fold) {
			return true
		}
		strFrac := float64(p.strCount) / float64(p.present)
		numFrac := float64(p.numCount) / float64(p.present)
		base := IndexSuggestion{
			Attr: p.name, QueriesEq: eq, QueriesRange: rng,
			SampledPresent: p.present, StringFrac: strFrac, NumericFrac: numFrac,
			DistinctValues: len(p.distinct), Capped: p.capped,
		}
		switch {
		case strFrac >= kindDominanceFrac && eq > 0:
			base.Kind = "categorical"
			out = append(out, base)
		case numFrac >= kindDominanceFrac && (eq > 0 || rng > 0):
			base.Kind = "value"
			out = append(out, base)
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool {
		di := out[i].QueriesEq + out[i].QueriesRange
		dj := out[j].QueriesEq + out[j].QueriesRange
		if di != dj {
			return di > dj
		}
		return out[i].Attr < out[j].Attr
	})
	return out
}

// DropSuggestion recommends removing one configured index, with the rationale.
type DropSuggestion struct {
	Attr   string // canonical attribute name
	Kind   string // "categorical" or "value" (how it is currently indexed)
	Reason string // "unused" (never queried) or "low-cardinality" (no pruning power)

	QueriesEq      int64
	QueriesRange   int64
	SampledPresent int
	DistinctValues int
	Capped         bool
}

// SuggestDrops recommends configured indexes to remove, from observed query demand
// and a sample of up to sampleMax live ads. It flags two cases: an index no query
// has ever filtered on ("unused"), and one whose sampled values are effectively
// constant ("low-cardinality", ≤1 distinct value — the index cannot prune). It is
// advisory: apply via DropIndex. Demand is cumulative since the collection was
// created, so give a workload time to run before trusting "unused".
func (c *Collection) SuggestDrops(sampleMax int) []DropSuggestion {
	spec := c.spec.Load()
	if !spec.any() {
		return nil
	}
	profiles := c.profileAttrs(sampleMax)
	var out []DropSuggestion
	// A configured index's attribute name in either mode: inline specs key by folded
	// name (reverse nameToID), interned specs by id (resolve via intern).
	foldName := func(id uint32) (string, bool) {
		if spec.inline {
			for n, sid := range spec.nameToID {
				if sid == id {
					return n, true // n is folded
				}
			}
			return "", false
		}
		if nm, ok := c.intern.Name(id); ok {
			return strings.ToLower(nm), true
		}
		return "", false
	}
	consider := func(id uint32, kind string) {
		fold, ok := foldName(id)
		if !ok {
			return
		}
		p := profiles[fold]
		display := fold
		if p != nil && p.name != "" {
			display = p.name
		}
		var eq, rng int64
		if v, ok := c.demand.m.Load(fold); ok {
			d := v.(*demandCounts)
			eq, rng = d.eq.Load(), d.rng.Load()
		}
		ds := DropSuggestion{Attr: display, Kind: kind, QueriesEq: eq, QueriesRange: rng}
		if p != nil {
			ds.SampledPresent, ds.DistinctValues, ds.Capped = p.present, len(p.distinct), p.capped
		}
		switch {
		case eq == 0 && rng == 0:
			ds.Reason = "unused"
		case ds.SampledPresent > 0 && ds.DistinctValues <= 1 && !ds.Capped:
			ds.Reason = "low-cardinality"
		default:
			return
		}
		out = append(out, ds)
	}
	for _, id := range spec.catIDs {
		consider(id, "categorical")
	}
	for _, id := range spec.valIDs {
		consider(id, "value")
	}
	// Unused before low-cardinality, then by name — stable and easy to read.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Reason != out[j].Reason {
			return out[i].Reason == "unused"
		}
		return out[i].Attr < out[j].Attr
	})
	return out
}

// AutoTuneOptions configures AutoTune.
type AutoTuneOptions struct {
	// SampleMax caps the ads profiled. Default maxDistinctSample.
	SampleMax int
	// MinDemand is the minimum equality+range probe count for an attribute to be
	// added as an index. Default 1 (any observed demand).
	MinDemand int64
	// DropUnused, if set, drops AUTO-created indexes that no query has filtered on
	// ("unused"). Off by default so AutoTune never removes an index a workload has
	// simply not exercised yet. Human-created indexes and low-cardinality indexes are
	// never auto-dropped (SuggestDrops reports them for manual review).
	DropUnused bool
	// BudgetHighFrac / BudgetLowFrac, when BudgetHighFrac > 0, bound index memory as a
	// fraction of the live data bytes: AutoTune stops adding demand-driven indexes once
	// index bytes reach the high mark, and trims the least-used AUTO indexes (never
	// human-created ones) until index bytes fall below BudgetLowFrac. This is a high/low-
	// watermark with hysteresis: grow until over the high mark, trim back to the low
	// mark. 0 disables the budget (unbounded). BudgetLowFrac defaults to 0.7*BudgetHighFrac.
	BudgetHighFrac float64
	BudgetLowFrac  float64
	// BudgetSlackBytes is absolute leeway on top of the high watermark: index bytes may
	// exceed BudgetHighFrac by up to this many bytes before any trim triggers, so a small
	// overage (or a small database, where the percentage is tiny in absolute terms) does
	// not cause churn. 0 uses defaultBudgetSlackBytes (10 MiB); set a negative value for
	// no slack (trim exactly at the fraction).
	BudgetSlackBytes int64
	// Reindex, if set, calls Reindex after applying changes so the new/removed
	// indexes take effect immediately instead of at the caller's next Reindex.
	Reindex bool
}

// AutoTuneChange records one add/drop AutoTune applied.
type AutoTuneChange struct {
	Attr, Kind, Action, Reason string // Action: "add" | "drop"
}

// AutoTuneResult reports what AutoTune changed.
type AutoTuneResult struct {
	Changes []AutoTuneChange
	Changed bool
}

// AutoTune applies the advisory signals: it adds the demand-driven indexes from
// SuggestIndexes (those at or above MinDemand) and, if DropUnused is set, drops the
// unused ones from SuggestDrops. It is the "make it so" companion to the SuggestX
// methods — call it on whatever schedule you like (e.g. alongside Reindex). All
// suggestions are computed from one consistent snapshot before any change is
// applied.
func (c *Collection) AutoTune(opts AutoTuneOptions) AutoTuneResult {
	if opts.SampleMax <= 0 {
		opts.SampleMax = maxDistinctSample
	}
	minDemand := opts.MinDemand
	if minDemand < 1 {
		minDemand = 1
	}
	adds := c.SuggestIndexes(opts.SampleMax)
	var drops []DropSuggestion
	if opts.DropUnused {
		drops = c.SuggestDrops(opts.SampleMax)
	}

	slack := opts.BudgetSlackBytes
	if slack == 0 {
		slack = defaultBudgetSlackBytes
	} else if slack < 0 {
		slack = 0
	}

	var res AutoTuneResult
	// Growth is gated by the memory budget: skip adds when index bytes already reach the
	// high watermark (plus the absolute slack); the trim phase below brings us back to low.
	underBudget := true
	if opts.BudgetHighFrac > 0 {
		sz := c.IndexSizes()
		highBytes := int64(opts.BudgetHighFrac*float64(sz.DataBytes)) + slack
		underBudget = sz.DataBytes <= 0 || sz.TotalBytes < highBytes
	}
	var cat, val []string
	if underBudget {
		for _, s := range adds {
			if s.QueriesEq+s.QueriesRange < minDemand {
				continue
			}
			if s.Kind == "categorical" {
				cat = append(cat, s.Attr)
			} else {
				val = append(val, s.Attr)
			}
			res.Changes = append(res.Changes, AutoTuneChange{s.Attr, s.Kind, "add", "demand"})
		}
		if len(cat) > 0 || len(val) > 0 {
			res.Changed = c.addIndexAuto(cat, val) || res.Changed
		}
	}

	var dropNames []string
	for _, d := range drops {
		if d.Reason != "unused" {
			continue // never auto-drop on low-cardinality alone
		}
		if !c.spec.Load().isAutoName(c, d.Attr) {
			continue // never auto-drop a human-created index
		}
		dropNames = append(dropNames, d.Attr)
		res.Changes = append(res.Changes, AutoTuneChange{d.Attr, d.Kind, "drop", d.Reason})
	}
	if len(dropNames) > 0 {
		res.Changed = c.DropIndex(dropNames...) || res.Changed
	}

	// Budget trim: if over the high watermark (plus slack), drop the least-valuable AUTO
	// indexes (fewest queries, then largest bytes) until under the low watermark.
	if opts.BudgetHighFrac > 0 {
		lowFrac := opts.BudgetLowFrac
		if lowFrac <= 0 {
			lowFrac = 0.7 * opts.BudgetHighFrac
		}
		for _, d := range c.budgetTrim(opts.BudgetHighFrac, lowFrac, slack) {
			res.Changes = append(res.Changes, AutoTuneChange{d.Attr, d.Kind, "drop", "over-budget"})
			res.Changed = c.DropIndex(d.Attr) || res.Changed
		}
	}

	if opts.Reindex && res.Changed {
		c.Reindex()
	}
	return res
}

// budgetTrim selects AUTO indexes to drop so total index bytes fall from above highFrac
// to below lowFrac of the live data bytes. It never selects a human-created index, and
// picks the least-valuable first: fewest observed queries, then largest bytes (free the
// most memory soonest). It only selects; the caller applies the drops.
func (c *Collection) budgetTrim(highFrac, lowFrac float64, slackBytes int64) []IndexSize {
	sz := c.IndexSizes()
	if sz.DataBytes <= 0 {
		return nil
	}
	highBytes := int64(highFrac*float64(sz.DataBytes)) + slackBytes
	if sz.TotalBytes <= highBytes {
		return nil // within the high watermark plus slack: nothing to trim
	}
	target := int64(lowFrac * float64(sz.DataBytes))
	// Auto indexes only, ranked least-valuable first.
	var cand []IndexSize
	for _, s := range sz.PerIndex {
		if s.Auto {
			cand = append(cand, s)
		}
	}
	demandOf := func(attr string) int64 {
		if v, ok := c.demand.m.Load(strings.ToLower(attr)); ok {
			d := v.(*demandCounts)
			return d.eq.Load() + d.rng.Load()
		}
		return 0
	}
	sort.Slice(cand, func(i, j int) bool {
		di, dj := demandOf(cand[i].Attr), demandOf(cand[j].Attr)
		if di != dj {
			return di < dj // fewest queries first
		}
		return cand[i].Bytes > cand[j].Bytes // then free the most bytes
	})
	remaining := sz.TotalBytes
	var drop []IndexSize
	for _, s := range cand {
		if remaining <= target {
			break
		}
		drop = append(drop, s)
		remaining -= s.Bytes
	}
	return drop
}

// alreadyIndexed reports whether attr id already has a configured index.
func (c *Collection) alreadyIndexed(id uint32) bool {
	spec := c.spec.Load()
	if spec == nil {
		return false
	}
	if _, ok := spec.cat[id]; ok {
		return true
	}
	_, ok := spec.val[id]
	return ok
}

// alreadyIndexedName reports whether the folded attribute name is already indexed, in
// either index mode: an inline spec (persistent store) keys indexes by folded name via
// nameToID, an interned spec by id via the intern table.
func (c *Collection) alreadyIndexedName(fold string) bool {
	spec := c.spec.Load()
	if spec == nil {
		return false
	}
	var id uint32
	var ok bool
	if spec.inline {
		id, ok = spec.nameToID[fold]
	} else {
		id, ok = c.intern.LookupID(fold)
	}
	if !ok {
		return false
	}
	if _, ok := spec.cat[id]; ok {
		return true
	}
	_, ok = spec.val[id]
	return ok
}
