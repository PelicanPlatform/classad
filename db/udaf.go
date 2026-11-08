package db

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/classad"
)

// USER-DEFINED AGGREGATES.
//
// docs/user-defined-functions.md at the repository root is the prose version of this, for someone
// writing one: the merge laws, the columnar opt-in, and a worked example. What follows is why the
// registry is built this way.
//
// The expression layer already takes a user-defined scalar function, which covers projections and
// predicates. It does not cover aggregation, and aggregation is where a query layer over this
// engine spends its time -- so a story that stops at scalars stops short of the use that motivates
// it. An aggregate registered here is named in an AggSpec as AggUser plus the registered name, and
// the shared GROUP BY core reduces with it exactly as it reduces with SUM.
//
// WHAT IT COSTS, stated first because it is the thing a caller has to decide about. A built-in
// numeric aggregate reads its argument as a CONTIGUOUS COLUMN: one pass over the column's blocks,
// with a segment's zone map and each block's own min/max ruling whole blocks out before any value
// is touched, and no record decode and no boxing for the values that survive. A plain Accumulator
// is served from the RECORD SCAN instead -- every matching record decoded, its argument evaluated
// into a classad.Value, one call per row.
//
// AN ACCUMULATOR CAN OPT OUT OF THAT by also implementing ColumnAccumulator, which is fed from the
// same block-pruned unboxed pass that answers SUM, ungrouped and grouped alike. Implementing it is
// a declaration about order and about what is offered, neither of which a signature can check --
// see ColumnAccumulator for both, and CheckAggregate for the test. An aggregate that cannot make
// those promises stays on the record scan, which is correct and slower, never wrong.
//
// The one aggregate path a user-defined aggregate still cannot reach is the archive's
// index-resident GROUP BY count, which attributes a whole segment's postings without visiting a
// value. SUM, AVG, MIN and MAX do not use that one either, so it is not part of the gap.
//
// THE SAME CHANNEL COUNT(DISTINCT) ALREADY CUT. Its selector sits past AggMax, which is what routes
// a request carrying it onto the extended RPC opcodes and makes an older server refuse rather than
// answer a different aggregate; ColumnarAggregate, which serves the built-ins from precomputed
// statistics, declines it; and the materialized-view layer cannot name it. AggUser inherits all
// three by being past AggMax too, so this was a registration and a reduction case rather than a new
// mechanism. (The columnar refusal is ColumnarAggregate's alone: a ColumnAccumulator is served by
// ColumnarUserAggregate, a different entry on the same pass.)
//
// It needs ONE thing COUNT(DISTINCT) did not: a request has to say WHICH aggregate. A selector is
// the whole of COUNT(DISTINCT), and it is only half of this one, so the RPC layer carries the name
// beside the selector on opcodes of its own. The reduction here is unaffected either way -- it
// resolves a name against this process's registry whether the request came from an embedded caller
// or off the wire -- which is why an unknown name is an error rather than an undefined column: the
// caller is told what happened in both cases.

// Registration failures a caller can act on differently, as sentinels rather than only as messages.
var (
	// ErrAggregateExists means the name is already registered.
	ErrAggregateExists = errors.New("db: aggregate already registered")
	// ErrBadAggregate means the registration itself is malformed: a nil aggregate, or a name
	// no query could refer to.
	ErrBadAggregate = errors.New("db: invalid aggregate registration")
	// ErrNoSuchAggregate means a query named an aggregate that is not registered here. It is
	// returned rather than answered as undefined: a query asking for p95 and receiving a
	// column of "undefined" has been answered WRONGLY, where a query that fails says what
	// happened.
	ErrNoSuchAggregate = errors.New("db: no such aggregate")
)

// Aggregate is a user-defined aggregate: a factory for the per-group accumulators that do the
// work. It carries no state itself, because one registration serves every group of every
// concurrent query.
type Aggregate interface {
	// NewAccumulator returns a fresh accumulator holding the aggregate's identity -- the answer
	// for a group that contributed no rows at all.
	NewAccumulator() Accumulator
}

