// Package scriptagg registers an aggregate written in ClassAd Script, so a GROUP BY can name it
// exactly as it names SUM.
//
// It is the aggregate counterpart of collections/scriptudf, which registers a script function as a
// scalar user-defined function, and the two are deliberately the same shape: a module exports
// functions by name, registration picks the ones it needs, and what the engine ends up holding is
// the same kind of thing a Go or wasm implementation would have produced.
//
// AN AGGREGATE IS FOUR PURE FUNCTIONS THREADING A STATE VALUE, and that is the whole of the
// language support it needs. There is no accumulator object in the script, no mutation and no
// handle:
//
//	def init():            the identity -- the answer for a group that contributed no rows
//	def add(state, v):     the state with one row's value folded in
//	def merge(a, b):       two partial states combined
//	def result(state):     the group's answer
//
// The state is an ordinary ClassAd value and the Go side holds exactly one of them per group. A
// scalar state -- a running sum, a maximum, a count -- is a number; anything needing two numbers at
// once is a LIST, which a module function builds natively because it has no ad for a list literal
// to be lazy against (see vm's OpMakeList). That is why AVG is expressible here at all: its state
// is a sum and a count, and a scalar cannot carry both.
//
// THE LAWS ARE THE SCRIPT AUTHOR'S TO KEEP. db.Accumulator states them -- merge associative AND
// commutative, result repeatable and non-mutating -- and nothing here can check them, because the
// four functions are the author's code. db.CheckAggregate tests one against them over a sample, and
// an aggregate written here should be run through it in the author's own tests before it is
// registered. The failure it prevents is the bad one: the columnar pass fans out across segment
// windows and which worker draws which segment is a race, so an order-sensitive merge is right in
// every test its author writes and wrong in production, plausibly and only sometimes.
//
// WHY THIS LIVES UNDER db AND NOT BESIDE scriptudf. An accumulator cannot be reached through a
// structurally-matching interface the way the scalar registry can: db.Accumulator's Merge takes a
// db.Accumulator, and Go satisfies interfaces by type identity, so a copy declared elsewhere would
// be a different type db would not accept. The import therefore has to be real, and collections --
// where vm lives -- is the module db depends on, so the edge can only run this way round. wasmudf
// reaches the same conclusion for the same reason.
package scriptagg

import (
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/db"
)

// ErrBadSpec is returned when the module does not offer the four functions this needs, in the
// shapes it needs them. It wraps whatever the compiler said, so a loader can print one error and a
// caller can still tell a malformed aggregate from a missing name.
var ErrBadSpec = errors.New("scriptagg: the module does not define a usable aggregate")

// errForeignAccumulator is the engine-bug case: a merge with an accumulator from some other
// registration. See accum.Merge.
var errForeignAccumulator = errors.New("scriptagg: merged with an accumulator from another aggregate")

// Spec names the module's four functions and states what the author promises about them.
//
// An empty name means the default, which is the function's role spelled in lower case -- init, add,
// merge, result. A module written for this package needs no Spec fields at all; the names exist for
// a module that exports several aggregates and has to tell them apart.
type Spec struct {
	Init, Add, Merge, Result string

	// Columnar opts the aggregate into the columnar value pass, which is how SUM is served: one
	// pass over the column's blocks, whole blocks ruled out by the zone map, no record decoded.
	// The alternative is the record scan, which is correct and slower, never wrong.
	//
	// IT IS A PROMISE, NOT A SPEED SETTING, and the promise is one this package cannot check for
	// the author. The pass yields values in BLOCK ORDER, which is not insertion order and not the
	// same order twice, so `add` must give the same answer whatever order it sees the values in;
	// and it yields only the column's NUMERIC values, so a row whose attribute is absent or a
	// string is never offered -- an `add` that counts those would count a different set of rows on
	// the two rungs and answer the same query two ways depending on what has been sealed.
	//
	// db.CheckAggregate tests both of those for an aggregate that declares it. Set this, run
	// CheckAggregate, and keep the test.
	Columnar bool
}

// names fills in the defaults.
func (s Spec) names() (init, add, merge, result string) {
	pick := func(given, dflt string) string {
		if given == "" {
			return dflt
		}
		return given
	}
	return pick(s.Init, "init"), pick(s.Add, "add"), pick(s.Merge, "merge"), pick(s.Result, "result")
}

