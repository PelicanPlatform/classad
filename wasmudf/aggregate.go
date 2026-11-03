package wasmudf

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/PelicanPlatform/classad/classad"
)

// A WASM AGGREGATE, as four operations over a state the host owns.
//
//	init    the identity: the answer for a group that saw no rows
//	fold    a batch of values into a state
//	merge   two states into one
//	result  a state into a value
//
// Fold and merge are PURE FUNCTIONS of their inputs as far as the host is
// concerned -- nothing about an accumulator survives in the guest between calls
// -- which is what lets any instance serve any accumulator, and lets two
// accumulators folded on different goroutines be merged without either of them
// having to be alive anywhere in particular. See abiagg.go for why the state
// crosses the boundary instead of staying behind a handle.
//
// MERGE IS NOT DECORATION HERE. The engine's columnar aggregate pass fans out
// across segment windows: one accumulator per worker, each fed by exactly one
// goroutine, folded on the calling goroutine once every worker has returned.
// Which worker draws which segment is a race, so merge has to be associative and
// commutative and an accumulator must not depend on the order values arrive in.
// A guest that gets this wrong is wrong only sometimes, and plausibly, which is
// why the laws are testable from the engine side (db.CheckAggregate) rather than
// asserted in a comment.

// UDAF is one loaded wasm aggregate.
//
// It is a separate type from UDF rather than a mode of it because the two share
// no entry point, and a module exports one set or the other. Merging them would
// mean every UDF carrying five nil function pointers and every load check
// branching on which kind it was looking at.
type UDAF struct {
	name string

	compiled wazero.CompiledModule
	loader   *Loader
	rt       wazero.Runtime
	opts     Options

	trusted bool
	timeout time.Duration
	region  int
	abi     uint32

	// columnar is whether the module exports the numeric fold entry point, and
	// so whether its accumulators take column values. Read from the COMPILED
	// MODULE, like the scalar vector entry point and for the same reason.
	columnar bool

	// identity is the state init produced, read ONCE at load and copied per
	// accumulator.
	//
	// Caching it is not only an optimization. db's columnar rung decides whether
	// an aggregate opted into the column pass by BUILDING an accumulator and
	// type-asserting it, and keeps that instance as worker zero rather than
	// discarding it, because constructing one may have side effects. With the
	// identity cached, constructing one here is a byte-slice copy and has none --
	// so that hazard does not reach this package, and db's care is still right
	// for the aggregates it cannot see.
	identity []byte

	free   chan *aggInstance
	tokens chan struct{}

	accums    atomic.Uint64
	folds     atomic.Uint64
	merges    atomic.Uint64
	results   atomic.Uint64
	values    atomic.Uint64
	instances atomic.Uint64
	failures  [reasonCount]atomic.Uint64
}

// Name returns the name the aggregate was loaded under.
func (a *UDAF) Name() string { return a.name }

// Trusted reports whether the module was labelled trusted at load.
func (a *UDAF) Trusted() bool { return a.trusted }

// Columnar reports whether the module exports the numeric fold entry point, and
// so whether its accumulators can be fed from a column. See AggregateOf.
func (a *UDAF) Columnar() bool { return a.columnar }

// AggStats is a snapshot of one aggregate's counters.
//
// Accumulators is the one an operator reads first: it is how many per-worker
// states a query made, so a value above one is the fan-out reaching the
// aggregate. A test asserting a fan-out happened has nothing else to assert on,
// since a pass that quietly stopped fanning out gives the same answers.
type AggStats struct {
	Accumulators uint64
	Folds        uint64
	Merges       uint64
	Results      uint64
	Values       uint64
	Failures     uint64
	Instances    uint64
	ByReason     map[Reason]uint64
}

// Stats snapshots the counters. The fields are read independently, so a snapshot
// taken while a query is running can be internally inconsistent by a call or two.
func (a *UDAF) Stats() AggStats {
	s := AggStats{
		Accumulators: a.accums.Load(),
		Folds:        a.folds.Load(),
		Merges:       a.merges.Load(),
		Results:      a.results.Load(),
		Values:       a.values.Load(),
		Instances:    a.instances.Load(),
		ByReason:     map[Reason]uint64{},
	}
	for r := range reasonCount {
		if n := a.failures[r].Load(); n != 0 {
			s.ByReason[Reason(r)] = n
			s.Failures += n
		}
	}
	return s
}

// errNoNumericFold is the impossible case in callFold: see there.
var errNoNumericFold = errors.New("column values offered to a module with no numeric fold")

