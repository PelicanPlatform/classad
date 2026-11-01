package wasmudf

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// Options configure a Loader. A zero Options is usable: every field falls back to
// the default named on it.
//
// These are per-LOADER, not per-UDF, and MemoryLimitPages could not be otherwise:
// wazero's memory cap is a property of the runtime, so a per-UDF limit would mean
// a runtime per UDF. The step and time limits could be per-UDF and are not yet;
// a caller needing two policies runs two Loaders, which costs one wasm runtime
// and shares nothing it should not.
type Options struct {
	// MemoryLimitPages caps each guest's linear memory, in 64 KiB pages. The cap
	// is hard: the guest's own memory.grow fails rather than the host noticing
	// afterwards. Default 128 pages (8 MiB) -- generous for a scalar function,
	// and a scalar function that needs more is doing something else.
	MemoryLimitPages uint32

	// Steps is the per-call budget charged through the classad_udf.fuel import.
	// Default 10,000,000, which a straight-line scalar function never approaches
	// and a runaway loop reaches in milliseconds. Deterministic; see the package
	// comment for why that is the point and what it does not cover.
	Steps uint64

	// Timeout is the wall-clock backstop, the only bound that applies to a guest
	// which never calls fuel. Default one second. It is deliberately far looser
	// than Steps: it is there to stop a hang, not to shape semantics, and every
	// millisecond of slack is a millisecond in which the reproducible limit gets
	// to fire first. Zero means the default.
	//
	// A NEGATIVE value disables it, and disables the runtime's interruption
	// support with it -- there being no deadline left to deliver. That is the
	// difference between ~800ns and ~170ns per call, and it leaves an
	// uninstrumented runaway able to hang its goroutine forever. It is right
	// only where every loaded module is known to terminate, or under an external
	// watchdog that can kill the process.
	Timeout time.Duration

	// RegionBytes is the host-owned window in each guest's memory holding the
	// call frame: the slots plus the string arena. Default 128 KiB. Rounded up to
	// whole pages, since that is the unit memory.grow works in.
	RegionBytes int

	// MaxResultBytes caps a string the guest returns. Default 64 KiB. A longer
	// result is ERROR rather than a truncation, because a truncated string is a
	// wrong answer and this is a value-producing API.
	MaxResultBytes int

	// MaxBatch is the largest vector batch a UDF with a vector entry point will
	// be handed, and so how much of its memory the host reserves for the call
	// frame. Default 4096, matching the row-group scale the engine scans at.
	//
	// A batch larger than this declines, which is correct (the engine falls back
	// to calling per record) and slow, so the counter for it exists -- see
	// ReasonBatchTooLarge.
	MaxBatch int

	// MaxBatchArgs sizes the vector call frame for a VARIADIC UDF, which has no
	// declared maximum arity to compute from. Default 4. A UDF with a declared
	// maximum ignores this and is sized from that instead, exactly.
	MaxBatchArgs int

	// MaxInstances bounds how many instances of one UDF exist at once, and so how
	// many calls to it run concurrently; a caller past the bound waits. Default
	// GOMAXPROCS, on the grounds that a UDF is CPU-bound and an instance beyond a
	// core buys queueing rather than throughput. Each instance costs its guest's
	// memory, so this times MemoryLimitPages is the worst case per UDF.
	MaxInstances int

	// OnFailure, if set, is called for every call that produced ERROR for a
	// reason other than the guest's own ERROR value, with the error wazero
	// reported where there was one.
	//
	// The counters in Stats say how often; only this says which trap. Without it
	// a UDF that fails on every record is a column of ERRORs and a number, which
	// is enough to know something is wrong and not enough to find it. It is
	// called on the calling goroutine, so an implementation that blocks blocks a
	// record.
	OnFailure func(name string, r Reason, err error)
}

