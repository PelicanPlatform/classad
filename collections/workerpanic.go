package collections

import (
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
)

// Panics on a scan's worker goroutines.
//
// A parallel scan evaluates the query on goroutines it starts, and a panic there cannot be recovered
// by anything the CALLER defers: recover only sees a panic on its own goroutine. So a bug reached
// through a parallel read -- an out-of-range column read, say -- took the whole process down even
// when the caller was a server prepared to fail just that request. workerGroup closes the gap: each
// worker recovers its own panic, the scan stops its other workers, and the goroutine that started the
// scan re-raises it once they have all returned. The caller then sees the panic exactly as it would
// from a serial scan, on its own goroutine, and whatever it defers handles it.

// WorkerPanic is a panic raised on one of a scan's worker goroutines, re-raised on the goroutine that
// started the scan. Value is what the worker panicked with; Stack is the worker's stack at the panic,
// which the re-raise would otherwise lose.
type WorkerPanic struct {
	Value any
	Stack []byte
}

func (p *WorkerPanic) Error() string { return fmt.Sprintf("panic in scan worker: %v", p.Value) }

// PanicStack returns the worker's stack. A method rather than only the field so a recovering caller can
// ask for it through an interface, without depending on this type.
func (p *WorkerPanic) PanicStack() []byte { return p.Stack }

// Unwrap returns the panic value when it is an error (a runtime error, typically).
func (p *WorkerPanic) Unwrap() error {
	if err, ok := p.Value.(error); ok {
		return err
	}
	return nil
}

// workerGroup runs a scan's workers and carries the first panic among them to the scan's goroutine.
type workerGroup struct {
	wg sync.WaitGroup
	// stop, when non-nil, is set on a worker panic so the other workers stop at their next check
	// rather than finish a scan whose answer is being thrown away.
	stop *atomic.Bool

	mu  sync.Mutex
	got *WorkerPanic
}

// Go runs fn on a new worker goroutine.
func (g *workerGroup) Go(fn func()) {
	g.wg.Add(1)
	liveScanWorkers.Add(1)
	go func() {
		defer g.wg.Done()
		defer liveScanWorkers.Add(-1)
		defer g.capture()
		if workerPanicHook != nil {
			workerPanicHook()
		}
		fn()
	}()
}

// capture records a worker's panic. It is deferred directly, not called from a deferred closure:
// recover stops a panic only when the deferred function itself calls it.
func (g *workerGroup) capture() {
	r := recover()
	if r == nil {
		return
	}
	stack := debug.Stack()
	g.mu.Lock()
	if g.got == nil {
		g.got = &WorkerPanic{Value: r, Stack: stack}
	}
	g.mu.Unlock()
	if g.stop != nil {
		g.stop.Store(true)
	}
}

// liveScanWorkers counts scan workers that have not returned, so a test can see that a scan which
// ends -- normally or by a panic -- leaves none behind still reading what it unpinned.
var liveScanWorkers atomic.Int64

// Wait waits for every worker to return.
func (g *workerGroup) Wait() { g.wg.Wait() }

// rethrow re-raises a captured worker panic on the calling goroutine. Call it only after every worker
// has returned, so nothing still reads what the caller's deferred cleanup releases.
func (g *workerGroup) rethrow() {
	g.mu.Lock()
	p := g.got
	g.mu.Unlock()
	if p != nil {
		panic(p)
	}
}

// workerPanicHook, when non-nil, runs at the top of every scan worker. Test-only: it injects a panic
// on a worker goroutine, which no input reaches on purpose.
var workerPanicHook func()

// stopAndDrain stops a scan's workers and waits for them, by draining their result channel to its
// close (which the scan closes after the workers' Wait). A parallel scan defers it so that a panic on
// the CONSUMING goroutine -- in the caller's yield -- still leaves no worker reading what the scan's
// deferred cleanup unpins. On the normal path the channel is already drained and closed, and this
// returns at once.
func stopAndDrain[T any](stopped *atomic.Bool, results <-chan T) {
	stopped.Store(true)
	for range results {
	}
}

// lockedReadHook, when non-nil, runs inside the read-path critical sections that decode stored data
// under a lock, with the site's name. Test-only: it injects a panic there to show the lock is
// released when a recovering caller fails the read.
var lockedReadHook func(site string)
