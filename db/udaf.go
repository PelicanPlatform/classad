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
// The expression layer already takes a user-defined scalar function, which covers projections and
// predicates. It does not cover aggregation, and aggregation is where a query layer over this
// engine spends its time -- so a story that stops at scalars stops short of the use that motivates
// it. An aggregate registered here is named in an AggSpec as AggUser plus the registered name, and
// the shared GROUP BY core reduces with it exactly as it reduces with SUM.
//
// WHAT IT COSTS, stated first because it is the thing a caller has to decide about. A built-in
// numeric aggregate reads its argument as a CONTIGUOUS COLUMN: one pass over the column's blocks,
// with a segment's zone map and each block's own min/max ruling whole blocks out before any value
// is touched, and no record decode and no boxing for the values that survive. A user-defined
// aggregate is served from the RECORD SCAN instead -- every matching record decoded, its argument
// evaluated into a classad.Value, one call per row.
//
// So the honest headline is not "slower than a built-in": A USER-DEFINED AGGREGATE FORFEITS THE
// COLUMNAR VALUE PASS. What is lost is the column layout, the block pruning and the absence of
// boxing. What is NOT lost is reading the values, because the columnar pass reads them too -- it
// computes the statistics it reports, it does not look them up. (The one aggregate path that truly
// visits no values is the archive's index-resident GROUP BY count, which attributes a whole
// segment's postings; SUM, AVG, MIN and MAX do not use that one either.)
//
// That distinction is worth keeping straight, because it says what could change. Nothing about this
// interface forecloses a column-shaped accumulate riding the same pass; an aggregate that could be
// handed a column rather than a value per row would lose only the statistics shortcut itself. The
// cliff is deliberately left visible meanwhile: ColumnarAggregate declines AggUser outright.
//
// THE SAME CHANNEL COUNT(DISTINCT) ALREADY CUT. Its selector sits past AggMax, which is what routes
// a request carrying it onto the extended RPC opcodes and makes an older server refuse rather than
// answer a different aggregate; it declines the columnar path for the same reason as above; and the
// materialized-view layer cannot name it. AggUser inherits all three behaviours by being past
// AggMax too, so this is a registration and a reduction case rather than a new mechanism.

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
// MERGE IS REQUIRED AND IS NOT YET CALLED, which is worth saying plainly rather than implying a
// parallelism that is not there. The reduction core today is strictly serial over one sequence, so
// every accumulator this package builds is folded by Add alone. Merge is in the interface anyway
// because leaving it out would bake that serialism into the contract: the moment a per-segment or
// parallel reduction exists, an interface without merge is single-threaded BY CONSTRUCTION in the
// one place the engine is not, and every aggregate already written would have to be revisited.
// Requiring it now costs an implementation a method; adding it later costs everyone a rewrite.
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
// initialization order.
//
// THERE IS NO UNREGISTER, and the reason is NOT the one that makes the scalar function registry
// append-only. There, a compiled program captures the decision that a name was a UDF, programs are
// cached, and withdrawing a registration would leave cached programs calling something that is
// gone. Nothing of the sort applies here: an aggregate name is resolved once per query, so
// withdrawing one would be perfectly well defined. It is append-only because nothing needs the
// other thing, and an API with no caller is a maintenance cost with no return -- which is a reason
// that would stop applying if a caller turned up, where the scalar registry's would not.
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
		next := make(map[string]aggEntry, 8)
		if cur != nil {
			for k, v := range *cur {
				next[k] = v
			}
		}
		if prev, dup := next[folded]; dup {
			return fmt.Errorf("RegisterAggregate(%q): registered as %q: %w", name, prev.name, ErrAggregateExists)
		}
		next[folded] = entry
		if udafs.CompareAndSwap(cur, &next) {
			return nil
		}
	}
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
