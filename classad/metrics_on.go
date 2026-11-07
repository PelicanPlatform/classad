//go:build classadmetrics

package classad

import "sync/atomic"

// Value-construction and comparison accounting, compiled in by -tags classadmetrics.
// See metrics_off.go for why these exist and what the ratio they measure means.
//
// A construction is one call to a Value constructor -- every place a Go value is
// wrapped into a Value. A comparison is one application of a comparison operator
// (< <= > >= == != =?= =!=), whether the predicate fast path decided it off the AST
// (comparisonsFast) or the ordinary Value path applied it (comparisonsSlow). Counting
// the ordinary ones at applyBinaryValues -- the dispatch site shared by the
// tree-walking evaluator and the bytecode interpreter -- rather than inside
// valuesEqual is what keeps != from counting twice, since it is implemented by
// negating ==.
//
// Atomics, not plain increments: the measurement harness is free to evaluate from
// several goroutines, and a data race in the instrumentation would be reported as a
// race in the evaluator.
var (
	valueConstructions atomic.Int64
	comparisonsSlow    atomic.Int64
	comparisonsFast    atomic.Int64
)

func countValue() { valueConstructions.Add(1) }

func countFastCompare() { comparisonsFast.Add(1) }

// countComparisonOp records one application of a comparison operator by the ordinary
// Value path, keyed on the operator's source spelling as applyBinaryValues receives it.
func countComparisonOp(op string) {
	switch op {
	case "<", "<=", ">", ">=", "==", "!=", "is", "isnt":
		comparisonsSlow.Add(1)
	}
}

// ResetEvalMetrics zeroes the counters. Build-tagged, so it is not part of the
// package's API.
func ResetEvalMetrics() {
	valueConstructions.Store(0)
	comparisonsSlow.Store(0)
	comparisonsFast.Store(0)
}

// EvalMetrics returns the Value constructions, the total comparison applications and
// the subset of those the predicate fast path decided, since the last reset.
func EvalMetrics() (constructions, comparisons, fast int64) {
	f := comparisonsFast.Load()
	return valueConstructions.Load(), comparisonsSlow.Load() + f, f
}