// AggError is what a failed aggregate operation reports.
//
// It names the aggregate, because the engine turns it into a failed query and
// the one thing an operator needs from that is whose code did it.
type AggError struct {
	Name   string
	Op     string
	Reason Reason
	Err    error
}

func (e *AggError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("wasmudf: aggregate %s: %s: %s: %v", e.Name, e.Op, e.Reason, e.Err)
	}
	return fmt.Sprintf("wasmudf: aggregate %s: %s: %s", e.Name, e.Op, e.Reason)
}

func (e *AggError) Unwrap() error { return e.Err }

// LoadAggregate compiles wasm as an aggregate named name.
//
// Everything checkable is checked here, as for a scalar UDF and for the same
// reason -- but an aggregate has one more thing worth proving at load, and it is
// proven: init is CALLED, so a module whose identity state does not fit
// MaxStateBytes, or that refuses to produce one at all, is a load error naming
// the problem rather than a query that fails later for every caller.
func (l *Loader) LoadAggregate(ctx context.Context, name string, wasm []byte) (*UDAF, error) {
	return l.LoadAggregateWith(ctx, name, wasm, LoadOptions{})
}

// LoadAggregateWith is LoadAggregate with the per-module options an admin sets.
// An aggregate is exactly as untrusted as a function: same step budget, same
// memory cap, same wall-clock backstop, same meaning for Trusted.
func (l *Loader) LoadAggregateWith(ctx context.Context, name string, wasm []byte, lo LoadOptions) (*UDAF, error) {
	if name == "" {
		return nil, errors.New("wasmudf: empty aggregate name")
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, errors.New("wasmudf: loader is closed")
	}
	if _, dup := l.aggs[name]; dup {
		return nil, fmt.Errorf("wasmudf: aggregate %s is already loaded", name)
	}

	rt, err := l.runtime(ctx, lo.Trusted)
	if err != nil {
		return nil, fmt.Errorf("wasmudf: %s: %w", name, err)
	}
	compiled, err := rt.CompileModule(ctx, wasm)
	if err != nil {
		return nil, fmt.Errorf("wasmudf: %s: %w", name, err)
	}
	if cerr := checkImports(compiled); cerr != nil {
		_ = compiled.Close(ctx)
		return nil, fmt.Errorf("wasmudf: %s: %w", name, cerr)
	}
	if cerr := checkAggExports(compiled); cerr != nil {
		_ = compiled.Close(ctx)
		return nil, fmt.Errorf("wasmudf: %s: %w", name, cerr)
	}

	a := &UDAF{
		name:     name,
		compiled: compiled,
		loader:   l,
		rt:       rt,
		opts:     l.opts,
		trusted:  lo.Trusted,
		columnar: hasAggNumEntry(compiled),
		free:     make(chan *aggInstance, l.opts.MaxInstances),
		tokens:   make(chan struct{}, l.opts.MaxInstances),
	}
	a.timeout = l.opts.Timeout
	if lo.Trusted {
		a.timeout = -1
	}
	a.region = aggRegionBytes(l.opts.MaxStateBytes, l.opts.AggBatch, l.opts.RegionBytes)
	for range l.opts.MaxInstances {
		a.tokens <- struct{}{}
	}

	// The probe instance proves the module instantiates, speaks this ABI and can
	// produce an identity; it then goes into the pool rather than being closed,
	// exactly as the scalar loader does.
	<-a.tokens
	in, err := a.newInstance(ctx)
	if err != nil {
		a.tokens <- struct{}{}
		_ = compiled.Close(ctx)
		return nil, fmt.Errorf("wasmudf: %s: %w", name, err)
	}
	ver, verr := checkAggVersion(in)
	if verr != nil {
		a.discard(in)
		_ = compiled.Close(ctx)
		return nil, fmt.Errorf("wasmudf: %s: %w", name, verr)
	}
	a.abi = ver
	a.release(in)

	id, ierr := a.callInit()
	if ierr != nil {
		_ = compiled.Close(ctx)
		return nil, fmt.Errorf("wasmudf: %s: %w", name, ierr)
	}
	a.identity = id

	l.aggs[name] = a
	return a, nil
}

// LookupAggregate returns a loaded aggregate by name.
func (l *Loader) LookupAggregate(name string) (*UDAF, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.aggs[name]
	return a, ok
}

