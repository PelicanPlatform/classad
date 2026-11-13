package db

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
)

// A BAND JOIN IS A WINDOW OVER A SORTED SIDE.
//
// An equality join narrows by hashing: the key picks one bucket and the rows outside it are
// never looked at. An inequality has no such key, so until this file a step without an
// equality held its side whole in joinStage.all and paired EVERY row of it against every
// accumulated row. That is the one shape in the join with no algorithm behind it -- the pairs
// are the product of the sides, and MaxPairs stops the query rather than the plan avoiding
// the work. At the default 20,000,000 the largest square band join that finishes is about
// 4,472 rows a side.
//
// A band is the case that does have an algorithm. When the condition bounds ONE attribute of
// the folded side between two expressions over the sides already in hand -- `h.Ts BETWEEN
// j.Start AND j.End` -- the rows that can match are contiguous once the side is sorted on
// that attribute. So the side is sorted once, when the stage is built, and each accumulated
// row binary-searches its own window and pairs against that alone. The pairs become the
// matches rather than the product, and the sort is paid once for the whole join rather than
// once per row.
//
// The window is DECLARED, not inferred. A caller writes JoinBand the way it writes JoinEqui,
// naming the attribute and the two bounds, because the alternative is recognising a band in
// the ON text and that makes the plan depend on how the predicate was spelled. It also makes
// the semantics the caller's to read rather than ours to guess: see JoinBand.

// JoinBand is an ordered window over one attribute of a step's table.
//
// It is a conjunct of the step's match, like JoinEqui: a row of the step's table joins only
// where its Attr falls inside the window, and the step's Cond still has to hold on top.
//
// It is defined over NUMBERS. Attr joins when it evaluates to an integer, a real or a boolean
// -- the three the rest of the join already orders, with false below true -- and does not
// join when it is undefined, a string, a list, an error or NaN. A bound that does not
// evaluate to one of those three admits nothing, which is the rule an undefined hash key
// already follows: a key that cannot be compared matches nothing rather than everything.
//
// That makes a band narrower than the ClassAd comparison it reads like, and deliberately so.
// ClassAd orders strings against strings -- `"abc" >= "abb"` is true -- and errors on a
// string against a number. A band answers neither: the first it declines, and the second it
// declines rather than erroring. A caller wanting ClassAd's own answer writes the comparison
// in Cond, where it is evaluated with ClassAd's semantics against every candidate; what is
// lost is the narrowing, not the question.
type JoinBand struct {
	// Attr names the attribute of THIS step's table the window is over.
	Attr string `json:"attr"`
	// Lo and Hi bound the window, as expressions over the sides folded in BEFORE this
	// step. An empty string leaves that end unbounded, which is how a one-sided
	// inequality is written: `h.Ts >= j.Start` is Lo alone.
	Lo string `json:"lo,omitempty"`
	Hi string `json:"hi,omitempty"`
	// LoOpen and HiOpen make that end exclusive -- `>` rather than `>=`.
	LoOpen bool `json:"loOpen,omitempty"`
	HiOpen bool `json:"hiOpen,omitempty"`
}

// bandPlan is one compiled JoinBand.
type bandPlan struct {
	attr   string
	lo, hi *classad.Expr
	// loAlias and hiAlias are the bounds compiled to read the sides directly, or nil when
	// one declined and lo/hi must be evaluated against a composite of the row so far. The
	// same two-form arrangement an equality's row half uses, for the same reason.
	loAlias, hiAlias *aliasedValue
	loOpen, hiOpen   bool
}