// Accumulator is one group's state for one user-defined aggregate.
//
// THE STATE IS THE IMPLEMENTATION'S OWN, which is a deliberate departure from how the built-ins
// work. A built-in SUM/AVG/MIN/MAX here RETAINS EVERY ARGUMENT VALUE in the group and hands the
// whole slice to the reference implementation at the end, so that integer-exact sums, int/real
// promotion, boolean coercion and error propagation match the reference exactly rather than being
// a re-implementation of it. That is parity bought with memory: built-in aggregate state is O(group
// size), not O(1). An aggregate with its own bounded state is therefore BETTER BEHAVED than the
// built-in it imitates, which is the argument for giving this interface a state of its own instead
// of modelling it on the built-in accumulator and handing an implementation the value slice.
//
// THE LAWS. Merge must be ASSOCIATIVE AND COMMUTATIVE, not merely associative. Nothing promises the
// order partial states are produced or combined in -- zone and segment identity has already
// produced one silent wrong-count bug in this engine -- so an aggregate whose merge is
// order-sensitive would be correct in every test and wrong in production, which is the worst
// failure mode available. Result must be repeatable and must not mutate the accumulator. Those are
// contracts an interface cannot enforce, so CheckAggregate exists to test one against them.
//
// MERGE IS CALLED, twice over, and not as decoration. The columnar pass fans out across segment
// windows and folds one accumulator per worker, so merge is the reduction of that fan-out; and two
// storage groups that render to the same label are merged into one row. Which worker draws which
// segment is a race, which is why associativity alone is not enough and commutativity is in the
// contract: an order-sensitive merge is correct in every test its author writes and wrong in
// production, plausibly and only sometimes.
type Accumulator interface {
	// Add folds one row's argument value into the state. The value is whatever the row's
	// argument attribute evaluated to, including undefined for a row that does not carry it;
	// skipping those is the aggregate's own decision, as it is for the built-ins.
	Add(v classad.Value)

	// Merge folds another accumulator's state into this one. The engine only ever passes an
	// accumulator made by the SAME registration, so an implementation may assert its own type;
	// a mismatch is a bug in the engine rather than an input to handle. Nothing reads other
	// afterwards, so an implementation may consume it rather than copying out of it.
	Merge(other Accumulator)

	// Result renders the group's answer. It may be called more than once and must return the
	// same value each time without changing the state.
	Result() classad.Value
}

// aggEntry is one registration. Read-only once published.
type aggEntry struct {
	name string // as registered, for diagnostics; lookups go by the folded name
	agg  Aggregate
}

// udafs is the registry, published as a whole map rather than mutated in place, so the read path
// is a single atomic load with no lock. Registration happens a handful of times in a process's
// life and lookup happens once per aggregate per query.
var udafs atomic.Pointer[map[string]aggEntry]