// checkAggExports verifies the aggregate entry points and their signatures before
// any instance exists.
func checkAggExports(c wazero.CompiledModule) error {
	if _, ok := c.ExportedMemories()[exportMemory]; !ok {
		return fmt.Errorf("no exported memory %q: there is nowhere to pass a state", exportMemory)
	}
	fns := c.ExportedFunctions()

	ver, ok := fns[exportAggVersion]
	if !ok {
		return fmt.Errorf("no exported function %q", exportAggVersion)
	}
	if len(ver.ParamTypes()) != 0 || len(ver.ResultTypes()) != 1 ||
		ver.ResultTypes()[0] != api.ValueTypeI32 {
		return fmt.Errorf("%s must be (func (result i32))", exportAggVersion)
	}

	for _, want := range []struct {
		name    string
		nparams int
	}{
		{exportAggInit, 2},
		{exportAggFold, 6},
		{exportAggMerge, 6},
		{exportAggResult, 3},
	} {
		fn, ok := fns[want.name]
		if !ok {
			return fmt.Errorf("no exported function %q", want.name)
		}
		if err := checkSignature(want.name, fn, want.nparams); err != nil {
			return err
		}
	}

	// The numeric fold is OPTIONAL and its presence is the declaration that this
	// aggregate may be fed from a column. An aggregate without it is served from
	// the record scan, which is correct and slower -- never wrong.
	if fn, ok := fns[exportAggFoldNum]; ok {
		if err := checkSignature(exportAggFoldNum, fn, 6); err != nil {
			return err
		}
	}
	return nil
}

// hasAggNumEntry reports whether the module exports the numeric fold. Read from
// the compiled module and nothing a module author could write in a name or a
// custom section.
//
// It is the guest declaring a property of its own ARITHMETIC -- that its answer
// does not depend on the order values arrive in, and that it does not count what
// it was not offered -- which is the guest's to declare in the way exporting a
// vector entry point is. It is not a capability and nothing is relaxed by it: a
// guest that declares it wrongly computes a wrong aggregate, the same way a guest
// that adds wrongly does.
func hasAggNumEntry(c wazero.CompiledModule) bool {
	_, ok := c.ExportedFunctions()[exportAggFoldNum]
	return ok
}

// checkAggVersion calls the guest's version export and returns the version it
// reports.
func checkAggVersion(in *aggInstance) (uint32, error) {
	res, err := in.version.Call(in.ctx)
	if err != nil {
		return 0, fmt.Errorf("calling %s: %w", exportAggVersion, err)
	}
	if len(res) != 1 {
		return 0, fmt.Errorf("%s returned %d values", exportAggVersion, len(res))
	}
	got := uint32(res[0])
	if got < aggABIVersionMin || got > aggABIVersionMax {
		return 0, fmt.Errorf("module speaks aggregate ABI version %d, host speaks %d..%d",
			got, aggABIVersionMin, aggABIVersionMax)
	}
	return got, nil
}

// aggInstance is one guest instance plus the entry points and the region layout,
// resolved once. Used by one call at a time, like a scalar instance and for the
// same reason: it has one linear memory.
type aggInstance struct {
	mod     api.Module
	version api.Function
	init    api.Function
	fold    api.Function
	foldNum api.Function
	merge   api.Function
	result  api.Function
	mem     api.Memory

	base, size uint32
	frame      aggFrame

	ctx  context.Context
	fuel callState

	// stack is the wasm call stack for CallWithStack, reused so a call allocates
	// neither its arguments nor its results. Six words: fold and merge take six
	// parameters, and one array sized for the widest is one fewer thing to get
	// wrong than four arrays.
	stack [6]uint64
}

func (in *aggInstance) callContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout < 0 {
		return in.ctx, nil
	}
	return context.WithTimeout(in.ctx, timeout)
}

func (a *UDAF) acquire() (*aggInstance, error) {
	<-a.tokens
	select {
	case in := <-a.free:
		return in, nil
	default:
	}
	in, err := a.newInstance(context.Background())
	if err != nil {
		a.tokens <- struct{}{}
		return nil, err
	}
	return in, nil
}

func (a *UDAF) release(in *aggInstance) {
	a.free <- in
	a.tokens <- struct{}{}
}

// discard closes an instance whose guest state can no longer be trusted. The
// state an aggregate cares about is in the host, so nothing is lost -- but a
// guest stopped at an arbitrary instruction has its own invariants broken, and
// the next fold through it would compute rather than fail.
func (a *UDAF) discard(in *aggInstance) {
	_ = in.mod.Close(context.Background())
	a.tokens <- struct{}{}
}

