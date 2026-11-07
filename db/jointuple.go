package db

// Narrowing a join on the WHOLE key at once.
//
// A step joined on (ClusterId, ProcId) collects key values for each attribute separately and
// conjoins them: `ClusterId in {7,9,...} && ProcId in {0,1,2,3}`. That is sound and it is
// what the planner can answer without a composite, but it is the CROSS PRODUCT of the two
// key sets. Fifty clusters and four procs narrow the table to two hundred (cluster, proc)
// pairs when the FROM side carries at most fifty of them, and the scan re-verifies the rest.
// The wider the fan-out of either attribute, the more of the narrowing is wasted.
//
// The constraint can instead name the pairs the FROM side actually holds: `(ClusterId == 7
// && ProcId == 0) || (ClusterId == 9 && ProcId == 2) || ...`. Each disjunct pins every
// component to one value, and the planner resolves each one and unions the results.
//
// WHAT ACTUALLY WINS HERE IS THE SHAPE, NOT THE INDEX. A disjunct pinning both components
// to one value is answered, with no composite at all, by intersecting the two components'
// own postings -- one cluster's and one proc's -- which yields exactly that pair. Measured
// on a 32-pair fixture the tuple spine hashes 32 rows against the in-chains' 132 whether or
// not a composite exists, and the composite's own contribution to the clock was inside the
// noise. So the tuple form is not gated on having a composite. What the composite changes is
// the PRICE the cost model below charges: one lookup per disjunct instead of one per
// component.
//
// It changes the CLOCK by far less than that, and the noise above was not the fixture being
// small. Swept at 8192 records over 196 segments, the same spine with and without a
// composite runs within 4-16% of itself at every tuple count measured -- 3.74 ms against
// 4.02 ms at 64 tuples, 6.09 against 7.24 at 96 -- while the model charges the second twice
// as much (joinprobecost_bench_test.go). Intersecting two single-value postings is nearly as
// cheap as reading one composite posting, so perTuple is a factor of two the clock cannot
// find.
//
// It is kept anyway, and this is the awkward part. probeLookupCost's own sweep shows the
// segment factor over-weighting segments by about 6x across a 15x range, which leaves the
// gate too permissive on an archive of few, large segments -- and on exactly those shapes
// perTuple=2 is what pulls a composite-less spine back towards its measured crossover.
// Dropping it on its own would be right about the composite and more wrong about the join.
// Neither moves until the term is priced on segments the way the clock says, which is more
// than a constant's worth of change.
//
// THREE THINGS MAKE THIS NARROWER THAN IT LOOKS.
//
// The constraint must be a PURE disjunction. The planner reaches a disjunct's probes by
// distributing the predicate into disjunctive normal form, which is bounded (a predicate
// whose full DNF is a cross-product cannot be allowed to explode); past the bound it falls
// back to splitting the top-level `||` spine only. A spine of per-tuple conjunctions
// survives that fallback with one group per tuple. The same spine conjoined with anything
// else does not: the top level is then an `&&`, the spine stays folded inside one group as a
// single expression no probe can be read from, and the step narrows on the other conjunct
// alone. So a tuple constraint is built only when the tuple is EXACTLY the set of attributes
// the step would otherwise have narrowed on -- never alongside a leftover in-chain, which
// would be slower than not narrowing on the tuple at all.
//
// And it costs a lookup per TUPLE rather than per key value. Fifty clusters and four procs
// is fifty-four lookups per segment the per-attribute way and up to two hundred this way, or
// four hundred without a composite to answer each pair in one. The tuple form reads fewer
// records and asks the index for more, so it is priced under the same model and declines
// when the asking costs more than the reading saves.
//
// It is also declined when it would buy nothing: if the FROM side's pairs are the whole
// cross product of its key values -- which is what a key half taking one value looks like --
// the in-chains already admit exactly those pairs, more cheaply.

import (
	"fmt"
	"sort"
	"strings"
)

// tupleProbes turns the whole-key form off.
//
// A var for the same reason the probe tuning in joinprobe.go is, and with the same meaning:
// every decline in this file falls back to the per-attribute form, so turning the form off
// costs selectivity and never correctness. That makes it both the knob a benchmark needs in
// order to compare the two forms, and a retreat if the form turns out to be wrong about a
// shape nothing here measured.
var tupleProbes = true

// tupleProbe is a step's candidate for narrowing on a whole composite key: which equalities
// the components come from, and the distinct tuples the FROM side holds for them.
type tupleProbe struct {
	step int
	// attrs and equis are parallel: attrs[i] is the composite's i'th component and equis[i]
	// the step equality that names it. The order is the composite's, not the step's.
	attrs    []string
	equis    []int
	rows     int
	segments int
	// perTuple is how many index lookups one disjunct costs: one when a composite answers
	// the whole tuple, otherwise one per component, whose postings are then intersected.
	perTuple int
	// composite is the composite index answering the tuple, empty when none does.
	composite []string
	// cross is the size of the cross product the per-attribute form would admit -- the
	// product of each component's distinct key count. The tuple form is worth building only
	// if it admits fewer candidates than that.
	cross int
	// terms is one rendered conjunction per distinct tuple, e.g. `(ClusterId == 7 && ProcId == 0)`.
	terms []string
	seen  map[string]bool
}