// Aggregate is a compiled script aggregate, ready for db.RegisterAggregate.
//
// It is READ-ONLY once built, apart from its counters, and safe to use from any number of
// goroutines: a vm.ScriptFunc allocates its own register file per call, and the state an
// accumulator threads is a value rather than a reference into anything shared.
type Aggregate struct {
	init, add, merge, result *vm.ScriptFunc

	// identity is what init returned, read ONCE when the aggregate is built and handed to every
	// accumulator.
	//
	// Sharing it is sound because a value is not written through: an accumulator replaces its state
	// with whatever `add` returned rather than modifying the one it had. It also matters for a
	// reason that is not speed: db's columnar rung decides whether an aggregate opted into the
	// column pass by BUILDING an accumulator and type-asserting it, so constructing one has to be
	// free of consequences. With init called once here, it is.
	identity classad.Value

	columnar bool

	accums  atomic.Uint64
	merges  atomic.Uint64
	results atomic.Uint64
}

// Stats is a snapshot of one aggregate's counters.
//
// Accumulators is the one to read first: it is how many per-group, per-worker states a query made,
// so a value above the group count is the columnar fan-out reaching the aggregate. A test asserting
// that a fan-out happened has nothing else to assert on, because a pass that quietly stopped
// fanning out gives the same answers.
//
// THERE IS NO PER-ROW COUNTER. Add is the hot loop -- once per row per group -- and an atomic
// increment on it would be a contended write on the one path that has to stay cheap. Merges and
// Results are per partial state and per group, which is thousands of times rarer.
type Stats struct {
	Accumulators uint64
	Merges       uint64
	Results      uint64
}

// Stats snapshots the counters. The fields are read independently, so a snapshot taken while a
// query is running can be internally inconsistent by a call or two.
func (a *Aggregate) Stats() Stats {
	return Stats{
		Accumulators: a.accums.Load(),
		Merges:       a.merges.Load(),
		Results:      a.results.Load(),
	}
}

// Columnar reports whether the aggregate declared itself fit for the columnar value pass.
func (a *Aggregate) Columnar() bool { return a.columnar }

// New compiles the module's four functions and returns the aggregate they describe.
//
// EVERYTHING CHECKABLE IS CHECKED HERE, which is the same order scriptudf uses and matters for the
// same reason: a registration that can fail at query time fails for every caller of that query, and
// says so in a place nobody is watching. So each function is compiled -- which applies the purity
// gate, so a function reaching the ad is refused here -- each arity is checked against the role it
// fills, and `init` is CALLED, so an aggregate whose identity is itself a failure is a build error
// rather than a column of ERROR later.
//
// The arities are not negotiable because the engine supplies the arguments: init takes none, add
// takes the state and one row's value, merge takes two states, result takes one.
func New(mod *vm.ScriptModule, spec Spec) (*Aggregate, error) {
	initName, addName, mergeName, resultName := spec.names()
	a := &Aggregate{columnar: spec.Columnar}
	for _, want := range []struct {
		role  string // the part this function plays
		name  string // what the module calls it
		arity int
		takes string // what the engine passes it, for the refusal
		dst   **vm.ScriptFunc
	}{
		{"init", initName, 0, "nothing", &a.init},
		{"add", addName, 2, "the state and one row's value", &a.add},
		{"merge", mergeName, 2, "two states", &a.merge},
		{"result", resultName, 1, "the state", &a.result},
	} {
		f, err := mod.Compile(want.name)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrBadSpec, want.role, err)
		}
		if f.Arity() != want.arity {
			return nil, fmt.Errorf("%w: %s is the aggregate's %s, so the engine calls it with %s "+
				"-- %d parameters -- but it declares %d",
				ErrBadSpec, want.name, want.role, want.takes, want.arity, f.Arity())
		}
		*want.dst = f
	}
	a.identity = a.init.Call(nil)
	return a, nil
}

// Register compiles the aggregate and registers it under name, so a request naming
// db.AggSpec{Func: db.AggUser, UserFunc: name} reaches it.
//
// Compilation comes first and registration second, as it does for a scalar script function: a name
// is only ever registered against four functions that compiled, passed the purity gate and produced
// an identity. The registry's own failures come back unwrapped -- db.ErrAggregateExists for a name
// already taken, db.ErrBadAggregate for one no query could refer to.
func Register(name string, mod *vm.ScriptModule, spec Spec) (*Aggregate, error) {
	a, err := New(mod, spec)
	if err != nil {
		return nil, err
	}
	if err := db.RegisterAggregate(name, a); err != nil {
		return a, err
	}
	return a, nil
}