// validAggName reports whether name is one a query could refer to. The same shape as a function
// name, because that is what a query layer above this will have parsed it as.
func validAggName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// RegisterAggregate registers agg under name, for use as AggSpec{Func: AggUser, UserFunc: name}.
//
// It fails rather than overwriting, so that what an aggregate means cannot depend on package
// initialization order. A caller that MEANS to install over whatever is there says so, with
// ReplaceAggregate.
//
// THIS REGISTRY OFFERS REPLACEMENT AND WITHDRAWAL AND THE SCALAR FUNCTION REGISTRY DOES NOT, and
// the difference is where the decision is recorded. There, compiling an expression captures the
// decision that a name was a UDF, along with its arity, and compiled programs are cached and
// shared -- so withdrawing a scalar registration would leave cached programs calling something
// that is gone. Nothing of the sort happens here. An aggregate name is resolved against this
// registry once per query, when the reduction is set up (resolveAggregates, and the columnar
// passes beside it), and nothing caches that resolution past the query that made it. Replacing or
// removing a registration is therefore well defined, and ReplaceAggregate and UnregisterAggregate
// do it.
//
// ARITY DOES NOT CONSTRAIN A REPLACEMENT, because an aggregate registration does not carry one. A
// scalar registration declares min and max argument counts and every call site is checked against
// them when the expression compiles; an AggSpec names exactly one argument attribute whatever the
// aggregate is, so a replacement has no signature it could disagree with. What it has to keep is
// Accumulator's laws, which CheckAggregate tests and no registration can check.
func RegisterAggregate(name string, agg Aggregate) error {
	if agg == nil {
		return fmt.Errorf("RegisterAggregate(%q): nil aggregate: %w", name, ErrBadAggregate)
	}
	if !validAggName(name) {
		return fmt.Errorf("RegisterAggregate(%q): not a name a query can refer to: %w", name, ErrBadAggregate)
	}
	folded := strings.ToLower(name)
	entry := aggEntry{name: name, agg: agg}
	for {
		cur := udafs.Load()
		next := copyAggs(cur)
		if prev, dup := next[folded]; dup {
			return fmt.Errorf("RegisterAggregate(%q): registered as %q: %w", name, prev.name, ErrAggregateExists)
		}
		next[folded] = entry
		if udafs.CompareAndSwap(cur, &next) {
			return nil
		}
	}
}

// ReplaceAggregate installs agg under name whether or not something is already registered there.
// It is the overwrite RegisterAggregate refuses, for the caller holding aggregates somewhere
// durable -- a table of stored modules an administrative command writes to -- which has to be able
// to CORRECT one. Validation is RegisterAggregate's: a nil aggregate and a name no query could
// refer to are both ErrBadAggregate, so nothing unusable reaches the registry by this door either.
//
// IT REGISTERS AN ABSENT NAME rather than requiring a present one, because its caller is applying
// a stored definition and not editing a table: the command that writes a module and installs it
// runs the same line whether or not this process has seen that name before, and demanding presence
// would make every such caller write a lookup-then-branch that is also a race. A caller that wants
// the name to be free uses RegisterAggregate, which says so by failing.
//
// THE NAME IS STORED AS GIVEN, so a replacement under a different casing than the registration it
// replaces leaves ONE entry -- lookups fold case -- carrying the NEW spelling, which is what
// RegisteredAggregates then reports. The spelling is diagnostics, and the useful thing to report
// is how the aggregate that is installed was asked for, not how an earlier one was.
//
// A QUERY IN FLIGHT IS UNAFFECTED. A query resolves each name it uses to an Aggregate value once,
// before it reduces anything, and runs against that value; a replacement that lands mid-scan
// changes this map and reaches nothing already holding the old value. So a running query finishes
// under the implementation it started with, and one group's rows are never split across two of
// them -- which matters more than it sounds, because a half-and-half group is a wrong answer that
// looks exactly like a right one.
//
// WHAT IT DOES CHANGE IS WHAT A LATER QUERY MEANS, mid-process and with no synchronization point
// anywhere: two queries seconds apart, naming the same aggregate, can compute different things,
// and nothing in either answer says so. That is the hazard, and it is inherent in being able to
// correct a definition at all -- a caller that cares which definition produced a number has to
// record that itself, because this registry holds only the current one.
func ReplaceAggregate(name string, agg Aggregate) error {
	if agg == nil {
		return fmt.Errorf("ReplaceAggregate(%q): nil aggregate: %w", name, ErrBadAggregate)
	}
	if !validAggName(name) {
		return fmt.Errorf("ReplaceAggregate(%q): not a name a query can refer to: %w", name, ErrBadAggregate)
	}
	folded := strings.ToLower(name)
	entry := aggEntry{name: name, agg: agg}
	for {
		cur := udafs.Load()
		next := copyAggs(cur)
		next[folded] = entry
		if udafs.CompareAndSwap(cur, &next) {
			return nil
		}
	}
}

