package wasmudf

// Reason says why a call produced ERROR instead of the guest's answer.
//
// It exists because "the UDF returned ERROR" is not one event. A trap, a step
// budget, a wall-clock deadline and a malformed result slot are four different
// problems with four different fixes, and collapsing them into one value would
// leave an operator with a Requirements expression that is always ERROR and
// nothing to look at. Keeping them apart is also what lets a test assert that a
// particular module hits the DETERMINISTIC limit and never the clock -- an
// assertion that is impossible if both arrive as the same ERROR.
//
// A guest that deliberately returns the ClassAd value ERROR is NOT a failure and
// carries ReasonNone: it computed its answer, and its answer is ERROR.
type Reason uint8

// The failure classes. Appending is fine; renumbering is not, because Stats
// snapshots are keyed by these.
const (
	ReasonNone Reason = iota
	// ReasonArity -- argc outside the registered range. The registry checks
	// arity before evaluating arguments, so reaching here means a caller bypassed
	// it; the check is kept anyway because Call is exported.
	ReasonArity
	// ReasonUnsupportedArg -- an argument the ABI has no tag for: a list or a
	// nested ad. Detected before the guest runs.
	ReasonUnsupportedArg
	// ReasonArgsTooLarge -- the slots plus the string bytes do not fit the
	// region.
	ReasonArgsTooLarge
	// ReasonInstance -- no instance could be created (memory limit, or a start
	// section that traps on a later instantiation than the one load checked).
	ReasonInstance
	// ReasonTrap -- the guest trapped: unreachable, an out-of-bounds access, an
	// integer divide by zero, a stack overflow, a bad indirect call.
	ReasonTrap
	// ReasonStepLimit -- the guest spent its declared step budget. Deterministic.
	ReasonStepLimit
	// ReasonTimeout -- the wall-clock backstop fired. NOT deterministic, and its
	// separation from ReasonStepLimit is deliberate; see the package comment.
	ReasonTimeout
	// ReasonStatus -- the guest returned a nonzero status, meaning it declined to
	// produce a value.
	ReasonStatus
	// ReasonBadResult -- the guest reported success and left something that is
	// not a value: the poison tag (it never wrote the slot), an unknown tag, a
	// boolean payload above 1, a string pointer outside linear memory or with
	// bits above 32, or a string over the cap.
	ReasonBadResult
	// ReasonPanic -- something in the host call path panicked. Unreachable by
	// design; counted rather than assumed away, because the alternative to
	// counting it is a daemon dying of a bug in this package.
	ReasonPanic

	// The three below are DECLINES rather than failures: no record got ERROR,
	// the vector path simply did not run and the caller falls back to calling
	// per record. They are counted anyway, and OnFailure fires for them, because
	// a vector path that has quietly turned itself off is otherwise visible only
	// as a query being slower than it ought to be -- the hardest kind of problem
	// to find, since nothing is wrong with the answers.

	// ReasonBatchTooLarge -- the batch has more records, or more arguments, than
	// the region was sized for at load. Raise Options.MaxBatch or MaxBatchArgs.
	ReasonBatchTooLarge
	// ReasonBatchStrings -- an argument column holds strings or dictionary codes,
	// which the vector ABI does not carry. See Batch.
	ReasonBatchStrings
	// ReasonBatchShape -- the columns do not agree on a record count, a column is
	// shorter than it claims, the result column is not in the data form, or the
	// argument count is outside the registered arity.
	ReasonBatchShape
)

// reasonCount sizes the per-reason counters. It is derived from the last constant
// rather than written out, so adding a Reason cannot leave a counter unallocated.
const reasonCount = int(ReasonBatchShape) + 1

var reasonNames = [reasonCount]string{
	ReasonNone:           "none",
	ReasonArity:          "arity",
	ReasonUnsupportedArg: "unsupported argument",
	ReasonArgsTooLarge:   "arguments too large",
	ReasonInstance:       "instantiation failed",
	ReasonTrap:           "trap",
	ReasonStepLimit:      "step limit",
	ReasonTimeout:        "timeout",
	ReasonStatus:         "guest status",
	ReasonBadResult:      "malformed result",
	ReasonPanic:          "host panic",
	ReasonBatchTooLarge:  "batch too large",
	ReasonBatchStrings:   "batch holds strings",
	ReasonBatchShape:     "malformed batch",
}

func (r Reason) String() string {
	if int(r) >= len(reasonNames) {
		return "unknown"
	}
	return reasonNames[r]
}

// fromGuest reports whether this failure happened with the guest mid-execution,
// which is the test for whether its instance may be reused.
//
// It may not. A trapped or interrupted guest was stopped at an arbitrary
// instruction, so anything it keeps in linear memory -- an allocator's free list,
// a half-written buffer, an initialization flag -- is in whatever state that
// instruction left it. The instance is mechanically still callable (wazero only
// closes the module for the deadline case), and reusing it is the cheap thing to
// do, but the next record would then be computed by a guest whose own invariants
// are broken, and the plausible outcome of that is a wrong answer rather than a
// second failure. So the instance is dropped and a fresh one instantiated.
//
// The cost is one instantiation per failing record, which is only paid by a UDF
// that is already failing.
func (r Reason) fromGuest() bool {
	switch r {
	case ReasonTrap, ReasonStepLimit, ReasonTimeout, ReasonPanic:
		return true
	default:
		return false
	}
}