// label renders the composite for a human: "ClusterId+ProcId".
func (tp *tupleProbe) label() string { return strings.Join(tp.attrs, "+") }

// add records one row's tuple, if every component of it is a usable key value.
//
// A row missing any component is dropped WHOLE rather than contributing its other
// components. That is sound for the same reason the per-attribute form drops a row's term:
// a row whose ProcId is undefined matches no table row through `l.ProcId == r.ProcId`, so it
// is entitled to no part of the narrowing. Keeping its ClusterId would widen the constraint
// on behalf of a row that matches nothing.
func (tp *tupleProbe) add(lits []string) {
	key := strings.Join(lits, "\x00")
	if tp.seen[key] {
		return
	}
	tp.seen[key] = true
	parts := make([]string, len(lits))
	for i, l := range lits {
		parts[i] = tp.attrs[i] + " == " + l
	}
	tp.terms = append(tp.terms, "("+strings.Join(parts, " && ")+")")
}

// constraint renders the tuple membership constraint, or says why narrowing this way was not
// worth it. A decline is not fatal: the caller falls back to the per-attribute form, which is
// priced separately and may still pay.
func (tp *tupleProbe) constraint() (cons, why string) {
	switch {
	case len(tp.terms) == 0:
		return "", fmt.Sprintf("no row of the FROM side has a usable value for every "+
			"component of %s", tp.label())
	case len(tp.terms) > maxProbeKeys:
		return "", fmt.Sprintf("the FROM side has more than %d distinct %s tuples",
			maxProbeKeys, tp.label())
	}
	if tp.cross > 0 && len(tp.terms) >= tp.cross {
		return "", fmt.Sprintf("the FROM side's %d %s tuples are the whole cross product of "+
			"its key values, which the per-attribute constraint already admits for fewer "+
			"lookups", len(tp.terms), tp.label())
	}
	if lookups := tp.segments * len(tp.terms) * tp.perTuple; lookups*probeLookupCost > tp.rows {
		return "", fmt.Sprintf("%d %s tuples across %d segments is %d index lookups against "+
			"a read of %d records, which costs more than it saves",
			len(tp.terms), tp.label(), tp.segments, lookups, tp.rows)
	}
	sort.Strings(tp.terms) // so the same join builds the same constraint
	return strings.Join(tp.terms, " || "), ""
}

// planTupleProbes gives every step that would narrow on several attributes a tuple
// candidate, and notes which of them a composite index can answer in one lookup.
//
// Every such step, not only the ones with a composite: the spine's selectivity comes from
// its shape and a step without a composite gets the same candidates for more lookups, which
// is a matter for the cost model rather than a reason to refuse. Where a composite does
// cover the tuple it must cover it EXACTLY, as a set -- a composite answers the full tuple
// only, and one covering a strict subset would leave an in-chain the constraint cannot carry
// alongside the spine.
func (cat *Catalog) planTupleProbes(stepKeys map[int]*stepKey, cands []*probeCandidate,
	sides []JoinSide, steps []*joinStepPlan) map[int]*tupleProbe {
	out := map[int]*tupleProbe{}
	if !tupleProbes {
		return out
	}
	// Two sets per step, tried in this order: the step's WHOLE probeable key, and the
	// subset of it that carries single-attribute indexes.
	//
	// The whole key first, because that is the set a composite is most likely to cover and
	// the only one that can include a component-only attribute -- an attribute with no
	// index of its own, answerable only as part of a tuple. The indexed subset second,
	// because a step can hold an equality on an attribute that is neither indexed nor a
	// component: the per-attribute form already ignores it, so a spine over the rest is
	// still exactly the set the step would otherwise have narrowed on.
	indexed := map[int][]probeAttr{}
	for _, g := range groupByStep(cands) {
		for _, c := range g {
			indexed[c.step] = append(indexed[c.step], probeAttr{equi: c.equi, attr: c.attr})
		}
	}
	for i := range steps {
		sk := stepKeys[i]
		if sk == nil {
			continue
		}
		set, ok := cat.tupleSetFor(sides[steps[i].side].Table, sk.attrs, indexed[i])
		if !ok {
			continue
		}
		out[i] = &tupleProbe{
			step: i, attrs: set.attrs, equis: set.equis, composite: set.composite,
			perTuple: set.perTuple, rows: sk.rows, segments: sk.segments,
			seen: map[string]bool{},
		}
	}
	return out
}

// tupleSet is the attribute tuple a step would narrow on as a whole, resolved against the
// table's composites: the attributes in the order they will be rendered, the step equality
// each one comes from, the composite answering them if there is one, and what one disjunct
// costs in lookups.
type tupleSet struct {
	attrs     []string
	equis     []int
	composite []string
	perTuple  int
}