// compileBands compiles a step's bands and refuses the ones that cannot mean anything.
//
// The refusals are all cases where the band would otherwise read as a window that admits
// everything or nothing without saying so. A bound naming the step's own side, or a later
// one, is refused by refuseUnscopedSides for the reason it refuses an equality's row half:
// it would read undefined, and a window that silently admits nothing is a wrong answer with
// no symptom.
func compileBands(bands []JoinBand, aliases []string, side int, kind JoinKind) ([]*bandPlan, error) {
	if len(bands) == 0 {
		return nil, nil
	}
	if kind == JoinCross {
		return nil, fmt.Errorf("a cross join pairs every row with every row, so it has no "+
			"band to window on; drop the band or give %s a condition", aliases[side])
	}
	out := make([]*bandPlan, 0, len(bands))
	for i := range bands {
		b := bands[i]
		attr := strings.TrimSpace(b.Attr)
		if attr == "" {
			return nil, fmt.Errorf("band %d of %s names no attribute to window on", i, aliases[side])
		}
		lo, hi := strings.TrimSpace(b.Lo), strings.TrimSpace(b.Hi)
		if lo == "" && hi == "" {
			return nil, fmt.Errorf("band %d of %s bounds %s at neither end, so it admits "+
				"every row; drop it", i, aliases[side], attr)
		}
		p := &bandPlan{attr: attr, loOpen: b.LoOpen, hiOpen: b.HiOpen}
		for _, half := range []struct {
			src   string
			which string
			ex    **classad.Expr
			al    **aliasedValue
		}{{lo, "lower", &p.lo, &p.loAlias}, {hi, "upper", &p.hi, &p.hiAlias}} {
			if half.src == "" {
				continue
			}
			ex, err := classad.ParseExpr(half.src)
			if err != nil {
				return nil, fmt.Errorf("band %d of %s, %s bound %q: %w",
					i, aliases[side], half.which, half.src, err)
			}
			// The bound reads the row as it stands BEFORE this step, so the sides in
			// scope stop one short of the step's own.
			if err := refuseUnscopedSides("band bound", []string{half.src}, aliases, side-1); err != nil {
				return nil, err
			}
			*half.ex = ex
			*half.al = compileAliasedValue(half.src, aliases)
		}
		out = append(out, p)
	}
	return out, nil
}

