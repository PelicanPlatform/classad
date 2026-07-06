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

// maxDistinctSample bounds the per-attribute distinct-value set kept while
// profiling, so a high-cardinality attribute cannot blow up memory. Beyond it the
// profile is marked Capped and the exact count is a lower bound.
const maxDistinctSample = 4096

// attrProfile is one attribute's shape across the sampled ads.
type attrProfile struct {
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
// keyed by interned id.
func (c *Collection) profileAttrs(sampleMax int) map[uint32]*attrProfile {
	profiles := map[uint32]*attrProfile{}
	for _, w := range c.CollectSamples(sampleMax) {
		wire.Ad(w).ForEach(func(id uint32, node []byte) bool {
			p := profiles[id]
			if p == nil {
				p = &attrProfile{distinct: map[string]struct{}{}}
				profiles[id] = p
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
)

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
		id, ok := c.intern.LookupID(k.(string))
		if !ok {
			return true // queried a name no ad ever had -> nothing to index
		}
		if c.alreadyIndexed(id) {
			return true
		}
		p := profiles[id]
		if p == nil || p.present == 0 {
			return true
		}
		strFrac := float64(p.strCount) / float64(p.present)
		numFrac := float64(p.numCount) / float64(p.present)
		name, _ := c.intern.Name(id)
		base := IndexSuggestion{
			Attr: name, QueriesEq: eq, QueriesRange: rng,
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
	consider := func(id uint32, kind string) {
		name, ok := c.intern.Name(id)
		if !ok {
			return
		}
		var eq, rng int64
		if v, ok := c.demand.m.Load(strings.ToLower(name)); ok {
			d := v.(*demandCounts)
			eq, rng = d.eq.Load(), d.rng.Load()
		}
		ds := DropSuggestion{Attr: name, Kind: kind, QueriesEq: eq, QueriesRange: rng}
		if p := profiles[id]; p != nil {
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
	// DropUnused, if set, drops configured indexes that no query has filtered on
	// ("unused"). Off by default so AutoTune never removes an index a workload has
	// simply not exercised yet. Low-cardinality indexes are never auto-dropped
	// (SuggestDrops reports them for manual review).
	DropUnused bool
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

	var res AutoTuneResult
	var cat, val []string
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
		res.Changed = c.AddIndex(cat, val) || res.Changed
	}

	var dropNames []string
	for _, d := range drops {
		if d.Reason != "unused" {
			continue // never auto-drop on low-cardinality alone
		}
		dropNames = append(dropNames, d.Attr)
		res.Changes = append(res.Changes, AutoTuneChange{d.Attr, d.Kind, "drop", d.Reason})
	}
	if len(dropNames) > 0 {
		res.Changed = c.DropIndex(dropNames...) || res.Changed
	}

	if opts.Reindex && res.Changed {
		c.Reindex()
	}
	return res
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
