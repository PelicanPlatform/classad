package collections

import (
	"bytes"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// parallelPanicStore is an in-memory store large enough that every parallel read fans out.
func parallelPanicStore(t *testing.T) *Collection {
	t.Helper()
	c := New(Options{Shards: 4, SegmentSize: 1 << 12, QueryParallelism: 4})
	t.Cleanup(func() { c.Close() })
	c.parallelMinBytes = 0
	largeMatchCorpus(t, c, 2000)
	return c
}

// catchPanic runs fn and returns what it panicked with, or nil.
func catchPanic(fn func()) (r any) {
	defer func() { r = recover() }()
	fn()
	return nil
}

// parallelReads are the reads that fan out over worker goroutines, each returning how many rows it saw.
func parallelReads(t *testing.T, c *Collection) map[string]func() int {
	q, err := vm.Parse("Id >= 0")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]func() int{
		"Query": func() int {
			n := 0
			for range c.Query(q) {
				n++
			}
			return n
		},
		"ScanRawWire": func() int {
			n := 0
			for range c.ScanRawWire([]string{"Id"}, false) {
				n++
			}
			return n
		},
		"QueryRawWire": func() int {
			n := 0
			for range c.QueryRawWire(q, []string{"Id"}, false) {
				n++
			}
			return n
		},
		"Match": func() int {
			n := 0
			for range c.Match(bigMatchJob(t)) {
				n++
			}
			return n
		},
	}
}

// TestScanWorkerPanicReachesCaller: a panic on a parallel read's worker goroutine is re-raised on the
// goroutine that started the read -- where a recover can catch it -- with the worker's stack, once
// every worker has returned and the worker budget is given back. The store then serves the same read.
func TestScanWorkerPanicReachesCaller(t *testing.T) {
	c := parallelPanicStore(t)
	for name, read := range parallelReads(t, c) {
		t.Run(name, func(t *testing.T) {
			want := read()
			var fired atomic.Bool
			workerPanicHook = func() {
				if fired.CompareAndSwap(false, true) {
					panic(fmt.Errorf("injected worker panic"))
				}
			}
			r := catchPanic(func() { read() })
			workerPanicHook = nil
			if !fired.Load() {
				t.Fatal("the read never started a worker: it did not fan out")
			}
			wp, ok := r.(*WorkerPanic)
			if !ok {
				t.Fatalf("recovered %T (%v), want a *WorkerPanic", r, r)
			}
			if err := error(wp); err.Error() == "" || errors.Unwrap(err) == nil || errors.Unwrap(err).Error() != "injected worker panic" {
				t.Fatalf("WorkerPanic = %v, want the worker's error inside it", err)
			}
			if !bytes.Equal(wp.PanicStack(), wp.Stack) || !bytes.Contains(wp.Stack, []byte("workerpanic_test.go")) {
				t.Fatalf("worker stack does not show the panicking frame:\n%s", wp.Stack)
			}
			if n := liveScanWorkers.Load(); n != 0 {
				t.Fatalf("%d workers still running after the panic reached the caller", n)
			}
			if n := len(c.qsem); n != 0 {
				t.Fatalf("%d worker tokens still held after the panic", n)
			}
			if got := read(); got != want {
				t.Fatalf("after the panic the read returned %d rows, want %d", got, want)
			}
		})
	}
}

// TestScanYieldPanicStopsWorkers: a panic in the CALLER's yield, on the consuming goroutine, still
// stops and waits for the workers before the read's cleanup unpins what they read, and keeps the
// caller's panic value.
func TestScanYieldPanicStopsWorkers(t *testing.T) {
	c := parallelPanicStore(t)
	q, err := vm.Parse("Id >= 0")
	if err != nil {
		t.Fatal(err)
	}
	reads := map[string]func(){
		"Query": func() {
			for range c.Query(q) {
				panic("injected yield panic")
			}
		},
		"ScanRawWire": func() {
			for range c.ScanRawWire([]string{"Id"}, false) {
				panic("injected yield panic")
			}
		},
		"QueryRawWire": func() {
			for range c.QueryRawWire(q, []string{"Id"}, false) {
				panic("injected yield panic")
			}
		},
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			if r := catchPanic(read); r != "injected yield panic" {
				t.Fatalf("recovered %v, want the yield's own panic", r)
			}
			if n := liveScanWorkers.Load(); n != 0 {
				t.Fatalf("%d workers still running after the read returned", n)
			}
			if n := len(c.qsem); n != 0 {
				t.Fatalf("%d worker tokens still held", n)
			}
		})
	}
}

// TestLockedReadPanicReleasesLock: the read-path critical sections that decode stored data under a
// lock release it when the decode panics, so a server that fails that read alone does not leave the
// shard (or the segment's CurrentTime proof) locked for every later request.
func TestLockedReadPanicReleasesLock(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	if err := c.Put([]byte("k"), mustAd(t, "[ A = 1 ]")); err != nil {
		t.Fatal(err)
	}
	sh := c.shards[0]
	seg := sh.segs[0]
	cases := map[string]struct {
		run    func()
		locked func() bool
	}{
		"materialize": {
			run: func() { sh.materializeLocked(c, []byte("k"), 0, 0, nil) },
			locked: func() bool {
				if sh.mu.TryLock() {
					sh.mu.Unlock()
					return false
				}
				sh.mu.RUnlock() // release the leaked read lock, so Close does not hang on it
				return true
			},
		},
		"ctproof": {
			run: func() { c.proveSegCTLocked(seg, seg.used) },
			locked: func() bool {
				if seg.ct.mu.TryLock() {
					seg.ct.mu.Unlock()
					return false
				}
				seg.ct.mu.Unlock()
				return true
			},
		},
		"sidecarsizes": {
			run: func() { c.SidecarSizes() },
			locked: func() bool {
				if sh.mu.TryLock() {
					sh.mu.Unlock()
					return false
				}
				sh.mu.RUnlock() // release the leaked read lock, so Close does not hang on it
				return true
			},
		},
	}
	for site, tc := range cases {
		t.Run(site, func(t *testing.T) {
			lockedReadHook = func(s string) {
				if s == site {
					panic("injected: " + s)
				}
			}
			defer func() { lockedReadHook = nil }()
			if r := catchPanic(tc.run); r != "injected: "+site {
				t.Fatalf("recovered %v, want the injected panic (the hook did not run inside the section)", r)
			}
			if tc.locked() {
				t.Fatal("lock still held after the panic was recovered")
			}
		})
	}
}