// bound evaluates one end of the window against the row so far.
//
// ok is false when the end is absent -- unbounded -- or does not evaluate to a number, and
// the two are not distinguished here because the caller treats them differently by which end
// it asked for: an absent bound is an open side of the window, a non-numeric one admits
// nothing. See bandWindow, which holds that distinction.
//
// NaN is refused HERE rather than left to the comparisons, because the comparisons get it
// wrong in one direction. Every comparison against NaN is false, so a NaN UPPER bound makes
// `vals[i] > hi` false for every row, and sort.Search answers "no such index" with the
// slice's length -- which is not an empty window, it is the whole side. The lower bound
// happens to come out empty by the same arithmetic, so the two ends would disagree about
// what an unreadable bound means.
func (b *bandPlan) bound(ex *classad.Expr, al *aliasedValue, p *joinPlan, step *joinStepPlan,
	rows []*joinRow, resolve sideLookup) (float64, bool) {
	if ex == nil {
		return 0, false
	}
	var v classad.Value
	if al != nil {
		got, taken := al.eval(resolve)
		if taken {
			v = got
		} else {
			v = ex.Eval(p.rowHalfScope(step, rows))
		}
	} else {
		v = ex.Eval(p.rowHalfScope(step, rows))
	}
	f, num := numericKey(v)
	if !num || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

// bandWindow turns the row's bounds into the half-open range of a sorted side the first band
// admits: vals[lo:hi].
//
// ok is false when the window admits nothing at all, which is a bound that is present and not
// a number. The caller then pairs against no candidate, exactly as an undefined hash key
// selects no bucket.
func (st *joinStage) bandWindow(p *joinPlan, rows []*joinRow, resolve sideLookup) (int, int, bool) {
	b := st.step.band[0]
	vals := st.bandVals
	lo, hi := 0, len(vals)
	if b.lo != nil {
		v, num := b.bound(b.lo, b.loAlias, p, st.step, rows, resolve)
		if !num {
			return 0, 0, false
		}
		// Closed takes the first value at or above the bound, open the first strictly
		// above it. Both are the same search with the comparison moved, which is the
		// whole of what LoOpen means.
		if b.loOpen {
			lo = sort.Search(len(vals), func(i int) bool { return vals[i] > v })
		} else {
			lo = sort.Search(len(vals), func(i int) bool { return vals[i] >= v })
		}
	}
	if b.hi != nil {
		v, num := b.bound(b.hi, b.hiAlias, p, st.step, rows, resolve)
		if !num {
			return 0, 0, false
		}
		// The end is EXCLUSIVE, so a closed upper bound stops at the first value above
		// it and an open one at the first value at or above it.
		if b.hiOpen {
			hi = sort.Search(len(vals), func(i int) bool { return vals[i] >= v })
		} else {
			hi = sort.Search(len(vals), func(i int) bool { return vals[i] > v })
		}
	}
	if lo >= hi {
		return 0, 0, false // the bounds crossed, or nothing lies between them
	}
	return lo, hi, true
}

// bandsHold evaluates this step's bands from index `from` as plain predicates.
//
// `from` is 1 where the stage is sorted, because the window has already decided band 0 for
// every candidate it yielded, and 0 where it is not -- a step that also hashes keeps its
// bands but has no sorted side to search, so each one is checked here instead.
func (p *joinPlan) bandsHold(step *joinStepPlan, rows []*joinRow, resolve sideLookup, from int) bool {
	for i := from; i < len(step.band); i++ {
		b := step.band[i]
		v, num := numericKey(rows[step.side].eval(b.attr))
		if !num || math.IsNaN(v) {
			return false
		}
		if b.lo != nil {
			lo, ok := b.bound(b.lo, b.loAlias, p, step, rows, resolve)
			if !ok || v < lo || (b.loOpen && v == lo) {
				return false
			}
		}
		if b.hi != nil {
			hi, ok := b.bound(b.hi, b.hiAlias, p, step, rows, resolve)
			if !ok || v > hi || (b.hiOpen && v == hi) {
				return false
			}
		}
	}
	return true
}

// sortBand orders a stage's held rows by the first band's attribute and splits off the ones
// that can never match.
//
// A row whose band attribute is not a number fails the band whatever the bounds are, so it is
// moved out of the searchable side entirely rather than left to be skipped. unmatchable
// carries those rows back to the caller, which is the only thing that still wants them: a
// right-outer step has to emit the rows of its held side that joined to nothing, and these
// are exactly the ones that joined to nothing most certainly. They are the band's analogue of
// the rows an undefined key leaves out of every bucket, and they travel the same route.
//
// NaN is counted as not a number here although Go's float64 holds it. It compares false
// against every bound, including itself, so it can match nothing -- and leaving it in the
// slice would put a value in the sort that breaks the ordering the binary search assumes.
func (st *joinStage) sortBand() (unmatchable []*joinRow) {
	attr := st.step.band[0].attr
	keep := st.all[:0]
	vals := make([]float64, 0, len(st.all))
	for _, r := range st.all {
		v, num := numericKey(r.eval(attr))
		if !num || math.IsNaN(v) {
			unmatchable = append(unmatchable, r)
			continue
		}
		keep = append(keep, r)
		vals = append(vals, v)
	}
	st.all = keep
	st.bandVals = vals
	sort.Sort(&bandSort{rows: st.all, vals: st.bandVals})
	st.banded = true
	return unmatchable
}

// bandSort orders the rows and their values together. A sort.Interface rather than
// slices.SortFunc because the two slices move as one and the values are what is compared:
// re-reading the attribute inside the comparison would decode a row per comparison.
type bandSort struct {
	rows []*joinRow
	vals []float64
}

func (b *bandSort) Len() int           { return len(b.vals) }
func (b *bandSort) Less(i, j int) bool { return b.vals[i] < b.vals[j] }
func (b *bandSort) Swap(i, j int) {
	b.vals[i], b.vals[j] = b.vals[j], b.vals[i]
	b.rows[i], b.rows[j] = b.rows[j], b.rows[i]
}