// UnregisterAggregate removes the registration of name, folding case as lookup does, and reports
// whether there was one to remove. It is for the caller taking a stored aggregate out of service,
// and it reports rather than returning nothing because "was it there" is the one thing that can
// surprise a caller reconciling a stored set against this process.
//
// It does not validate the name: a name no query could refer to is a name nothing can be
// registered under, so it is simply absent and the answer is false.
//
// A query in flight is unaffected, exactly as under ReplaceAggregate and for the same reason -- it
// holds the Aggregate it resolved, not this map. A query naming the aggregate AFTERWARDS fails
// with ErrNoSuchAggregate, the same answer a name that was never registered gets, since an unknown
// aggregate fails a query rather than being answered undefined.
func UnregisterAggregate(name string) bool {
	folded := strings.ToLower(name)
	for {
		cur := udafs.Load()
		if cur == nil {
			return false
		}
		if _, ok := (*cur)[folded]; !ok {
			return false
		}
		next := make(map[string]aggEntry, len(*cur))
		for k, v := range *cur {
			if k != folded {
				next[k] = v
			}
		}
		if udafs.CompareAndSwap(cur, &next) {
			return true
		}
	}
}

// copyAggs returns a mutable copy of a published registry map, which is how each writer here
// builds the version it tries to swap in: a published map is never written to, so a lookup is a
// load and nothing else.
func copyAggs(cur *map[string]aggEntry) map[string]aggEntry {
	n := 8
	if cur != nil && len(*cur) > n {
		n = len(*cur)
	}
	next := make(map[string]aggEntry, n)
	if cur != nil {
		for k, v := range *cur {
			next[k] = v
		}
	}
	return next
}

// LookupAggregate finds a registration by name, case-insensitively.
func LookupAggregate(name string) (Aggregate, bool) {
	m := udafs.Load()
	if m == nil {
		return nil, false
	}
	e, ok := (*m)[strings.ToLower(name)]
	if !ok {
		return nil, false
	}
	return e.agg, true
}

// RegisteredAggregates returns the registered names, as registered, for diagnostics. The order is
// unspecified.
func RegisteredAggregates() []string {
	m := udafs.Load()
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(*m))
	for _, e := range *m {
		out = append(out, e.name)
	}
	return out
}

// resolveAggregates looks up every user-defined aggregate a request names, returning them
// positionally so the reduction does no lookup per group. A nil entry means that position is a
// built-in.
//
// An unknown name fails the whole query. The alternative -- answering that column undefined -- is
// how the function selector behaves on a server too old to know it, and the comment there says why
// that is bad: visibly odd rather than plausibly wrong, but still not an error. Here there is no
// compatibility reason to tolerate it, so it is an error.
func resolveAggregates(aggs []AggSpec) ([]Aggregate, error) {
	var out []Aggregate
	for i, a := range aggs {
		if a.Func != AggUser {
			continue
		}
		if a.Arg == "*" || a.Arg == "" {
			return nil, fmt.Errorf("%s(*) is not meaningful; name an attribute", a.UserFunc)
		}
		agg, ok := LookupAggregate(a.UserFunc)
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrNoSuchAggregate, a.UserFunc)
		}
		if out == nil {
			out = make([]Aggregate, len(aggs))
		}
		out[i] = agg
	}
	return out, nil
}

// udafPanic reports a panic that escaped a user-defined aggregate, as an error naming it.
//
// CONTAINMENT IS PER QUERY, NOT PER ROW, which is the opposite of how a scalar user function is
// contained. That one turns a panic into an ERROR value, and can, because one record's value being
// ERROR is a perfectly ordinary outcome that says nothing about the others. An aggregate has no
// such room: a group's answer is a function of every row in it, so a state that has stopped being
// updated part-way does not yield a worse answer, it yields a WRONG one -- indistinguishable, from
// the outside, from a right one. The query fails instead.
//
// Per query also costs nothing per row, where a recovery around each Add would be a deferred call
// on the hottest loop in the reduction.
type udafPanic struct {
	name string
	val  any
}

func (e *udafPanic) Error() string {
	return fmt.Sprintf("aggregate %q panicked: %v", e.name, e.val)
}