func (o Options) withDefaults() Options {
	if o.MemoryLimitPages == 0 {
		o.MemoryLimitPages = 128
	}
	if o.Steps == 0 {
		o.Steps = 10_000_000
	}
	if o.Timeout == 0 {
		o.Timeout = time.Second
	}
	if o.RegionBytes <= 0 {
		o.RegionBytes = 128 * 1024
	}
	if o.MaxResultBytes <= 0 {
		o.MaxResultBytes = 64 * 1024
	}
	if o.MaxBatch <= 0 {
		o.MaxBatch = 4096
	}
	if o.MaxBatchArgs <= 0 {
		o.MaxBatchArgs = 4
	}
	if o.MaxInstances <= 0 {
		o.MaxInstances = runtime.GOMAXPROCS(0)
	}
	return o
}

// Loader owns a wasm runtime and the UDFs compiled into it.
//
// LIFETIME. One Loader per process is the intended shape. A module is compiled
// ONCE, by Load, and every later call instantiates from the compiled form -- a
// compile is milliseconds and machine-code-generating, so compiling per query
// would cost more than any query it could speed up, and compiling per record is
// not a thing that could work at all. The compiled code lives as long as the
// Loader; Close releases it along with every instance.
//
// CONCURRENCY. Safe for concurrent use, which it has to be: evaluation is
// concurrent, so one registered UDF is called from many goroutines at once.
// Load serializes against itself and against Close. Calls do not serialize
// against each other -- what they share is the compiled module, which wazero
// documents as safe to instantiate concurrently, and each gets an INSTANCE of its
// own from a pool. An instance is never shared by two calls in flight: it has one
// linear memory, so two concurrent calls would be writing each other's call
// frames, and its guest may keep state there besides.
type Loader struct {
	opts Options

	mu sync.Mutex
	// rts holds one runtime per interruption mode, created on demand and indexed
	// by whether a guest compiled into it can be stopped by the clock.
	//
	// It has to be two runtimes rather than one with a per-call choice, because
	// interruptibility is a property of the COMPILED CODE: wazero emits a
	// termination check at loop back-edges and call sites to make the stop
	// possible, so the machine code for a trusted module and an untrusted one
	// genuinely differs and the two cannot share a compilation. That is also why
	// the second runtime is lazy -- a process that loads no trusted module never
	// builds one.
	rts    [2]wazero.Runtime
	udfs   map[string]*UDF
	closed bool
}

// LoadOptions are the per-module decisions an ADMIN makes about a module, as
// distinct from the per-process ones in Options.
type LoadOptions struct {
	// Trusted runs the module without the wall-clock backstop and on wazero's
	// non-interruptible code path: ~15ns per call against ~350ns, which is the
	// whole reason the label exists (BenchmarkRawGuestInterruptible).
	//
	// It does NOT relax the deterministic step budget, the memory cap, the result
	// validation, the poisoned result slot, or discarding an instance that
	// trapped. Those are not the expensive part -- the measurement is unambiguous
	// that the cost is wazero's interruptibility and the per-call deadline
	// context, not the budget, which costs nothing at all for a guest that
	// declares no steps. And the budget is the only bound whose failures are
	// reproducible, so dropping it would mean a trusted module and an untrusted
	// one could not be run against each other on the same input and compared.
	//
	// What it does give up is liveness: a trusted module that loops forever hangs
	// its goroutine, with nothing able to stop it. That is the trade, and it is
	// why this is the admin's decision and not the module's.
	//
	// TRUST IS NOT SOMETHING A MODULE CAN CLAIM. This field is the only input.
	// Nothing in the wasm -- an export, a custom section, a name, a start
	// section -- is consulted, and there is deliberately no ABI through which a
	// guest could ask. A module arrives trusted because configuration said so,
	// which is the only party in a position to know.
	Trusted bool
}

