package db

import (
	"errors"
	"fmt"

	"github.com/PelicanPlatform/classad/classad"
)

// CheckAggregate tests one Aggregate against the laws the reduction relies on, over the sample
// values given.
//
// IT SHIPS IN THE PACKAGE RATHER THAN IN A TEST because the thing it checks is not this package's
// code -- it is a caller's, written somewhere this package's tests cannot see. The alternative is a
// comment saying "merge must be associative and commutative", and a comment is not a check. An
// aggregate whose merge is order-sensitive passes every test an author would think to write,
// because an author tests the order their own code produces; it fails in production, where segment
// and zone visit order is not the author's to choose.
//
// THE SAMPLES ARE THE CALLER'S because only the caller knows what its aggregate eats. A generic
// generator here would produce integers for an aggregate over host names and prove nothing. Pass
// values of the shape the aggregate will really see, and include the awkward ones -- undefined, an
// error, a string where a number is expected, duplicates, a value appearing once -- since those are
// where merge and identity usually part company.
//
// WHAT IT CANNOT CHECK. These laws are universally quantified and this runs them over one finite
// sample, so a pass is evidence rather than proof; an aggregate that is commutative except on a
// value the caller did not pass still passes here. It is a filter for the mistakes people actually
// make, not a verification.
func CheckAggregate(agg Aggregate, samples []classad.Value) error {
	return CheckAggregateEqual(agg, samples, nil)
}

// CheckAggregateEqual is CheckAggregate with the caller's own notion of two answers being the same.
// A nil equal compares the rendered values exactly, which is what CheckAggregate does.
//
// THE EXACT COMPARISON IS WRONG FOR AN AGGREGATE THAT DOES FLOATING-POINT ARITHMETIC, and that is
// not a weakness of the implementation being checked. Floating-point addition is not associative,
// so an aggregate computing a variance -- the obvious first thing anyone writes here, since its
// state is bounded where the built-ins' is O(group size) -- gives answers that differ in the last
// bits depending on how the rows were divided into partial states. That IS the order dependence the
// law is about; it is simply not avoidable, and a checker demanding exact equality would reject
// every correct implementation of it while passing an integer aggregate that is genuinely broken.
//
// So the comparison is the caller's, and an aggregate needing a tolerance has thereby SAID that it
// needs one, in its own tests, where a reader can see the size of the slack and judge it. The
// alternative -- a tolerance built in here -- would apply silently to aggregates that should have
// been exact.
func CheckAggregateEqual(agg Aggregate, samples []classad.Value, equal func(a, b classad.Value) bool) error {
	if agg == nil {
		return fmt.Errorf("CheckAggregate: nil aggregate: %w", ErrBadAggregate)
	}
	// Four is the smallest sample that lets a three-way split put at least one value in each
	// part, which is what associativity needs to say anything. Refusing is deliberate: the
	// alternative is a check that passes having compared almost nothing, which is worse than no
	// check because it reads as one.
	if len(samples) < 4 {
		return fmt.Errorf("CheckAggregate: %d samples is too few for the merge laws to mean anything; pass at least 4: %w",
			len(samples), ErrBadAggregate)
	}

	fold := func(vals []classad.Value) (Accumulator, error) {
		acc := agg.NewAccumulator()
		if acc == nil {
			return nil, errors.New("CheckAggregate: NewAccumulator returned nil")
		}
		for _, v := range vals {
			acc.Add(v)
		}
		return acc, nil
	}
	// Answers are compared as values and REPORTED as text, so a failure message shows what the
	// engine would have put in the result column rather than a Go rendering of a value struct.
	same := func(a, b classad.Value) bool {
		if equal != nil {
			return equal(a, b)
		}
		return ValueText(a) == ValueText(b)
	}

	// The identity: a group that contributed no rows still has an answer, and asking for it
	// twice gives the same one.
	empty, err := fold(nil)
	if err != nil {
		return err
	}
	identity := empty.Result()
	if again := empty.Result(); !same(identity, again) {
		return fmt.Errorf("CheckAggregate: Result on an empty accumulator is not repeatable: %q then %q",
			ValueText(identity), ValueText(again))
	}

	// The serial answer, and the two properties Result itself has to have: repeatable, and
	// leaving the state alone. A Result that finalizes in place is the classic way for a state
	// that is read and then merged into to come out wrong.
	whole, err := fold(samples)
	if err != nil {
		return err
	}
	want := whole.Result()
	if again := whole.Result(); !same(want, again) {
		return fmt.Errorf("CheckAggregate: Result is not repeatable: %q then %q", ValueText(want), ValueText(again))
	}
	twice, err := fold(samples)
	if err != nil {
		return err
	}
	if got := twice.Result(); !same(want, got) {
		return fmt.Errorf("CheckAggregate: two identical folds disagree: %q and %q", ValueText(want), ValueText(got))
	}

	// Merging the identity in must change nothing, from either side. An aggregate that counts
	// merges, or that treats an untouched state as "no data" and takes the other side wholesale,
	// fails exactly here.
	for _, c := range []struct {
		what       string
		data, unit bool // which side of the merge carries the samples
	}{{"acc.Merge(empty)", true, false}, {"empty.Merge(acc)", false, true}} {
		got, err := mergeOf(fold, nilIf(samples, !c.data), nilIf(samples, !c.unit))
		if err != nil {
			return err
		}
		if !same(want, got) {
			return fmt.Errorf("CheckAggregate: %s is not the identity: want %q, got %q",
				c.what, ValueText(want), ValueText(got))
		}
	}

	// Merge agrees with the serial fold, in both orders, at several split points -- not every
	// one, since that is quadratic in the sample and buys nothing: a merge that is wrong is
	// almost never wrong only at one cut.
	for _, k := range splitPoints(len(samples)) {
		a, b := samples[:k], samples[k:]
		ab, err := mergeOf(fold, a, b)
		if err != nil {
			return err
		}
		if !same(want, ab) {
			return fmt.Errorf("CheckAggregate: merge of a %d/%d split disagrees with the serial fold: want %q, got %q",
				len(a), len(b), ValueText(want), ValueText(ab))
		}
		ba, err := mergeOf(fold, b, a)
		if err != nil {
			return err
		}
		if !same(ab, ba) {
			return fmt.Errorf("CheckAggregate: merge is not commutative at a %d/%d split: %q one way, %q the other",
				len(a), len(b), ValueText(ab), ValueText(ba))
		}
	}

	// Associativity, over a three-way split. Commutativity above would miss a merge that is
	// symmetric but regroups badly -- one accumulating into a fixed-size reservoir, say, where
	// combining two halves then a third samples differently from combining one then two.
	i, j := len(samples)/3, 2*len(samples)/3
	p, q, r := samples[:i], samples[i:j], samples[j:]
	left, err := mergeOf3(fold, p, q, r, true)
	if err != nil {
		return err
	}
	right, err := mergeOf3(fold, p, q, r, false)
	if err != nil {
		return err
	}
	if !same(left, right) {
		return fmt.Errorf("CheckAggregate: merge is not associative: ((a+b)+c) is %q, (a+(b+c)) is %q",
			ValueText(left), ValueText(right))
	}
	if !same(want, left) {
		return fmt.Errorf("CheckAggregate: a three-way merge disagrees with the serial fold: want %q, got %q",
			ValueText(want), ValueText(left))
	}
	return nil
}

