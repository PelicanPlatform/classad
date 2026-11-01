package wasmudf

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
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
	rt   wazero.Runtime
	opts Options

	mu     sync.Mutex
	udfs   map[string]*UDF
	closed bool
}

// NewLoader creates a Loader and its wasm runtime.
func NewLoader(ctx context.Context, opts Options) (*Loader, error) {
	opts = opts.withDefaults()

	cfg := wazero.NewRuntimeConfig().
		WithMemoryLimitPages(opts.MemoryLimitPages).
		// Without this the runtime cannot interrupt a guest at all, and a bare
		// infinite loop would hold its goroutine until the process died.
		//
		// It is expensive, and not in the place one would guess: the measured
		// cost is ~340ns per CALL on an Apple M2 (see
		// BenchmarkRawGuestInterruptible, where the same guest costs 15ns
		// without it), which is more than everything else this package does per
		// call put together. So it is enabled exactly when there is a deadline
		// for it to enforce -- a caller who has disabled the backstop is not
		// also charged for the machinery that would have delivered it.
		WithCloseOnContextDone(opts.Timeout >= 0).
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
	return &Loader{rt: rt, opts: opts, udfs: map[string]*UDF{}}, nil
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

	compiled, err := l.rt.CompileModule(ctx, wasm)
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
		opts:     l.opts,
		free:     make(chan *instance, l.opts.MaxInstances),
		tokens:   make(chan struct{}, l.opts.MaxInstances),
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
	if verr := checkVersion(in); verr != nil {
		u.discard(in)
		_ = compiled.Close(ctx)
		return nil, fmt.Errorf("wasmudf: %s: %w", name, verr)
	}
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
	l.mu.Unlock()
	return l.rt.Close(ctx)
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
	wantParams := []api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}
	if !slices.Equal(call.ParamTypes(), wantParams) ||
		len(call.ResultTypes()) != 1 || call.ResultTypes()[0] != api.ValueTypeI32 {
		return fmt.Errorf("%s must be (func (param i32 i32 i32) (result i32)), got params %v results %v",
			exportCall, call.ParamTypes(), call.ResultTypes())
	}
	return nil
}

// checkVersion calls the guest's version export. It is the one load check that
// needs the guest to run, which is also why it doubles as the proof that the
// module can be instantiated and called at all.
func checkVersion(in *instance) error {
	res, err := in.version.Call(in.ctx)
	if err != nil {
		return fmt.Errorf("calling %s: %w", exportABIVersion, err)
	}
	if len(res) != 1 {
		return fmt.Errorf("%s returned %d values", exportABIVersion, len(res))
	}
	if got := uint32(res[0]); got != abiVersion {
		return fmt.Errorf("module speaks ABI version %d, host speaks %d", got, abiVersion)
	}
	return nil
}
