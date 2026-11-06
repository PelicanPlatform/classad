//go:build !classadmetrics

package classad

// Value-construction and comparison accounting, compiled out.
//
// The question these counters answer is a design question, not a monitoring one:
// how many Values does an evaluation construct per comparison it decides? At 1:1
// every Value is built for exactly one comparison, and no scheme that makes the
// comparison cheaper by making the Value richer -- an eager or lazy prefix,
// interning -- can ever amortize. The predicate fast path (predicate.go) exists to
// move that ratio, so the ratio has to be MEASURED rather than inferred from a
// benchmark that moved.
//
// All three hooks are empty functions in the ordinary build. The compiler inlines
// them to nothing, so the shipped evaluator carries no counter, no atomic and no
// branch; build with -tags classadmetrics to turn them on, and see
// TestConstructionRatio.
func countValue() {}

func countFastCompare() {}

func countComparisonOp(string) {}