// NewLoader creates a Loader. Its wasm runtimes are built on demand by Load.
func NewLoader(ctx context.Context, opts Options) (*Loader, error) {
	l := &Loader{opts: opts.withDefaults(), udfs: map[string]*UDF{}}
	// The runtime for the default mode is built eagerly so that a configuration
	// error -- an impossible memory limit, say -- is reported by NewLoader rather
	// than by whichever Load happens to be first.
	if _, err := l.runtime(ctx, false); err != nil {
		return nil, err
	}
	return l, nil
}

// runtime returns the runtime for one interruption mode, building it on first
// use. The caller holds l.mu, except in NewLoader where nothing else can see it.
func (l *Loader) runtime(ctx context.Context, trusted bool) (wazero.Runtime, error) {
	// A trusted module is not interruptible. An untrusted one is, unless the
	// caller disabled the backstop for every module in the process, in which case
	// there is no deadline left for the machinery to deliver and charging for it
	// would be charging for nothing.
	interruptible := !trusted && l.opts.Timeout >= 0

	idx := 0
	if !interruptible {
		idx = 1
	}
	if rt := l.rts[idx]; rt != nil {
		return rt, nil
	}

	cfg := wazero.NewRuntimeConfig().
		WithMemoryLimitPages(l.opts.MemoryLimitPages).
		// Without this the runtime cannot interrupt a guest at all, and a bare
		// infinite loop would hold its goroutine until the process died.
		//
		// It is expensive, and not in the place one would guess: the measured
		// cost is ~340ns per CALL on an Apple M2 (see
		// BenchmarkRawGuestInterruptible, where the same guest costs 15ns
		// without it), which is more than everything else this package does per
		// call put together.
		WithCloseOnContextDone(interruptible).
		// Pinned rather than left at the runtime's default so that a wazero
		// upgrade enabling a new proposal does not silently widen what a guest
		// can do. Threads in particular would give two instances a shared memory
		// and take the one-call-per-instance invariant with them.
		WithCoreFeatures(api.CoreFeaturesV2)

	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	if err := installFuel(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, err
	}
	l.rts[idx] = rt
	return rt, nil
}

// Load compiles wasm and returns the UDF it defines, accepting minArgs..maxArgs
// arguments (maxArgs < 0 meaning variadic), under the name it will be registered
// as.
//
// Everything checkable is checked HERE, because the alternative is one ERROR per
// record with no message anywhere: a missing or mistyped export, an ABI version
// this host does not speak, an import the host does not offer, a memory that
// cannot hold the call frame, and a start section that traps. Load instantiates
// the module once to do it, and keeps that instance as the first pooled one, so
// the checks cost nothing extra.
//
// A returned error means nothing was loaded and nothing registered.
func (l *Loader) Load(ctx context.Context, name string, minArgs, maxArgs int, wasm []byte) (*UDF, error) {
	return l.LoadWith(ctx, name, minArgs, maxArgs, wasm, LoadOptions{})
}

