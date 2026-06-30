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
	"unsafe"

	"github.com/PelicanPlatform/classad/fuzz/canon"
)

// Name identifies this engine in divergence reports.
const Name = "cpp"

// EvalAd parses src as a single ClassAd and evaluates every top-level
// attribute. It returns the canonical encoding of the resulting ad. parsed is
// false when libclassad rejects the input at parse time.
func EvalAd(src string) (encoded string, parsed bool) {
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
