package wasmudf

import (
	"errors"
	"math"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// THE ENGINE SIDE: a loaded wasm aggregate as a db.Aggregate.
//
// This file is why the module depends on db at all, and the dependency is not
// avoidable by the trick register.go uses for scalar functions. There, the
// registry is reached through a one-method interface whose function parameter is
// an unnamed type, so anything with a matching method satisfies it and no import
// is needed. An accumulator cannot be reached that way: db.Accumulator's Merge
// takes a db.Accumulator, and Go interface satisfaction is by type identity, so a
// structurally identical interface declared here would be a different type that
// nothing in db would accept.
//
// The edge runs wasmudf -> db and never the other way, which is what keeps wazero
// out of the database engine's dependency graph -- the reason wasmudf is its own
// module in the first place.

// The failures an accumulator raises that are the host's own judgement rather
// than something the guest did.
var (
	errUnsupportedAggValue = errors.New("a list or nested ad has no tag in this ABI")
	errStringTooLarge      = errors.New("one string is larger than the whole value arena")
	errForeignAccumulator  = errors.New("merged with an accumulator from another aggregate")
)

// Aggregate returns the loaded aggregate as a db.Aggregate, ready for
// db.RegisterAggregate.
//
// Its accumulators take column values -- that is, they implement
// db.ColumnAccumulator and the engine's columnar aggregate pass will serve them
// -- if and only if the module exports the numeric fold. An aggregate without it
// is served from the record scan, which is correct and slower, never wrong.
func (a *UDAF) Aggregate() db.Aggregate { return wasmAggregate{a: a} }

type wasmAggregate struct{ a *UDAF }

func (w wasmAggregate) NewAccumulator() db.Accumulator {
	acc := accum{a: w.a, state: w.a.newState()}
	if w.a.columnar {
		return &colAccum{accum: acc}
	}
	return &acc
}

// accum is one group's state for one wasm aggregate.
//
// It holds the state as bytes and buffers values until there are enough to be
// worth a guest call. Nothing here synchronizes, and nothing needs to: the engine
// builds one accumulator per worker on the calling goroutine, feeds each from
// exactly one goroutine, and merges them on the calling goroutine after every
// worker has returned.
type accum struct {
	a     *UDAF
	state []byte

	// pend are the values not yet folded; pendStr is how many bytes of string
	// payload they hold, which is what decides an early flush; pendNum is which
	// entry point they go to.
	pend    []aggVal
	pendStr int
	pendNum bool

	// res caches what Result computed, because Result may be called more than
	// once and must answer the same each time. Everything that can change the
	// state clears it.
	res    classad.Value
	hasRes bool
}

// core lets Merge reach the state of either accumulator shape through one type
// assertion. Without it the two shapes would be unrelated types to the merge
// path, which would then have to know which one the aggregate makes -- a fact it
// has no business depending on.
func (s *accum) core() *accum { return s }

type accumCore interface{ core() *accum }

// Add folds one row's argument value.
//
// A list or a nested ad FAILS THE QUERY rather than being skipped, which is
// deliberately not the quiet choice. This ABI has no tag for either, so the
// honest options are to refuse or to substitute something; substituting
// undefined would change the aggregate's answer on exactly the ads carrying a
// list, and an aggregate's answer is the kind of wrong that looks right.
func (s *accum) Add(v classad.Value) {
	av, ok := encodeAggValue(v)
	if !ok {
		panic(s.a.aggFail(exportAggFold, ReasonUnsupportedArg, errUnsupportedAggValue))
	}
	s.push(av, false)
}

// addNum is AddNum's body. It is UNEXPORTED here and exported only on colAccum,
// so that an accumulator from a module with no numeric fold does not accidentally
// satisfy db.ColumnAccumulator -- which is the interface the engine reads as the
// aggregate's promise that it may be fed from a column.
//
// The three kinds map onto tags the guest already reads from an ordinary fold, so
// a guest for which the column promises hold can export one function under both
// names. NumInt and NumBool carry the exact integer; NumReal carries the float,
// which is the only field meaningful for it.
func (s *accum) addNum(v db.NumValue) {
	switch v.Kind {
	case db.NumReal:
		s.push(aggVal{tag: tagReal, payload: math.Float64bits(v.F)}, true)
	case db.NumBool:
		b := uint64(0)
		if v.I != 0 {
			b = 1
		}
		s.push(aggVal{tag: tagBoolean, payload: b}, true)
	default:
		s.push(aggVal{tag: tagInteger, payload: uint64(v.I)}, true)
	}
}

// push buffers one value, flushing first if it would not fit or if it is going
// somewhere else.
//
// The buffer is bounded two ways because the region is: by the slot count and by
// the string arena after the slots. A single string larger than the whole arena
// cannot be folded at any batch size, so it fails rather than looping.
//
// The entry point switch matters only for an aggregate fed both ways in one
// group, which the engine does not do -- a query is served by one rung. It is
// handled anyway because the alternative is a buffer whose values go to whichever
// entry point the LAST one chose, which is a silently wrong answer for a caller
// doing something this package did not forbid.
func (s *accum) push(v aggVal, numeric bool) {
	s.hasRes = false
	if len(v.s) > s.a.opts.RegionBytes {
		panic(s.a.aggFail(exportAggFold, ReasonArgsTooLarge, errStringTooLarge))
	}
	if len(s.pend) > 0 && s.pendNum != numeric {
		s.flush()
	}
	if len(s.pend) >= s.a.opts.AggBatch || s.pendStr+len(v.s) > s.a.opts.RegionBytes {
		s.flush()
	}
	s.pendNum = numeric
	s.pend = append(s.pend, v)
	s.pendStr += len(v.s)
}

// flush folds the buffered values into the state.
//
// A FAILURE PANICS, which is how this engine contains a broken aggregate: per
// query, not per row. A scalar function that fails turns one record into ERROR,
// which says nothing about the others; an aggregate's answer is a function of
// every row, so a state that stopped being updated part-way is a wrong answer
// indistinguishable from a right one. The aggregate pass recovers this -- on the
// calling goroutine, even for a panic raised on a worker -- and reports it
// against the aggregate's registered name.
func (s *accum) flush() {
	if len(s.pend) == 0 {
		return
	}
	next, err := s.a.callFold(s.state, s.pend, s.pendNum)
	if err != nil {
		panic(err)
	}
	s.state = next
	s.pend = s.pend[:0]
	s.pendStr = 0
}

// Merge folds another accumulator's state into this one.
//
// BOTH ARE FLUSHED FIRST. A pending buffer is part of the state as far as the
// answer is concerned, and merging without folding it would drop every value
// either side had not yet reached a batch boundary with -- which, for a worker
// that drew one short segment, is all of them. That loss is silent: the answer is
// a number and it is simply too small.
func (s *accum) Merge(other db.Accumulator) {
	o, ok := other.(accumCore)
	if !ok {
		panic(s.a.aggFail(exportAggMerge, ReasonStatus, errForeignAccumulator))
	}
	b := o.core()
	if b.a != s.a {
		panic(s.a.aggFail(exportAggMerge, ReasonStatus, errForeignAccumulator))
	}
	s.flush()
	b.flush()
	next, err := s.a.callMerge(s.state, b.state)
	if err != nil {
		panic(err)
	}
	s.state = next
	s.hasRes = false
}

// Result renders the group's answer.
//
// Flushing here does change the accumulator's internals, and it has to: the
// pending values are part of the answer. What the contract forbids is the ANSWER
// moving between calls, which is why it is cached and why everything that can
// change the state clears the cache.
func (s *accum) Result() classad.Value {
	if s.hasRes {
		return s.res
	}
	s.flush()
	v, err := s.a.callResult(s.state)
	if err != nil {
		panic(err)
	}
	s.res, s.hasRes = v, true
	return v
}

// colAccum is an accumulator that also takes column values.
//
// It is a distinct TYPE rather than a flag because the engine decides whether an
// aggregate opted into the columnar pass by asserting db.ColumnAccumulator. A
// single type carrying a flag would be asserted successfully by every aggregate,
// including the ones that must not be fed from a column, and the promise would be
// unenforceable.
type colAccum struct{ accum }

// AddNum folds one numeric column value, with no classad.Value in between.
func (s *colAccum) AddNum(v db.NumValue) { s.addNum(v) }