// LoadWith is Load with the per-module options an admin sets. See LoadOptions.
func (l *Loader) LoadWith(ctx context.Context, name string, minArgs, maxArgs int, wasm []byte, lo LoadOptions) (*UDF, error) {
	if name == "" {
		return nil, errors.New("wasmudf: empty function name")
	}
	if minArgs < 0 || (maxArgs >= 0 && maxArgs < minArgs) {
		return nil, fmt.Errorf("wasmudf: %s: bad arity range %d..%d", name, minArgs, maxArgs)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, errors.New("wasmudf: loader is closed")
	}
	if _, dup := l.udfs[name]; dup {
		return nil, fmt.Errorf("wasmudf: %s is already loaded", name)
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
	if cerr := checkExports(compiled); cerr != nil {
		_ = compiled.Close(ctx)
		return nil, fmt.Errorf("wasmudf: %s: %w", name, cerr)
	}

	u := &UDF{
		name:     name,
		minArgs:  minArgs,
		maxArgs:  maxArgs,
		compiled: compiled,
		loader:   l,
		rt:       rt,
		opts:     l.opts,
		trusted:  lo.Trusted,
		vectored: hasVectorEntry(compiled),
		free:     make(chan *instance, l.opts.MaxInstances),
		tokens:   make(chan struct{}, l.opts.MaxInstances),
	}
	// A trusted module runs with no deadline, so no deadline context is derived
	// for it and nothing is charged for the interruption support it does not have.
	u.timeout = l.opts.Timeout
	if lo.Trusted {
		u.timeout = -1
	}
	// The region has to hold the largest vector frame this UDF can be handed, or
	// the vector path turns itself off one batch at a time and the only symptom is
	// that it is slow. Sized from the declared arity where there is one; a
	// variadic UDF has no bound to compute from, so MaxBatchArgs supplies it.
	if u.vectored {
		argc := maxArgs
		if argc < 0 {
			argc = l.opts.MaxBatchArgs
		}
		u.region = max(l.opts.RegionBytes, vectorRegionBytes(l.opts.MaxBatch, argc))
	} else {
		u.region = l.opts.RegionBytes
	}
	for range l.opts.MaxInstances {
		u.tokens <- struct{}{}
	}

	// The probe instance is the ABI version check (it takes a call, so it takes
	// an instance) and the memory-grow check in one, and it is then handed to the
	// pool rather than closed -- a load that proved the module works should not
	// throw away the instance that proved it.
	//
	// Its token is taken first, like any other instance's. Creating one outside
	// the semaphore and then releasing it into the pool would put MaxInstances+1
	// tokens back, and returning the extra one blocks forever on a full channel:
	// a deadlock in Load, reached by a module that loaded perfectly.
	<-u.tokens
	in, err := u.newInstance(ctx)
	if err != nil {
		u.tokens <- struct{}{}
		_ = compiled.Close(ctx)
		return nil, fmt.Errorf("wasmudf: %s: %w", name, err)
	}
	ver, verr := checkVersion(in)
	if verr != nil {
		u.discard(in)
		_ = compiled.Close(ctx)
		return nil, fmt.Errorf("wasmudf: %s: %w", name, verr)
	}
	u.abi = ver
	u.release(in)

	l.udfs[name] = u
	return u, nil
}

// Lookup returns a loaded UDF by name.
func (l *Loader) Lookup(name string) (*UDF, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	u, ok := l.udfs[name]
	return u, ok
}

// Close releases the runtime, every compiled module and every instance. Calls in
// flight fail; calls after it return ERROR with ReasonInstance rather than
// panicking, since a registered UDF outlives the Loader in the registry (the
// registry is append-only, so there is no way to withdraw it) and a closed Loader
// must therefore stay callable without crashing.
func (l *Loader) Close(ctx context.Context) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	rts := l.rts
	l.mu.Unlock()

	// Both runtimes, and the first error rather than the last: a Close that
	// reported only the second would hide a failure to release the first.
	var firstErr error
	for _, rt := range rts {
		if rt == nil {
			continue
		}
		if err := rt.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// checkImports refuses a module wanting anything the host does not offer.
//
// Instantiation would refuse it too, but with a message naming the missing MODULE
// ("module[wasi_snapshot_preview1] not instantiated") and not what was wanted
// from it, and only for the first one. This names every offending import, which
// is the difference between "this needs WASI" and knowing it wanted fd_write --
// the second tells the author of the module which line to delete.
func checkImports(c wazero.CompiledModule) error {
	var bad []string
	for _, imp := range c.ImportedFunctions() {
		mod, fn, _ := imp.Import()
		if mod == importModule && fn == importFuel {
			continue
		}
		bad = append(bad, mod+"."+fn)
	}
	for _, imp := range c.ImportedMemories() {
		mod, fn, _ := imp.Import()
		bad = append(bad, mod+"."+fn+" (memory)")
	}
	// An imported global or table is not enumerable through CompiledModule, so
	// those are caught by instantiation instead -- which still makes them a LOAD
	// error, because Load instantiates the module before it returns.
	if len(bad) == 0 {
		return nil
	}
	slices.Sort(bad)
	return fmt.Errorf("the host offers only %s.%s, but this module imports %v",
		importModule, importFuel, bad)
}

// checkExports verifies the entry points and their signatures before any
// instance exists. A signature mismatch found at call time would be one ERROR per
// record with nothing naming the cause, and the signature is in the compiled
// module, so there is no reason to wait.
func checkExports(c wazero.CompiledModule) error {
	if _, ok := c.ExportedMemories()[exportMemory]; !ok {
		return fmt.Errorf("no exported memory %q: there is nowhere to pass a value", exportMemory)
	}
	fns := c.ExportedFunctions()

	ver, ok := fns[exportABIVersion]
	if !ok {
		return fmt.Errorf("no exported function %q", exportABIVersion)
	}
	if len(ver.ParamTypes()) != 0 || len(ver.ResultTypes()) != 1 ||
		ver.ResultTypes()[0] != api.ValueTypeI32 {
		return fmt.Errorf("%s must be (func (result i32))", exportABIVersion)
	}

	call, ok := fns[exportCall]
	if !ok {
		return fmt.Errorf("no exported function %q", exportCall)
	}
	if err := checkSignature(exportCall, call, 3); err != nil {
		return err
	}

	// The vector entry point is OPTIONAL -- a module without one is simply called
	// per record -- but the scalar one is not, and that is what makes the escape
	// hatch safe by construction: a vectorized guest can always decline a record,
	// because the path those records go to is a function every module must export.
	// There is no module shape in which a decline has nowhere to go.
	if vec, ok := fns[exportCallVec]; ok {
		if err := checkSignature(exportCallVec, vec, 5); err != nil {
			return err
		}
	}
	return nil
}

// checkSignature requires (func (param i32 x nparams) (result i32)).
func checkSignature(name string, fn api.FunctionDefinition, nparams int) error {
	want := make([]api.ValueType, nparams)
	for i := range want {
		want[i] = api.ValueTypeI32
	}
	if !slices.Equal(fn.ParamTypes(), want) ||
		len(fn.ResultTypes()) != 1 || fn.ResultTypes()[0] != api.ValueTypeI32 {
		return fmt.Errorf("%s must be (func (param%s) (result i32)), got params %v results %v",
			name, strings.Repeat(" i32", nparams), fn.ParamTypes(), fn.ResultTypes())
	}
	return nil
}

// hasVectorEntry reports whether the module exports the batch entry point. It is
// read from the COMPILED MODULE and nowhere else -- not from a name, a custom
// section or anything else a module author could write -- for the same reason
// trust is: what a guest says about itself is not a capability.
func hasVectorEntry(c wazero.CompiledModule) bool {
	_, ok := c.ExportedFunctions()[exportCallVec]
	return ok
}

// checkVersion calls the guest's version export and returns the version it
// reports. It is the one load check that needs the guest to run, which is also
// why it doubles as the proof that the module can be instantiated and called at
// all.
//
// The version it returns is not merely validated, it is KEPT: it decides which
// descriptor forms the guest is handed for the rest of its life. A host that
// checked the number and threw it away would have to guess per batch, and the
// guess that is wrong -- a version 1 guest handed a columnar column -- reads a
// state byte array where there is one state byte, in bounds, and returns numbers.
func checkVersion(in *instance) (uint32, error) {
	res, err := in.version.Call(in.ctx)
	if err != nil {
		return 0, fmt.Errorf("calling %s: %w", exportABIVersion, err)
	}
	if len(res) != 1 {
		return 0, fmt.Errorf("%s returned %d values", exportABIVersion, len(res))
	}
	got := uint32(res[0])
	if got < abiVersionMin || got > abiVersionMax {
		return 0, fmt.Errorf("module speaks ABI version %d, host speaks %d..%d",
			got, abiVersionMin, abiVersionMax)
	}
	return got, nil
}