func (a *UDAF) newInstance(ctx context.Context) (*aggInstance, error) {
	in := &aggInstance{}
	in.ctx = context.WithValue(ctx, fuelKey{}, &in.fuel)
	in.fuel.steps = a.opts.Steps

	cfg := wazero.NewModuleConfig().WithName("").WithStartFunctions()
	mod, err := a.rt.InstantiateModule(in.ctx, a.compiled, cfg)
	if err != nil {
		return nil, err
	}
	in.mod = mod
	in.version = mod.ExportedFunction(exportAggVersion)
	in.init = mod.ExportedFunction(exportAggInit)
	in.fold = mod.ExportedFunction(exportAggFold)
	in.merge = mod.ExportedFunction(exportAggMerge)
	in.result = mod.ExportedFunction(exportAggResult)
	in.mem = mod.ExportedMemory(exportMemory)
	if in.version == nil || in.init == nil || in.fold == nil || in.merge == nil ||
		in.result == nil || in.mem == nil {
		_ = mod.Close(ctx)
		return nil, errors.New("instance is missing an export the module declares")
	}
	if a.columnar {
		in.foldNum = mod.ExportedFunction(exportAggFoldNum)
		if in.foldNum == nil {
			_ = mod.Close(ctx)
			return nil, errors.New("instance is missing the numeric fold the module declares")
		}
	}

	pages := uint32((a.region + 65535) / 65536)
	prev, ok := in.mem.Grow(pages)
	if !ok {
		_ = mod.Close(ctx)
		return nil, fmt.Errorf("cannot grow memory by %d pages for the %d-byte aggregate region",
			pages, a.region)
	}
	in.base = prev * 65536
	in.size = pages * 65536
	if uint64(in.base)+uint64(in.size) > uint64(in.mem.Size()) {
		_ = mod.Close(ctx)
		return nil, errors.New("grown memory does not contain the aggregate region")
	}
	in.frame = aggFrameAt(in.base, a.opts.MaxStateBytes, a.opts.AggBatch, a.opts.RegionBytes)

	a.instances.Add(1)
	return in, nil
}

// run borrows an instance, gives body one call's worth of deadline and budget,
// and disposes of the instance according to what happened.
//
// body returns ReasonNone or the reason it failed; a panic anywhere inside it is
// ReasonPanic, counted and returned as an error rather than escaping. An
// aggregate is called from inside a query, which the engine runs with no
// top-level recover on its workers, so a panic out of here would be a daemon
// dying of a bad module.
func (a *UDAF) run(op string, body func(in *aggInstance, ctx context.Context) (Reason, error)) error {
	in, err := a.acquire()
	if err != nil {
		return a.aggFail(op, ReasonInstance, err)
	}
	ctx, cancel := in.callContext(a.timeout)
	if cancel != nil {
		defer cancel()
	}

	r, cerr := func() (r Reason, cerr error) {
		// recover() must be called DIRECTLY by the deferred literal, or it is one
		// frame too deep and stops nothing.
		defer func() {
			if p := recover(); p != nil {
				r, cerr = ReasonPanic, fmt.Errorf("panic in wasm aggregate path: %v", p)
			}
		}()
		return body(in, ctx)
	}()

	if r.fromGuest() {
		a.discard(in)
	} else {
		a.release(in)
	}
	if r != ReasonNone {
		return a.aggFail(op, r, cerr)
	}
	return nil
}

func (a *UDAF) aggFail(op string, r Reason, err error) error {
	a.failures[r].Add(1)
	if a.opts.OnFailure != nil {
		a.opts.OnFailure(a.name, r, err)
	}
	return &AggError{Name: a.name, Op: op, Reason: r, Err: err}
}

