package collections

import (
	"math"
	"slices"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// Demand-driven composite indexes: counting the attribute TUPLES a workload pins together,
// and creating a composite over the ones it keeps pinning.
//
// The per-attribute demand counters cannot see this. `ClusterId == 7 && ProcId == 3`
// increments ClusterId and ProcId exactly as `ClusterId == 7` and `ProcId == 3` issued an
// hour apart would, and the thing a composite is for -- the two arriving in ONE conjunction,
// where neither posting is anywhere near as narrow as their intersection -- leaves no trace.
// So a second counter is kept, keyed by the tuple rather than the attribute, and fed from
// the same place: every call site that records demand hands record() ONE conjunction's
// probes (the top-level && spine, or one group of a DNF plan), which is exactly the unit a
// composite answers.
//
// WHAT COUNTS. A composite answers an equality on EVERY one of its components and nothing
// else (see composite.go), so only a probe pinning its attribute to a single value counts
// towards a tuple. A range, a `!=`, a `=?=` and a multi-value `in` are demand for the
// component's own index and none at all for the tuple. The tuple is then the SET of
// attributes so pinned -- sorted, so that two queries spelling the same pair in either order
// accrue to one counter -- and nothing narrower: a conjunction pinning three attributes
// demands the triple, not also each of its three pairs. Counting subsets would turn one
// query into 2^n counters and invent demand for tuples no query asked for; a workload that
// wants the pair issues the pair.
//
// THE AWKWARD CONSEQUENCE of that, and of maxCompositeAttrs: a conjunction pinning more
// attributes than a composite can hold contributes NOTHING, not even a truncated tuple,
// because choosing which components to drop would be choosing a tuple on the query's behalf.
// Such a conjunction is already far past the width at which the demand to justify an index
// could be observed.
//
// DECAY AND PERSISTENCE are the per-attribute counters' (demand_persist.go), unchanged and
// shared: the same half-life, the same floor, the same demand.json, scaled on the same
// maintenance pass. A tuple's demand means "recent demand, as of now" for the same reason an
// attribute's does.

// maxTupleKeys bounds the tuple map. The per-attribute map is bounded by the schema -- there
// are only so many attributes -- but the number of distinct conjunction SHAPES is bounded by
// the workload's imagination, and a query generator emitting a fresh shape per request would
// otherwise grow the map without limit. Past the cap, counting continues for the tuples
// already known and new ones are ignored, which favours the shapes with the most observation
// behind them. On a collection that checkpoints, decay frees room as shapes fall below the
// floor; on an in-memory one, which never checkpoints and so never decays, it does not, and
// a workload that changes shape after the cap is reached is not followed. That is the same
// bargain the per-attribute counters already make with a collection that has no directory.
const maxTupleKeys = 4096

// tupleKeySep delimits the folded component names inside a tuple key. NUL occurs in neither
// an attribute name nor the dotted spelling of a path, so the join is unambiguous and the
// components split straight back out of the key -- which is why a tuple needs no parallel
// slice of names anywhere.
const tupleKeySep = "\x00"

// tupleCounts is one tuple's demand. Only equalities count towards a tuple (see above), so
// unlike demandCounts there is nothing to split by operator class.
type tupleCounts struct {
	eq atomic.Int64
}

// tupleEqProbe reports whether a probe pins its attribute to exactly one value, which is the
// only shape a composite component can answer. It is compositeEqProbe's rule one layer up:
// that one judges a probe the planner has already folded against an index, this one judges
// the raw probe, because demand accrues whether or not an index exists yet.
func tupleEqProbe(p vm.Probe) bool { return isEqOp(p.Op) && len(p.Vals) == 1 }

// recordTuple counts the tuple one conjunction pins, if it pins one a composite could serve.
// Called from record, so every path that reports per-attribute demand reports tuple demand
// too, from the same probes.
func (d *demandTracker) recordTuple(probes []vm.Probe) {
	if len(probes) < 2 {
		return
	}
	names := make([]string, 0, len(probes))
	for _, p := range probes {
		if !tupleEqProbe(p) {
			continue
		}
		fold := strings.ToLower(p.Attr)
		if slices.Contains(names, fold) {
			// The same attribute pinned twice (`A == 1 && A == 2`, which nothing satisfies,
			// or a repeated conjunct) is one component, not two.
			continue
		}
		names = append(names, fold)
		if len(names) > maxCompositeAttrs {
			return // too wide to be a composite, and no subset of it is what was asked for
		}
	}
	if len(names) < 2 {
		return
	}
	sort.Strings(names)
	key := strings.Join(names, tupleKeySep)
	v, ok := d.tuples.Load(key)
	if !ok {
		// Load before LoadOrStore, as recordReads does: after the first conjunction of a
		// shape every later one hits, and LoadOrStore would allocate a counter on each only
		// to discard it. The cap is checked here rather than on the hit path for the same
		// reason -- a known tuple costs one map read.
		if d.tupleN.Load() >= maxTupleKeys {
			return
		}
		var loaded bool
		v, loaded = d.tuples.LoadOrStore(key, &tupleCounts{})
		if !loaded {
			d.tupleN.Add(1)
		}
	}
	v.(*tupleCounts).eq.Add(1) //nolint:errcheck // the tuple map is keyed by tuple key and only ever holds *tupleCounts
}

// compositeDemand is the demand a set of components can serve: every observed conjunction
// whose pinned tuple CONTAINS them, not only the ones that pin exactly them.
//
// That distinction is the whole difference between a useful drop rule and one that discards
// a composite doing its job. A composite over (ClusterId, ProcId) covers `ClusterId == 7 &&
// ProcId == 3 && Owner == "alice"` -- it answers two of the three probes and the third is
// intersected in -- so a workload that only ever issues the triple keeps the pair busy while
// leaving the pair's own counter at zero. Counting the exact tuple alone would read that as
// unused.
//
// folds must be sorted, as the keys are, so containment is a merge.
func (d *demandTracker) compositeDemand(folds []string) int64 {
	var total int64
	d.tuples.Range(func(k, v any) bool {
		have := strings.Split(k.(string), tupleKeySep) //nolint:errcheck // tuple keys are strings
		if containsSorted(have, folds) {
			total += v.(*tupleCounts).eq.Load() //nolint:errcheck // the tuple map only ever holds *tupleCounts
		}
		return true
	})
	return total
}

// containsSorted reports whether the sorted set have contains every element of the sorted
// set want.
func containsSorted(have, want []string) bool {
	i := 0
	for _, w := range want {
		for i < len(have) && have[i] < w {
			i++
		}
		if i >= len(have) || have[i] != w {
			return false
		}
		i++
	}
	return true
}

// --- checkpointing (the per-attribute counters' file, same decay) --------------------

// persistedTuple is one tuple's entry in demand.json. The components are a list rather than
// a delimited map key so the file never has to agree with the in-memory key encoding, and so
// a reader can see what the tuple is.
type persistedTuple struct {
	Attrs []string `json:"attrs"` // folded component names, sorted (the canonical tuple)
	Eq    float64  `json:"eq"`
}

// decayTuples scales the tuple counters, forgetting the ones that fall below the floor --
// the per-attribute decay, applied to the other map. Charged by the same caller with the
// same factor, so a tuple and its components fade together.
func (d *demandTracker) decayTuples(f float64) {
	var dead []string
	d.tuples.Range(func(k, v any) bool {
		c := v.(*tupleCounts) //nolint:errcheck // the tuple map only ever holds *tupleCounts
		eq := float64(c.eq.Load()) * f
		if eq < demandFloor {
			dead = append(dead, k.(string)) //nolint:errcheck // tuple keys are strings
			return true
		}
		c.eq.Store(int64(math.Round(eq)))
		return true
	})
	for _, k := range dead {
		d.tuples.Delete(k)
		d.tupleN.Add(-1)
	}
}

// snapshotTuples renders the tuple counters for checkpointing, sorted so the file does not
// churn between saves that changed nothing (the per-attribute counters get that for free
// from Go's sorted marshalling of a map).
func (d *demandTracker) snapshotTuples() []persistedTuple {
	var out []persistedTuple
	d.tuples.Range(func(k, v any) bool {
		if eq := float64(v.(*tupleCounts).eq.Load()); eq >= demandFloor { //nolint:errcheck // the tuple map only ever holds *tupleCounts
			out = append(out, persistedTuple{Attrs: strings.Split(k.(string), tupleKeySep), Eq: eq}) //nolint:errcheck // tuple keys are strings
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool {
		return strings.Join(out[i].Attrs, tupleKeySep) < strings.Join(out[j].Attrs, tupleKeySep)
	})
	return out
}

// restoreTuples adds a checkpoint's tuple counters, decayed by f for the time since it was
// written. ADDED, like the per-attribute restore, so a restore is safe on a live tracker.
func (d *demandTracker) restoreTuples(tuples []persistedTuple, f float64) {
	for _, pt := range tuples {
		eq := pt.Eq * f
		if eq < demandFloor || len(pt.Attrs) < 2 || len(pt.Attrs) > maxCompositeAttrs {
			continue
		}
		// The checkpoint is written sorted, but it is a file: sort again rather than trust
		// it, since an out-of-order entry would key a second counter for a tuple already
		// held and both would read as half the demand.
		attrs := append([]string(nil), pt.Attrs...)
		sort.Strings(attrs)
		key := strings.Join(attrs, tupleKeySep)
		v, ok := d.tuples.Load(key)
		if !ok {
			if d.tupleN.Load() >= maxTupleKeys {
				continue
			}
			var loaded bool
			v, loaded = d.tuples.LoadOrStore(key, &tupleCounts{})
			if !loaded {
				d.tupleN.Add(1)
			}
		}
		v.(*tupleCounts).eq.Add(int64(math.Round(eq))) //nolint:errcheck // the tuple map only ever holds *tupleCounts
	}
}

// --- suggestions ---------------------------------------------------------------------

// CompositeSuggestion recommends a composite index over an attribute tuple, with the
// rationale (how many conjunctions pinned exactly that tuple) so the decision is
// explainable, as IndexSuggestion is for a single attribute.
type CompositeSuggestion struct {
	// Attrs is the tuple in canonical (first-seen) casing, in the order the composite
	// would be created: the folded names sorted. Order is part of a composite's identity
	// but not of what it answers, so the order here is the one that makes the same tuple
	// from the same demand every time rather than one read off a query.
	Attrs []string
	// Queries is the conjunctions observed pinning exactly this tuple. A conjunction
	// pinning a SUPERSET of it demands its own, wider tuple and is counted there; it is
	// also demand this composite could serve, which is what keeps it from being dropped
	// (compositeDemand), but it is not a reason to create this one.
	Queries int64
}

// SuggestComposites recommends composite indexes from observed tuple demand. It is
// advisory: apply via Options.CompositeAttrs at creation, or let AutoTune apply it.
//
// It takes no sample, unlike SuggestIndexes. A single-attribute suggestion has to profile
// the data to choose a KIND, and to establish that one literal kind dominates enough for an
// index to be worth building. A composite has neither question to answer: its components are
// already indexed -- that is a precondition, below -- so each one's kind is settled, and
// whether the tuple is selective enough to be worth probing is decided per query, at query
// time, by compositeWins. A composite the data does not suit is declined rather than used.
//
// COMPONENTS FIRST. A tuple is suggested only once EVERY component is already indexed in its
// own right. The alternative -- suggesting the components alongside the tuple, so one pass
// creates all of it -- was not taken, for three reasons. newCompositeSpecs refuses a tuple
// whose component is unindexed, and it refuses it for a reason: everything the composite
// does not answer (one attribute alone, a range, a segment indexed before the composite
// existed) falls back to the components' own postings, and there has to be something to fall
// back to. A composite must therefore never be the reason an attribute is indexed, which is
// exactly what creating both at once would make it. And creating a composite bumps the index
// generation, so every sealed sidecar is rebuilt; doing that on the strength of demand that
// has been observed for one maintenance interval, in the same breath as committing to the
// component indexes, is a large bill for a young signal. Waiting costs one AutoTune pass,
// during which the demand either survives its decay or turns out not to have been there.
func (c *Collection) SuggestComposites() []CompositeSuggestion {
	spec := c.spec.Load()
	if spec == nil {
		return nil
	}
	var out []CompositeSuggestion
	c.demand.tuples.Range(func(k, v any) bool {
		eq := v.(*tupleCounts).eq.Load() //nolint:errcheck // the tuple map only ever holds *tupleCounts
		if eq <= 0 {
			return true
		}
		folds := strings.Split(k.(string), tupleKeySep) //nolint:errcheck // tuple keys are strings
		if len(folds) < 2 || len(folds) > maxCompositeAttrs {
			return true
		}
		ids := make([]uint32, 0, len(folds))
		names := make([]string, 0, len(folds))
		for _, fold := range folds {
			id, ok := spec.resolveIndexed(c.intern, fold)
			if !ok {
				return true // a component not indexed in its own right: not yet
			}
			ids = append(ids, id)
			names = append(names, c.canonicalAttrName(spec, id, fold))
		}
		if spec.coversTupleIDs(ids) {
			return true // already configured, in some order
		}
		out = append(out, CompositeSuggestion{Attrs: names, Queries: eq})
		return true
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Queries != out[j].Queries {
			return out[i].Queries > out[j].Queries
		}
		return strings.Join(out[i].Attrs, "+") < strings.Join(out[j].Attrs, "+")
	})
	return out
}

// CompositeDropSuggestion recommends removing one configured composite, with the rationale.
type CompositeDropSuggestion struct {
	Attrs []string // the components in canonical casing, in the composite's own order
	Label string   // the tuple as IndexSizes names it: "ClusterId+ProcId"
	Auto  bool     // created by the auto-tuner; only these are ever auto-dropped
	// Reason is "unused" -- the only reason there is. A composite is reported here when NO
	// observed conjunction pins its components together, counting the ones that pin a
	// superset of them (compositeDemand), so there is no count worth carrying: it is zero.
	Reason string
}

// SuggestCompositeDrops recommends composite indexes to remove: the ones no observed
// conjunction pins together, so the tuple they are keyed on is one nothing asks for. It is
// advisory, and it reports human-configured composites as well as auto-created ones -- only
// the caller knows whether a configured tuple is a policy to respect. AutoTune drops the
// auto ones and leaves the rest.
//
// There is no low-cardinality analogue of the single-attribute rule. A composite is keyed on
// the tuple, so its cardinality is the tuple's, and a tuple of low-cardinality components can
// be the most selective key in the collection; the question of whether it prunes enough to be
// worth probing is already asked per query by compositeWins.
func (c *Collection) SuggestCompositeDrops() []CompositeDropSuggestion {
	spec := c.spec.Load()
	if spec == nil || len(spec.comps) == 0 {
		return nil
	}
	var out []CompositeDropSuggestion
	for _, cs := range spec.comps {
		if c.demand.compositeDemand(foldedSorted(cs.names)) > 0 {
			continue
		}
		out = append(out, CompositeDropSuggestion{
			Attrs: append([]string(nil), cs.names...), Label: cs.label(),
			Auto: cs.auto, Reason: "unused",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// compositeDemandByLabel is the demand the composite with this reporting label can serve,
// for ranking it against single-attribute indexes when a memory budget trims. A label that
// names no current composite has no demand, which sorts it first to be trimmed -- harmless,
// since dropping it is then a no-op.
func (c *Collection) compositeDemandByLabel(label string) int64 {
	spec := c.spec.Load()
	if spec == nil {
		return 0
	}
	for _, cs := range spec.comps {
		if cs.label() == label {
			return c.demand.compositeDemand(foldedSorted(cs.names))
		}
	}
	return 0
}

// foldedSorted folds attribute names and sorts them, producing the canonical tuple the
// demand counters are keyed on.
func foldedSorted(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = strings.ToLower(n)
	}
	sort.Strings(out)
	return out
}

// canonicalAttrName renders an indexed id for a human, in either index mode, falling back to
// the folded name when the id resolves to nothing.
func (c *Collection) canonicalAttrName(spec *indexSpec, id uint32, fold string) string {
	if spec.inline {
		if n, ok := spec.names[id]; ok {
			return n
		}
		return fold
	}
	if n, ok := c.intern.Name(id); ok {
		return n
	}
	return fold
}

// coversTupleIDs reports whether some configured composite is built over exactly this SET of
// component ids. Order is part of a composite's IDENTITY but not of what it answers --
// (ClusterId, ProcId) and (ProcId, ClusterId) serve the same conjunctions -- so a tuple a
// human configured in one order must not be created a second time in the sorted order the
// demand counters canonicalize to.
func (s *indexSpec) coversTupleIDs(ids []uint32) bool {
	want := append([]uint32(nil), ids...)
	slices.Sort(want)
	for _, cs := range s.comps {
		if len(cs.ids) != len(want) {
			continue
		}
		have := append([]uint32(nil), cs.ids...)
		slices.Sort(have)
		if slices.Equal(have, want) {
			return true
		}
	}
	return false
}

// --- applying ------------------------------------------------------------------------

// addCompositesAuto installs auto-created composites over the given attribute tuples and
// returns the tuples it actually installed, which may be fewer: newCompositeSpecs refuses a
// tuple whose component is not indexed (or is duplicated, or is the wrong width), and a
// tuple some composite already covers in any order is skipped. Marked auto, so the memory
// budget may later trim them and an unused-drop may remove them -- neither of which may
// touch a human-configured composite.
//
// Like AddIndex, this only changes the configuration: the composite's postings appear when
// Reindex next rebuilds a segment, and until then the conjunction is satisfied by
// intersecting the components' postings, exactly as before. Unlike AddIndex, every sealed
// segment is stale afterwards -- a sidecar carries its composites, so the signature moves --
// which is the cost SuggestComposites' components-first rule is pacing.
func (c *Collection) addCompositesAuto(tuples [][]string) [][]string {
	if len(tuples) == 0 {
		return nil
	}
	for {
		cur := c.spec.Load()
		next := cur.clone()
		var added [][]string
		for _, t := range tuples {
			// One tuple at a time: newCompositeSpecs resolves against next, so a tuple
			// added in this pass is visible to coversTupleIDs for the next one, and a
			// refused tuple does not take an accepted one down with it.
			specs := next.newCompositeSpecs(c.intern, [][]string{t}, true)
			if len(specs) != 1 || next.coversTupleIDs(specs[0].ids) {
				continue
			}
			next.comps = append(next.comps, specs[0])
			added = append(added, t)
		}
		if len(added) == 0 {
			return nil
		}
		// Tuple-identity order, which is the order newCompositeSpecs leaves a configured
		// set in and the order equalIDs compares two specs in.
		sort.Slice(next.comps, func(i, j int) bool { return next.comps[i].key < next.comps[j].key })
		next.gen = next.signature()
		if c.spec.CompareAndSwap(cur, next) {
			return added
		}
	}
}

// dropComposites removes the composites with these reporting labels and returns whether the
// configuration changed. A label naming no current composite is ignored, which is what the
// budget trim hands back when dropping a component index has already taken the composite
// with it (pruneComposites).
//
// The components keep their own indexes: a composite is an addition to them, never a
// replacement, so dropping one costs selectivity on the tuple and nothing else.
func (c *Collection) dropComposites(labels ...string) bool {
	if len(labels) == 0 {
		return false
	}
	want := make(map[string]struct{}, len(labels))
	for _, l := range labels {
		want[l] = struct{}{}
	}
	for {
		cur := c.spec.Load()
		next := cur.clone()
		kept := next.comps[:0:0]
		for _, cs := range next.comps {
			if _, drop := want[cs.label()]; drop {
				continue
			}
			kept = append(kept, cs)
		}
		if len(kept) == len(next.comps) {
			return false
		}
		next.comps = kept
		next.gen = next.signature()
		if c.spec.CompareAndSwap(cur, next) {
			return true
		}
	}
}