// tupleSetFor picks the tuple a step narrows on, preferring the widest set a composite
// answers.
//
// all is every probeable equality of the step; indexed is the subset whose attributes carry
// an index of their own. Three outcomes, in order:
//
//   - a composite over the WHOLE key. The only case that can include a component-only
//     attribute, and the one this function exists for: one lookup per disjunct, and an
//     attribute that no single-attribute constraint could have narrowed on at all.
//   - a composite over the indexed subset. A step can hold an equality on an attribute that
//     is neither indexed nor a component; the per-attribute form already ignores it, so a
//     spine over the rest is still exactly the set the step would otherwise narrow on.
//   - no composite, but two or more indexed attributes: the spine still admits only the
//     tuples held where the in-chains admit their cross product, at one lookup per
//     component per disjunct.
//
// A spine over a component-only attribute WITHOUT a composite is never built. That attribute
// has no postings of its own, so the disjunct would be answered by its siblings alone --
// sound, but wider than the per-attribute form it replaced and paying more lookups for the
// privilege.
func (cat *Catalog) tupleSetFor(table string, all, indexed []probeAttr) (tupleSet, bool) {
	for _, cand := range [][]probeAttr{all, indexed} {
		if len(cand) < 2 {
			continue // one attribute is not a tuple
		}
		comp, ok := cat.joinSideComposite(table, attrNames(cand))
		if !ok {
			continue
		}
		// The composite's order, not the step's: a step pinning the same attributes in
		// either order matches the same index, and the tuple is the composite's to name
		// once one answers it.
		equis := make([]int, len(comp))
		for i, name := range comp {
			for _, pa := range cand {
				if strings.EqualFold(pa.attr, name) {
					equis[i] = pa.equi
					break
				}
			}
		}
		return tupleSet{attrs: comp, equis: equis, composite: comp, perTuple: 1}, true
	}
	if len(indexed) < 2 {
		return tupleSet{}, false
	}
	attrs := make([]string, len(indexed))
	equis := make([]int, len(indexed))
	for i, pa := range indexed {
		attrs[i], equis[i] = pa.attr, pa.equi
	}
	return tupleSet{attrs: attrs, equis: equis, perTuple: len(indexed)}, true
}

// attrNames is the attribute names of a probeable set, in order.
func attrNames(pas []probeAttr) []string {
	out := make([]string, len(pas))
	for i, pa := range pas {
		out[i] = pa.attr
	}
	return out
}

// joinSideComposite reports the composite index of the side's table whose components are
// exactly attrs, as a set.
//
// Configuration is read as availability here, unlike the single-attribute gate, which
// insists the index be BUILT. Two things make that safe. Reaching this function means that
// gate already passed for every component, which for an archive means a sidecar is sealed.
// And a composite the sidecars do not all carry yet -- one configured after some segments
// sealed -- degrades rather than fails: a segment without it answers each disjunct by
// intersecting its components' own postings, which is two lookups instead of one against a
// disjunct that pins both to a single value. Slower than the composite, still an index, and
// still the same rows.
func (cat *Catalog) joinSideComposite(table string, attrs []string) ([]string, bool) {
	var tuples [][]string
	switch d, found := cat.Table(table); {
	case found:
		tuples = d.CompositeAttrs()
	default:
		a, isArchive := cat.ArchiveTable(table)
		if !isArchive {
			return nil, false
		}
		tuples = a.CompositeAttrs()
	}
	for _, t := range tuples {
		if sameAttrSet(t, attrs) {
			return t, true
		}
	}
	return nil, false
}

// sameAttrSet reports whether two attribute lists name the same set, case-insensitively.
// ClassAd attribute names are case-insensitive, and neither list is sorted: a composite is
// held in its configured order and a step's candidates in the order its equalities were
// written.
func sameAttrSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	used := make([]bool, len(b))
	for _, x := range a {
		found := false
		for i, y := range b {
			if !used[i] && strings.EqualFold(x, y) {
				used[i], found = true, true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// crossProduct is how many tuples the per-attribute constraint admits: the product of each
// component's distinct key count. It is what the tuple form has to beat to be worth its
// extra lookups.
//
// Zero means there is no finite product to compare against and the caller skips the test;
// see the component-only case below.
//
// Capped rather than allowed to overflow. The ceiling is only ever compared against a term count
// bounded by maxProbeKeys, so any value past it is "larger than the tuple form" whatever its
// true size, and saturating says that without a wider integer.
func crossProduct(g []*probeCandidate, attrs []string) int {
	const ceiling = 1 << 30
	n := 1
	for _, a := range attrs {
		var lits int
		for _, c := range g {
			if strings.EqualFold(c.attr, a) {
				lits = len(c.lits)
				break
			}
		}
		if lits == 0 {
			// Unconstrained by the per-attribute form -- a component-only attribute has
			// no index and so no in-chain, and the conjunction of in-chains therefore
			// admits every value of it. There is no finite product to compare against,
			// and one computed over the siblings alone would understate the comparison
			// badly enough to decline the spine outright.
			return 0
		}
		if n > ceiling/lits {
			return ceiling
		}
		n *= lits
	}
	return n
}
