package wasmudf

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/sys"

	"github.com/PelicanPlatform/classad/classad"
)

// errStepLimit is what the fuel import panics with when the budget is gone.
//
// A panic is how a host function traps in wazero: it recovers it, wraps it and
// returns it from the guest call, so errors.Is finds it here. It is not a panic
// that can escape into the caller -- it crosses exactly one wasm call boundary,
// and wazero's own recover is what it lands in.
var errStepLimit = errors.New("wasmudf: step budget exhausted")

// errUnmetered means the fuel import was reached with no budget in the context.
// Unreachable: every context that can reach a guest (instantiation included)
// carries one. It traps rather than charging nothing, because the alternative to
// a loud impossible case is a guest running unmetered and nothing saying so.
var errUnmetered = errors.New("wasmudf: fuel called outside a metered call")

type fuelKey struct{}

// callState is one instance's step budget. It lives on the instance and is reset
// per call, rather than being allocated per call, because the fuel import finds
// it through the context and a fresh context value per call would allocate on a
// path that runs once per record.
type callState struct {
	steps uint64
}

func installFuel(ctx context.Context, rt wazero.Runtime) error {
	_, err := rt.NewHostModuleBuilder(importModule).
		NewFunctionBuilder().
		// WithGoModuleFunction rather than WithFunc: the reflection-based form
		// boxes arguments through reflect.Value on every call, and an
		// instrumented guest calls this once per basic block.
		WithGoModuleFunction(api.GoModuleFunc(chargeFuel),
			[]api.ValueType{api.ValueTypeI32}, nil).
		WithParameterNames("cost").
		Export(importFuel).
		Instantiate(ctx)
	return err
}

// chargeFuel debits the declared cost and traps when the budget is gone.
//
// The budget is reached through the context, which is where a per-CALL value has
// to live: one compiled host function serves every instance of every UDF in the
// runtime, wazero gives it no per-instance slot, and a map keyed by the calling
// module would put a lookup on the same hot path. The context the guest runs
// under has the budget as its outermost value, so Value hits on the first node.
//
// A cost of zero is charged as one: calling the import is itself a step, and
// without this a guest could declare its way out of the budget with fuel(0) in a
// loop and be as unmetered as a guest that never calls it.
func chargeFuel(ctx context.Context, _ api.Module, stack []uint64) {
	st, ok := ctx.Value(fuelKey{}).(*callState)
	if !ok || st == nil {
		panic(errUnmetered)
	}
	cost := stack[0] & 0xffffffff
	if cost == 0 {
		cost = 1
	}
	if cost > st.steps {
		st.steps = 0
		panic(errStepLimit)
	}
	st.steps -= cost
}

// instance is one guest module instance plus everything a call needs, resolved
// once. An instance is used by one call at a time: it has a single linear memory,
// so two concurrent calls would overwrite each other's call frames, and a guest
// is entitled to keep state in that memory between calls besides.
type instance struct {
	mod     api.Module
	call    api.Function
	version api.Function
	mem     api.Memory

	// base/size describe the host-owned region: pages added to the guest's memory
	// after instantiation, which nothing the guest already had can occupy.
	base, size uint32

	// ctx carries the step budget. Built once per instance; a call derives its
	// deadline from it.
	ctx  context.Context
	fuel callState

	// stack is the wasm call stack for CallWithStack, reused so a call allocates
	// neither its arguments nor its results.
	stack [3]uint64
}

// UDF is one loaded wasm function.
type UDF struct {
	name             string
	minArgs, maxArgs int

	compiled wazero.CompiledModule
	loader   *Loader
	opts     Options

	// free holds idle instances and tokens bounds how many exist. A caller takes
	// a token first (waiting if MaxInstances calls are already in flight) and
	// then either reuses an idle instance or instantiates one. Release returns
	// the instance BEFORE the token, so a waiter woken by the token always finds
	// something in free.
	free   chan *instance
	tokens chan struct{}

	calls     atomic.Uint64
	instances atomic.Uint64
	failures  [reasonCount]atomic.Uint64
}

// Name returns the name the UDF was loaded under.
func (u *UDF) Name() string { return u.name }

// Arity returns the accepted argument-count range, max < 0 meaning variadic.
func (u *UDF) Arity() (minArgs, maxArgs int) { return u.minArgs, u.maxArgs }

// Stats is a snapshot of one UDF's counters.
type Stats struct {
	Calls     uint64
	Failures  uint64
	Instances uint64
	ByReason  map[Reason]uint64
}