// callInit asks the guest for the identity state. Called once, at load.
func (a *UDAF) callInit() ([]byte, error) {
	var out []byte
	err := a.run(exportAggInit, func(in *aggInstance, ctx context.Context) (Reason, error) {
		in.fuel.steps = a.opts.Steps
		in.stack[0] = uint64(in.frame.stateOut)
		in.stack[1] = uint64(in.frame.stateCap)
		if err := in.init.CallWithStack(ctx, in.stack[:2]); err != nil {
			return classifyCallError(err), err
		}
		st, r := readAggState(in.mem, in.frame.stateOut, in.frame.stateCap, int32(uint32(in.stack[0])))
		out = st
		return r, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// callFold folds a batch of values into state and returns the state the guest
// produced. numeric selects the column entry point.
func (a *UDAF) callFold(state []byte, vals []aggVal, numeric bool) ([]byte, error) {
	var out []byte
	err := a.run(exportAggFold, func(in *aggInstance, ctx context.Context) (Reason, error) {
		f := in.frame
		if r := writeAggState(in.mem, f.stateA, f.stateCap, state); r != ReasonNone {
			return r, nil
		}
		if r := writeAggValues(in.mem, f, vals); r != ReasonNone {
			return r, nil
		}
		in.fuel.steps = a.opts.Steps
		in.stack[0] = uint64(f.stateA)
		in.stack[1] = uint64(len(state))
		in.stack[2] = uint64(f.vals)
		in.stack[3] = uint64(len(vals))
		in.stack[4] = uint64(f.stateOut)
		in.stack[5] = uint64(f.stateCap)
		fn := in.fold
		if numeric {
			if in.foldNum == nil {
				// Unreachable: only a module exporting the numeric fold yields
				// an accumulator that can be fed column values at all. Checked
				// anyway, because the alternative to a loud impossible case is a
				// nil dereference arriving as "host panic" with nothing naming
				// what was missing.
				return ReasonStatus, errNoNumericFold
			}
			fn = in.foldNum
		}
		if err := fn.CallWithStack(ctx, in.stack[:6]); err != nil {
			return classifyCallError(err), err
		}
		st, r := readAggState(in.mem, f.stateOut, f.stateCap, int32(uint32(in.stack[0])))
		out = st
		return r, nil
	})
	if err != nil {
		return nil, err
	}
	a.folds.Add(1)
	a.values.Add(uint64(len(vals)))
	return out, nil
}

// callMerge folds two states into one.
//
// BOTH states are copied into the guest, into buffers that do not overlap, and
// the guest writes a third. That is the whole reason the state is the host's:
// the two accumulators being merged were folded on different goroutines, in
// whichever instances happened to be free, and neither of those instances need
// still exist.
func (a *UDAF) callMerge(x, y []byte) ([]byte, error) {
	var out []byte
	err := a.run(exportAggMerge, func(in *aggInstance, ctx context.Context) (Reason, error) {
		f := in.frame
		if r := writeAggState(in.mem, f.stateA, f.stateCap, x); r != ReasonNone {
			return r, nil
		}
		if r := writeAggState(in.mem, f.stateB, f.stateCap, y); r != ReasonNone {
			return r, nil
		}
		in.fuel.steps = a.opts.Steps
		in.stack[0] = uint64(f.stateA)
		in.stack[1] = uint64(len(x))
		in.stack[2] = uint64(f.stateB)
		in.stack[3] = uint64(len(y))
		in.stack[4] = uint64(f.stateOut)
		in.stack[5] = uint64(f.stateCap)
		if err := in.merge.CallWithStack(ctx, in.stack[:6]); err != nil {
			return classifyCallError(err), err
		}
		st, r := readAggState(in.mem, f.stateOut, f.stateCap, int32(uint32(in.stack[0])))
		out = st
		return r, nil
	})
	if err != nil {
		return nil, err
	}
	a.merges.Add(1)
	return out, nil
}

// callResult renders a state as a value.
func (a *UDAF) callResult(state []byte) (classad.Value, error) {
	var out classad.Value
	err := a.run(exportAggResult, func(in *aggInstance, ctx context.Context) (Reason, error) {
		f := in.frame
		if r := writeAggState(in.mem, f.stateA, f.stateCap, state); r != ReasonNone {
			return r, nil
		}
		// Poisoned before the call, not after: a guest that returns success
		// without writing the slot would otherwise be credited with whatever the
		// previous use of this instance left there.
		if !in.mem.WriteUint32Le(f.result+offTag, resultPoison) {
			return ReasonBadResult, nil
		}
		in.fuel.steps = a.opts.Steps
		in.stack[0] = uint64(f.stateA)
		in.stack[1] = uint64(len(state))
		in.stack[2] = uint64(f.result)
		if err := in.result.CallWithStack(ctx, in.stack[:3]); err != nil {
			return classifyCallError(err), err
		}
		if status := uint32(in.stack[0]); status != statusOK {
			return ReasonStatus, fmt.Errorf("guest returned status %d", status)
		}
		v, r := readResult(in.mem, f.result, a.opts.MaxResultBytes)
		out = v
		return r, nil
	})
	if err != nil {
		return classad.NewErrorValue(), err
	}
	a.results.Add(1)
	return out, nil
}

// newState returns a fresh copy of the identity.
//
// The copy is what keeps one accumulator's state from being another's. Nothing
// today writes into a state in place -- a fold reads the old one and the guest
// writes a new one -- so removing the copy is currently invisible to every test
// here, and it is kept anyway: the obvious next optimization is to read the
// guest's answer back into the buffer that is already there, and with the
// identity shared that would corrupt every accumulator in the process at once,
// including those of other queries on other goroutines.
func (a *UDAF) newState() []byte {
	a.accums.Add(1)
	return slices.Clone(a.identity)
}
