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
// the PRICE: one lookup per disjunct instead of one per component, which is what the cost
// model below multiplies by.
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
func (cat *Catalog) planTupleProbes(cands []*probeCandidate, sides []JoinSide,
	steps []*joinStepPlan) map[int]*tupleProbe {
	out := map[int]*tupleProbe{}
	if !tupleProbes {
		return out
	}
	for _, g := range groupByStep(cands) {
		if len(g) < 2 {
			continue // one attribute is not a tuple
		}
		attrs := make([]string, len(g))
		equis := make([]int, len(g))
		for i, c := range g {
			attrs[i], equis[i] = c.attr, c.equi
		}
		table := sides[steps[g[0].step].side].Table
		tp := &tupleProbe{
			step: g[0].step, attrs: attrs, equis: equis, perTuple: len(g),
			rows: g[0].rows, segments: g[0].segments, seen: map[string]bool{},
		}
		if comp, ok := cat.joinSideComposite(table, attrs); ok {
			// The composite's order, not the step's: a step pinning the same attributes
			// in either order matches the same index, and the tuple is the composite's
			// to name once one answers it.
			reordered := make([]int, len(comp))
			for i, name := range comp {
				for _, c := range g {
					if strings.EqualFold(c.attr, name) {
						reordered[i] = c.equi
						break
					}
				}
			}
			tp.attrs, tp.equis, tp.composite, tp.perTuple = comp, reordered, comp, 1
		}
		out[g[0].step] = tp
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
// Capped rather than allowed to overflow. The ceiling is only ever compared against a term count
// bounded by maxProbeKeys, so any value past it is "larger than the tuple form" whatever its
// true size, and saturating says that without a wider integer.
func crossProduct(g []*probeCandidate) int {
	const ceiling = 1 << 30
	n := 1
	for _, c := range g {
		if len(c.lits) == 0 {
			return 0 // no constraint on this component: the product is not meaningful
		}
		if n > ceiling/len(c.lits) {
			return ceiling
		}
		n *= len(c.lits)
	}
	return n
}
