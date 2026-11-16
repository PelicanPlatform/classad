package db

import (
	"math"

	"github.com/PelicanPlatform/classad/classad"
)

// TOP-K PER GROUP, as a cutoff.
//
// "The five most recent jobs for every owner" is a ranking window
// (ROW_NUMBER() OVER (PARTITION BY Owner ORDER BY QDate DESC), QUALIFY n <= 5), and the only way
// to answer it so far has been to fetch every matching row and rank them on the client. A million
// rows cross the wire to keep five per owner.
//
// This is the per-group form of the cutoff the ungrouped top-K already uses
// (collections.TopKOrderThreshold): instead of the k-th best value over the whole table, the k-th
// best value PER GROUP. The caller then re-asks for the rows with a constraint built from the
// cutoffs -- `(Owner == "alice" && QDate >= t_alice) || (Owner == "bob" && QDate >= t_bob) || ...`
// -- which the columns and zone maps narrow to about k rows per group, and ranks those. The
// million-row pass never happens.
//
// WHY A CUTOFF AND NOT THE ROWS. A sink is fed column VALUES with no record identity attached
// (see ColumnAccumulator), so an aggregate here cannot name the rows it kept -- only the value
// that separates them from the rest. That is enough: the value is a constraint, and a constraint
// is what the storage layer is good at. It is also why this composes with everything else --
// a WHERE, a bucketed group column, a per-aggregate FILTER -- rather than being a separate plan.
//
// WHAT THE CALLER MUST STILL DO. The cutoff admits TIES: every row whose order value equals the
// k-th gets through, so a group with ties returns more than k rows and the caller's own ranking
// cuts them. That is the same contract the ungrouped cutoff has, and it is the safe direction to
// err in -- a cutoff that admitted too few would silently drop rows that belong in the answer.

// topKThresholdAcc keeps the best k order values of one group and reports the k-th.
//
// It satisfies the two promises ColumnAccumulator demands. ORDER: the set of the k best values of
// a multiset does not depend on the order they arrive in, and neither does its worst member, so
// the column pass and the record scan agree. WHAT IS OFFERED: it folds numbers and nothing else,
// exactly as the built-in MIN and MAX do, so a row whose order attribute is absent or is a string
// contributes to the group's COUNT and not to its cutoff.
//
// vals is unordered and at most k long, so offer is O(k) and the state is bounded by the REQUEST
// rather than by the group -- which is what makes this safe to point at a history table where a
// group holds millions of rows.
type topKThresholdAcc struct {
	vals []NumValue
	k    int
	desc bool
}

// topKWorse reports whether a is the value a better one would evict: the smallest when keeping the
// largest k (desc), the largest when keeping the smallest k (asc).
//
// Two integers compare as integers. Past 2^53 a NumInt's F has lost digits, so comparing the
// floats would call two different job ids equal and keep the wrong one; I is exact and is what a
// pair of integers is ordered by. Anything else compares as floats, which is what a mixed int/real
// column means by order anyway.
func topKWorse(a, b NumValue, desc bool) bool {
	if a.Kind == NumInt && b.Kind == NumInt {
		if desc {
			return a.I < b.I
		}
		return a.I > b.I
	}
	if desc {
		return a.F < b.F
	}
	return a.F > b.F
}

// AddNum folds one numeric column value.
//
// A NaN is dropped rather than kept. It compares false against everything, so a kept NaN would
// never be evicted and would become the group's reported cutoff -- a constraint that matches no
// row, turning "the five newest" into "none". The built-in numeric path declines a NaN column for
// the same reason; dropping it here keeps the cutoff a number that means what it says.
func (a *topKThresholdAcc) AddNum(v NumValue) {
	if v.Kind == NumReal && math.IsNaN(v.F) {
		return
	}
	if len(a.vals) < a.k {
		a.vals = append(a.vals, v)
		return
	}
	wi := a.worstIdx()
	if topKWorse(a.vals[wi], v, a.desc) {
		a.vals[wi] = v
	}
}

// Add folds one row's argument value, for the record scan. Non-numbers are skipped, as AddNum
// never sees them -- which is what keeps the two paths answering alike.
func (a *topKThresholdAcc) Add(v classad.Value) {
	switch {
	case v.IsInteger():
		i, err := v.IntValue()
		if err != nil {
			return
		}
		a.AddNum(NumValue{F: float64(i), I: i, Kind: NumInt})
	case v.IsNumber():
		f, err := v.NumberValue()
		if err != nil {
			return
		}
		a.AddNum(NumValue{F: f, Kind: NumReal})
	}
}

// worstIdx is the index of the kept value a better one would replace. O(k) per offer, which is
// what bounds k to a LIMIT rather than a scan.
func (a *topKThresholdAcc) worstIdx() int {
	wi := 0
	for i := 1; i < len(a.vals); i++ {
		if topKWorse(a.vals[i], a.vals[wi], a.desc) {
			wi = i
		}
	}
	return wi
}

// Merge folds another group-partial's kept values in. Offering them one at a time is associative
// and commutative because offer keeps the best k of everything it has seen, whatever the order --
// the law an accumulator fanned out across segment windows has to obey.
func (a *topKThresholdAcc) Merge(other Accumulator) {
	o, ok := other.(*topKThresholdAcc)
	if !ok {
		return
	}
	for _, v := range o.vals {
		a.AddNum(v)
	}
}

// Result is the k-th best value, or UNDEFINED when the group holds fewer than k numeric values.
//
// Undefined is the honest answer there and not a failure: with fewer than k rows the group has no
// cutoff, every row of it belongs in the answer, and a caller that read a number would constrain
// away rows it should have kept. The caller takes such a group whole.
func (a *topKThresholdAcc) Result() classad.Value {
	if len(a.vals) < a.k {
		return classad.NewUndefinedValue()
	}
	w := a.vals[a.worstIdx()]
	if w.Kind == NumInt || w.Kind == NumBool {
		return classad.NewIntValue(w.I)
	}
	return classad.NewRealValue(w.F)
}

// topKAggregate is the factory the grouped passes make one accumulator per group from. It is an
// Aggregate like a registered one, so the columnar grouped rung reaches it through exactly the
// machinery user-defined aggregates already use -- the same sinks, the same per-worker fan-out,
// the same Merge -- rather than through a second plan that could drift from it.
type topKAggregate struct {
	k    int
	desc bool
}

func (t topKAggregate) NewAccumulator() Accumulator {
	return &topKThresholdAcc{k: t.k, desc: t.desc, vals: make([]NumValue, 0, t.k)}
}

// topKSpecValid reports whether a TOPK_THRESHOLD spec is answerable.
//
// K must be positive: the cutoff of the best zero values is not a value. The argument must name an
// attribute, since the rows of a group are not a number to order them by -- the same hole
// COUNT(DISTINCT *) and LIST(*) have.
func topKSpecValid(a AggSpec) bool {
	return a.K > 0 && a.Arg != "" && a.Arg != "*"
}
