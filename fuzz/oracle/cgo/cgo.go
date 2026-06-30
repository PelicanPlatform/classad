// Package cgo wraps the reference C++ libclassad evaluation engine and exposes
// it as an in-process Go engine for differential fuzzing. The C++ work is done
// by shim.cc, which cgo compiles and links automatically.
//
// This avoids any subprocess/IPC overhead: a single Go process parses and
// evaluates the same ClassAd in both the native Go engine and libclassad,
// enabling high-throughput coverage-guided fuzzing.
//
// Crash isolation caveat: because libclassad runs in-process, a hard crash
// (segfault/abort) there takes the whole process down. The shim catches all
// C++ exceptions, but drivers should still journal the current input so a crash
// leaves a reproducer.
package cgo

/*
#cgo CXXFLAGS: -std=c++20 -I/usr/include
#cgo LDFLAGS: -lclassad -lstdc++
#include <stdlib.h>
#include "shim.h"
*/
import "C"

import (
	"sync"
	"time"
	"unsafe"

	"github.com/PelicanPlatform/classad/fuzz/canon"
)

// Name identifies this engine in divergence reports.
const Name = "cpp"

// cppMu serializes all calls into libclassad. libclassad is not thread-safe
// (it carries global/static evaluation state), so two concurrent evaluations
// corrupt each other. This matters because EvalAdTimeout abandons a goroutine
// that is stuck in a libclassad infinite loop (see below); without this lock
// that leaked goroutine would run concurrently with the next input and produce
// spurious divergences.
var cppMu sync.Mutex

// EvalAd parses src as a single ClassAd and evaluates every top-level
// attribute. It returns the canonical encoding of the resulting ad. parsed is
// false when libclassad rejects the input at parse time.
func EvalAd(src string) (encoded string, parsed bool) {
	cppMu.Lock()
	defer cppMu.Unlock()

	csrc := C.CString(src)
	defer C.free(unsafe.Pointer(csrc))

	var out *C.char
	rc := C.classad_eval_ad(csrc, &out)
	if rc == 0 {
		return "", false
	}
	defer C.classad_free(out)
	return C.GoString(out), true
}

// EvalAdTimeout is EvalAd with a wall-clock cap. libclassad can infinite-loop
// on some cyclic self-references reached through lazy operands (e.g.
// [A0 = 0 ? e : A0], where its cycle guard never fires) -- a libclassad bug.
// A cgo call cannot be interrupted, so the work runs in a separate goroutine;
// on timeout we abandon it (the goroutine leaks, spinning in the C++ loop while
// holding cppMu) and report timedOut. The Go engine detects these cycles and
// returns error, so such inputs are uncomparable rather than a Go bug; the
// differ treats a timeout as a non-divergence. Because the leaked goroutine
// keeps cppMu held, later evaluations in the same process block on the lock and
// also time out (rather than running concurrently and corrupting libclassad's
// global state) -- correctness is preserved at the cost of throughput after a
// hang, which a fresh process (a new fuzz worker) resets.
func EvalAdTimeout(src string, timeout time.Duration) (encoded string, parsed, timedOut bool) {
	type result struct {
		enc string
		ok  bool
	}
	ch := make(chan result, 1) // buffered so the goroutine can exit if we time out
	go func() {
		enc, ok := EvalAd(src)
		ch <- result{enc, ok}
	}()
	select {
	case r := <-ch:
		return r.enc, r.ok, false
	case <-time.After(timeout):
		return "", false, true
	}
}

// Eval parses src and returns the parsed canonical value tree.
func Eval(src string) (val canon.Value, parsed bool, err error) {
	encoded, ok := EvalAd(src)
	if !ok {
		return canon.Value{}, false, nil
	}
	v, err := canon.Parse(encoded)
	if err != nil {
		return canon.Value{}, true, err
	}
	return v, true, nil
}