// Stats snapshots the counters. The fields are read independently, so a snapshot
// taken while calls are in flight can be internally inconsistent by a call or
// two; it is for operators and tests, not for accounting.
func (u *UDF) Stats() Stats {
	s := Stats{
		Calls:     u.calls.Load(),
		Instances: u.instances.Load(),
		ByReason:  map[Reason]uint64{},
	}
	for r := range reasonCount {
		if n := u.failures[r].Load(); n != 0 {
			s.ByReason[Reason(r)] = n
			s.Failures += n
		}
	}
	return s
}

// Func returns the UDF as the registry's function type: a pure function of its
// argument values, with no evaluator and no error return, because every failure
// it can have is already a ClassAd value.
func (u *UDF) Func() func(args []classad.Value) classad.Value { return u.Call }

// Call runs the UDF.
//
// It returns a ClassAd value, always, for every input and every way a guest can
// misbehave. The classification:
//
//	the guest wrote a value             that value, including its own UNDEFINED/ERROR
//	a list or nested-ad argument        ERROR, before the guest runs
//	arguments too large for the region  ERROR, before the guest runs
//	a trap                              ERROR, and the instance is discarded
//	the step budget                     ERROR, and the instance is discarded
//	the wall-clock deadline             ERROR, and the instance is discarded
//	a nonzero status                    ERROR
//	a result slot that is not a value   ERROR
//	a panic anywhere in this path       ERROR, and the instance is discarded
//
// No input makes it panic and no input makes it return an invalid value. That is
// not politeness: a UDF is called from inside expression evaluation, which a
// daemon runs with no top-level recover, so a panic here would be a daemon dying
// one match at a time -- and an invalid value would be a wrong answer, which is
// worse than an ERROR an operator can see.
func (u *UDF) Call(args []classad.Value) classad.Value {
	u.calls.Add(1)

	if len(args) < u.minArgs || (u.maxArgs >= 0 && len(args) > u.maxArgs) {
		// The registry checks arity before evaluating arguments, so this is
		// unreachable through it. Call is exported, so it is checked anyway.
		return u.fail(ReasonArity, fmt.Errorf("%d arguments, want %d..%d",
			len(args), u.minArgs, u.maxArgs))
	}

	in, err := u.acquire()
	if err != nil {
		return u.fail(ReasonInstance, err)
	}

	v, r, cerr := u.invoke(in, args)

	if r.fromGuest() {
		u.discard(in)
	} else {
		u.release(in)
	}
	if r != ReasonNone {
		return u.fail(r, cerr)
	}
	return v
}

// invoke is the body of one call against an instance the caller owns. It returns
// ReasonNone with the guest's value, or a reason and the error behind it.
func (u *UDF) invoke(in *instance, args []classad.Value) (v classad.Value, r Reason, cerr error) {
	defer func() {
		// recover() must be called DIRECTLY by the deferred function literal: a
		// helper that calls it leaves recover one frame too deep, where it
		// returns nil and stops nothing. (The same trap classad.RecoverCyclic
		// documents, which cost that package a release where every cyclic
		// expression killed the process.)
		if p := recover(); p != nil {
			v, r = classad.NewErrorValue(), ReasonPanic
			cerr = fmt.Errorf("panic in wasm call path: %v", p)
		}
	}()

	f, reason := writeArgs(in.mem, in.base, in.size, args)
	if reason != ReasonNone {
		// Nothing ran, so the instance is untouched and reusable; the caller
		// sees a reason that is not fromGuest and keeps it.
		return classad.NewErrorValue(), reason, nil
	}

	ctx, cancel := in.callContext(u.opts.Timeout)
	if cancel != nil {
		defer cancel()
	}
	in.fuel.steps = u.opts.Steps

	in.stack[0] = uint64(f.args)
	in.stack[1] = uint64(len(args))
	in.stack[2] = uint64(f.result)
	if err := in.call.CallWithStack(ctx, in.stack[:]); err != nil {
		return classad.NewErrorValue(), classifyCallError(err), err
	}
	if status := uint32(in.stack[0]); status != statusOK {
		return classad.NewErrorValue(), ReasonStatus,
			fmt.Errorf("guest returned status %d", status)
	}

	v, reason = readResult(in.mem, f.result, u.opts.MaxResultBytes)
	return v, reason, nil
}