// nilIf returns nil when drop is set, so a merge-identity case can name which side is empty
// without building two near-identical folds.
func nilIf(vals []classad.Value, drop bool) []classad.Value {
	if drop {
		return nil
	}
	return vals
}

// mergeOf folds two parts separately, merges the second into the first, and renders.
func mergeOf(fold func([]classad.Value) (Accumulator, error), a, b []classad.Value) (classad.Value, error) {
	x, err := fold(a)
	if err != nil {
		return classad.Value{}, err
	}
	y, err := fold(b)
	if err != nil {
		return classad.Value{}, err
	}
	x.Merge(y)
	return x.Result(), nil
}

// mergeOf3 renders ((p+q)+r) when leftAssoc, else (p+(q+r)). Each call folds fresh parts, because
// Merge may consume what it is given.
func mergeOf3(fold func([]classad.Value) (Accumulator, error), p, q, r []classad.Value, leftAssoc bool) (classad.Value, error) {
	x, err := fold(p)
	if err != nil {
		return classad.Value{}, err
	}
	y, err := fold(q)
	if err != nil {
		return classad.Value{}, err
	}
	z, err := fold(r)
	if err != nil {
		return classad.Value{}, err
	}
	if leftAssoc {
		x.Merge(y)
		x.Merge(z)
		return x.Result(), nil
	}
	y.Merge(z)
	x.Merge(y)
	return x.Result(), nil
}

// splitPoints picks the cuts the merge laws are checked at: both ends, so the empty part is
// covered, and a few interior ones.
func splitPoints(n int) []int {
	seen := map[int]bool{}
	var out []int
	for _, k := range []int{0, 1, n / 3, n / 2, 2 * n / 3, n - 1, n} {
		if k < 0 || k > n || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}
