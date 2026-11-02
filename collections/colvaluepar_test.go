package collections

import (
	"math"
	"math/rand"
	"testing"
)

// statsAccum.merge has to produce what one pass over the same values in one accumulator produces --
// every field, not just the sum. The fields that make this non-trivial are the ones a plain "add
// them up" merge gets wrong: IntMin/IntMax hold nothing until anyInt, and Min/Max are seeded to
// infinities that an empty side must not contribute.
//
// EXACTLY ONE FIELD IS ALLOWED TO DIFFER, and the test says which: Sum, the float64 accumulation,
// is not associative, so folding the same values in two groupings can land on different last bits.
// That is the accepted cost of fanning the pass out -- an exact float sum needs a fixed summation
// order, which is the thing a work-stealing fan-out gives up. It is bounded rather than waved at:
// the difference must stay within a relative epsilon of the sum of the magnitudes, which is the
// error bound float summation actually obeys. Everything else is exact and compared as such --
// IntSum accumulates in int64, and Min/Max are comparisons rather than accumulations, so no
// grouping can move them. This test failed the first time it ran, on Sum and on nothing else.
func TestStatsAccumMergeMatchesOnePass(t *testing.T) {
	kinds := []adKind{akInt, akReal, akBool}
	rng := rand.New(rand.NewSource(7))
	for trial := 0; trial < 200; trial++ {
		n := rng.Intn(24)
		vals := make([]colVal, n)
		for i := range vals {
			k := kinds[rng.Intn(len(kinds))]
			switch k {
			case akInt:
				iv := int64(rng.Intn(2001) - 1000)
				vals[i] = colVal{f: float64(iv), i: iv, kind: akInt}
			case akBool:
				iv := int64(rng.Intn(2))
				vals[i] = colVal{f: float64(iv), i: iv, kind: akBool}
			default:
				f := rng.NormFloat64() * 1000
				if rng.Intn(16) == 0 {
					f = math.NaN()
				}
				vals[i] = colVal{f: f, kind: akReal}
			}
		}
		serial := newStatsAccum()
		sumAbs := 0.0
		for _, v := range vals {
			serial.add(v)
			if !math.IsNaN(v.f) {
				sumAbs += math.Abs(v.f)
			}
		}
		// Split at every point, including the ends, because the interesting mistakes are at the
		// boundaries: merging an EMPTY side is what the seeded infinities and the anyInt guard
		// exist for, and a one-element side is what a merge that silently assumes two gets wrong.
		for split := 0; split <= n; split++ {
			a, b := newStatsAccum(), newStatsAccum()
			for _, v := range vals[:split] {
				a.add(v)
			}
			for _, v := range vals[split:] {
				b.add(v)
			}
			a.merge(&b)
			if !equalStats(a.result(), serial.result(), sumAbs) || a.anyInt != serial.anyInt {
				t.Fatalf("trial %d split %d/%d: merged %+v (anyInt=%v), one pass %+v (anyInt=%v)",
					trial, split, n, a.result(), a.anyInt, serial.result(), serial.anyInt)
			}
		}
	}
}

// TestStatsAccumMergeIsCommutative: which worker finishes first is a race, so the fold order is not
// fixed and the merge has to be insensitive to it.
func TestStatsAccumMergeIsCommutative(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for trial := 0; trial < 200; trial++ {
		mk := func(n int) statsAccum {
			a := newStatsAccum()
			for i := 0; i < n; i++ {
				iv := int64(rng.Intn(201) - 100)
				if rng.Intn(3) == 0 {
					a.add(colVal{f: rng.NormFloat64(), kind: akReal})
					continue
				}
				a.add(colVal{f: float64(iv), i: iv, kind: akInt})
			}
			return a
		}
		x, y := mk(rng.Intn(10)), mk(rng.Intn(10))
		ab, ba := x, y
		ab.merge(&y)
		ba.merge(&x)
		if !equalStats(ab.result(), ba.result(), math.Inf(1)) {
			t.Fatalf("trial %d: a.merge(b) %+v, b.merge(a) %+v", trial, ab.result(), ba.result())
		}
	}
}

// equalStats compares two NumStats. Every field is exact except Sum, which is allowed to differ by
// a relative epsilon of sumAbs -- the sum of the magnitudes that went in, which is what bounds the
// error of a float summation done in a different order. Pass sumAbs = +Inf to accept any Sum, for a
// comparison where the inputs are not to hand.
//
// NaN reads as equal to NaN throughout: an aggregate over a column holding one legitimately carries
// NaN in Sum and in the extremes, and == would call that a difference on every comparison.
func equalStats(a, b NumStats, sumAbs float64) bool {
	eq := func(x, y float64) bool { return x == y || (math.IsNaN(x) && math.IsNaN(y)) }
	sumOK := eq(a.Sum, b.Sum) || math.Abs(a.Sum-b.Sum) <= 1e-12*sumAbs
	return a.N == b.N && sumOK && eq(a.Min, b.Min) && eq(a.Max, b.Max) &&
		a.IntSum == b.IntSum && a.IntMin == b.IntMin && a.IntMax == b.IntMax &&
		a.AnyReal == b.AnyReal && a.AnyBool == b.AnyBool && a.AnyNaN == b.AnyNaN
}