// classifyCallError sorts the ways a guest call can end.
//
// The step limit is tested first because it arrives as an ordinary wrapped error
// from the host function, while the deadline arrives as a sys.ExitError from the
// runtime having closed the module -- so the two cannot be confused, and keeping
// them apart is what lets a test insist a module hit the deterministic limit
// rather than the clock.
func classifyCallError(err error) Reason {
	if errors.Is(err, errStepLimit) {
		return ReasonStepLimit
	}
	var exit *sys.ExitError
	if errors.As(err, &exit) {
		if exit.ExitCode() == sys.ExitCodeDeadlineExceeded {
			return ReasonTimeout
		}
		// Any other exit code is the guest having terminated itself. There is no
		// way to do that in this ABI except proc_exit, which the host does not
		// offer, so it is as much a trap as unreachable is.
		return ReasonTrap
	}
	return ReasonTrap
}

// callContext derives the per-call deadline from the instance's context, which is
// where the step budget lives. A negative timeout means the caller disabled the
// backstop and accepts that an uninstrumented runaway hangs this goroutine.
func (in *instance) callContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout < 0 {
		return in.ctx, nil
	}
	return context.WithTimeout(in.ctx, timeout)
}

func (u *UDF) fail(r Reason, err error) classad.Value {
	u.failures[r].Add(1)
	if u.opts.OnFailure != nil {
		u.opts.OnFailure(u.name, r, err)
	}
	return classad.NewErrorValue()
}

// acquire takes a token, then an idle instance or a new one.
func (u *UDF) acquire() (*instance, error) {
	<-u.tokens
	select {
	case in := <-u.free:
		return in, nil
	default:
	}
	in, err := u.newInstance(context.Background())
	if err != nil {
		u.tokens <- struct{}{}
		return nil, err
	}
	return in, nil
}

func (u *UDF) release(in *instance) {
	// free has capacity MaxInstances and tokens bounds how many instances exist,
	// so this never blocks.
	u.free <- in
	u.tokens <- struct{}{}
}

// discard closes an instance whose guest state can no longer be trusted and
// returns its token, so the next caller instantiates a fresh one.
func (u *UDF) discard(in *instance) {
	_ = in.mod.Close(context.Background())
	u.tokens <- struct{}{}
}

// newInstance instantiates the compiled module and reserves the host region in
// its memory.
func (u *UDF) newInstance(ctx context.Context) (*instance, error) {
	in := &instance{}

	// The budget is attached BEFORE instantiation because a wasm start section
	// runs during it, and an instrumented module's start section charges fuel
	// like anything else. Without this, module initialization would be the one
	// unmetered code path in the system.
	in.ctx = context.WithValue(ctx, fuelKey{}, &in.fuel)
	in.fuel.steps = u.opts.Steps

	cfg := wazero.NewModuleConfig().
		// Anonymous, so the same module can be instantiated many times in one
		// runtime namespace.
		WithName("").
		// No start functions. wazero's default calls an exported _start, which
		// is a WASI command's main; a UDF module is not a command, and running
		// main once per pooled instance is not something a caller asked for.
		// Initialization belongs in the wasm start section, which instantiation
		// runs regardless.
		WithStartFunctions()

	mod, err := u.loader.rt.InstantiateModule(in.ctx, u.compiled, cfg)
	if err != nil {
		return nil, err
	}

	in.mod = mod
	in.call = mod.ExportedFunction(exportCall)
	in.version = mod.ExportedFunction(exportABIVersion)
	in.mem = mod.ExportedMemory(exportMemory)
	if in.call == nil || in.version == nil || in.mem == nil {
		// checkExports already proved these exist in the compiled module.
		_ = mod.Close(ctx)
		return nil, errors.New("instance is missing an export the module declares")
	}

	pages := uint32((u.opts.RegionBytes + 65535) / 65536)
	prev, ok := in.mem.Grow(pages)
	if !ok {
		// The guest's declared maximum, or the runtime's memory limit, leaves no
		// room for the call frame. A load error if this is the probe instance,
		// an ERROR per call if a later instance somehow differs.
		_ = mod.Close(ctx)
		return nil, fmt.Errorf("cannot grow memory by %d pages for the %d-byte call region",
			pages, u.opts.RegionBytes)
	}
	in.base = prev * 65536
	in.size = pages * 65536
	if uint64(in.base)+uint64(in.size) > uint64(in.mem.Size()) {
		_ = mod.Close(ctx)
		return nil, errors.New("grown memory does not contain the call region")
	}

	u.instances.Add(1)
	return in, nil
}