// NewAccumulator returns a fresh accumulator holding the identity.
func (a *Aggregate) NewAccumulator() db.Accumulator {
	a.accums.Add(1)
	acc := accum{a: a, state: a.identity}
	if a.columnar {
		return &colAccum{accum: acc}
	}
	return &acc
}

// accum is one group's state: one classad.Value and the aggregate that knows what to do with it.
//
// Nothing here synchronizes, and nothing needs to. The engine builds one accumulator per worker on
// the calling goroutine, feeds each from exactly one goroutine, and merges them on the calling
// goroutine once every worker has returned.
type accum struct {
	a     *Aggregate
	state classad.Value

	// res caches what result returned, because Result may be called more than once and must answer
	// the same each time. Add and Merge clear it, being the two things that can change the state.
	res    classad.Value
	hasRes bool
}

// core lets Merge reach either accumulator shape's state through one type assertion, so the merge
// path does not have to know which shape this aggregate makes.
func (s *accum) core() *accum { return s }

type accumCore interface{ core() *accum }

// Add folds one row's value into the state.
//
// EVERY VALUE A ROW CAN HOLD IS PASSED THROUGH, including UNDEFINED for a row that does not carry
// the attribute and ERROR for one whose expression failed. Skipping those is the script's decision,
// exactly as it is the built-ins': `add` is where `isUndefined(v)` belongs, and an `add` that does
// not test for them will fold them and answer accordingly.
//
// A script that runs out of its step budget returns ERROR, which is also a value a script may
// choose to return, so the two are not distinguishable from here. That is the argument for the
// budget being large rather than for a second channel: an aggregate whose `add` can loop long
// enough to hit it is one whose answer was not going to arrive either way.
func (s *accum) Add(v classad.Value) {
	s.state = s.a.add.Call([]classad.Value{s.state, v})
	s.hasRes = false
}

// addNum is AddNum's body, unexported here and exported only on colAccum so that an aggregate that
// did not declare itself columnar cannot satisfy db.ColumnAccumulator by accident -- that interface
// is read as the promise, so it must be impossible to make without saying so.
//
// A column value is boxed back into the classad.Value the script would have been given by the
// record scan, which is what makes the two rungs the same code rather than two implementations that
// have to be kept agreeing. NumInt and NumBool carry the exact integer; NumReal carries the float.
func (s *accum) addNum(v db.NumValue) {
	switch v.Kind {
	case db.NumReal:
		s.Add(classad.NewRealValue(v.F))
	case db.NumBool:
		s.Add(classad.NewBoolValue(v.I != 0))
	default:
		s.Add(classad.NewIntValue(v.I))
	}
}

// Merge folds another accumulator's state into this one by calling the script's `merge`.
//
// A FOREIGN ACCUMULATOR PANICS. The engine only ever merges accumulators from the same
// registration, so one from elsewhere is a bug in the engine rather than an input to handle, and
// the aggregate pass turns a panic out of here into a failed query naming the aggregate. Answering
// a wrong number quietly is the alternative, and an aggregate's answer is the kind of wrong that
// looks right.
func (s *accum) Merge(other db.Accumulator) {
	o, ok := other.(accumCore)
	if !ok {
		panic(errForeignAccumulator)
	}
	b := o.core()
	if b.a != s.a {
		panic(errForeignAccumulator)
	}
	s.a.merges.Add(1)
	s.state = s.a.merge.Call([]classad.Value{s.state, b.state})
	s.hasRes = false
}

// Result renders the group's answer, once, and remembers it.
//
// The contract forbids the ANSWER moving between calls and forbids Result mutating the state;
// calling the script again would satisfy neither cheaply nor obviously, since `result` is the
// author's code and only its purity is proven, not its agreement with itself. Caching makes the
// repeatability this package's property rather than the script's.
func (s *accum) Result() classad.Value {
	if s.hasRes {
		return s.res
	}
	s.a.results.Add(1)
	s.res, s.hasRes = s.a.result.Call([]classad.Value{s.state}), true
	return s.res
}

// colAccum is an accumulator that also takes column values. A distinct type, not a flag: see
// accum.addNum.
type colAccum struct{ accum }

// AddNum folds one numeric column value.
func (s *colAccum) AddNum(v db.NumValue) { s.addNum(v) }
