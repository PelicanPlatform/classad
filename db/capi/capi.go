// Package main builds the embedded ClassAd log as a C archive: it exports C symbols
// (cadb_*) mirroring HTCondor's classad_log.h, so a C++ interface (built on
// libcondor_utils) can sit on top. Build:
//
//	go build -buildmode=c-archive -o libclassad_db.a ./capi
//
// which also emits capi.h with these signatures.
//
// Handles: a DB and a transaction are passed to C as opaque cgo.Handle values
// (uintptr_t). C never dereferences them; it only passes them back. Returned strings
// are C-allocated and must be released with cadb_free.
//
// This is the minimal, correct surface; the wire-bytes zero-alloc PutClassAd path
// (cadb_new_classad_wire) is a planned addition once the collections store exposes a
// raw-wire ingest (see DESIGN.md).
package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"runtime/cgo"
	"unsafe"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

func main() {}

// result codes shared with the C side (see classad_db.h).
const (
	cadbOK      = 0
	cadbErr     = -1
	cadbMissing = -2
)

// Opens a log at dir (empty/NULL = in-memory). Returns an opaque handle, or 0 on error.
//
//export cadb_open
func cadb_open(dir *C.char) C.uintptr_t {
	d, err := db.Open(C.GoString(dir))
	if err != nil {
		return 0
	}
	return C.uintptr_t(cgo.NewHandle(d))
}

// Closes and frees the log handle.
//
//export cadb_close
func cadb_close(h C.uintptr_t) {
	hd := cgo.Handle(h)
	_ = hd.Value().(*db.DB).Close()
	hd.Delete()
}

// Begins an independent transaction on the log; returns an opaque transaction handle.
//
//export cadb_begin
func cadb_begin(h C.uintptr_t) C.uintptr_t {
	d := cgo.Handle(h).Value().(*db.DB)
	return C.uintptr_t(cgo.NewHandle(d.Begin()))
}

// Commits and frees the transaction. Returns the number of conflicted keys (0 =
// full success), or cadbErr.
//
//export cadb_commit
func cadb_commit(h C.uintptr_t) C.int {
	hd := cgo.Handle(h)
	tx := hd.Value().(*db.Txn)
	defer hd.Delete()
	if err := tx.Commit(); err != nil {
		if ce, ok := err.(*db.ConflictError); ok {
			return C.int(len(ce.Keys))
		}
		return cadbErr
	}
	return cadbOK
}

// Aborts and frees the transaction (discards its buffered operations).
//
//export cadb_abort
func cadb_abort(h C.uintptr_t) {
	hd := cgo.Handle(h)
	hd.Value().(*db.Txn).Abort()
	hd.Delete()
}

// NewClassAd: stores the ad parsed from ad_text (old-ClassAd, newline-separated
// "Attr = expr") under key. Returns cadbOK or cadbErr (parse failure).
//
//export cadb_new_classad
func cadb_new_classad(h C.uintptr_t, key, adText *C.char) C.int {
	ad, err := classad.ParseOld(C.GoString(adText))
	if err != nil {
		return cadbErr
	}
	cgo.Handle(h).Value().(*db.Txn).NewClassAd(C.GoString(key), ad)
	return cadbOK
}

// DestroyClassAd: removes key.
//
//export cadb_destroy_classad
func cadb_destroy_classad(h C.uintptr_t, key *C.char) {
	cgo.Handle(h).Value().(*db.Txn).DestroyClassAd(C.GoString(key))
}

// SetAttribute: sets key's attribute name to the expression parsed from expr.
// Returns cadbOK or cadbErr (parse failure).
//
//export cadb_set_attribute
func cadb_set_attribute(h C.uintptr_t, key, name, expr *C.char) C.int {
	if err := cgo.Handle(h).Value().(*db.Txn).SetAttribute(
		C.GoString(key), C.GoString(name), C.GoString(expr)); err != nil {
		return cadbErr
	}
	return cadbOK
}

// DeleteAttribute: removes key's attribute name.
//
//export cadb_delete_attribute
func cadb_delete_attribute(h C.uintptr_t, key, name *C.char) {
	cgo.Handle(h).Value().(*db.Txn).DeleteAttribute(C.GoString(key), C.GoString(name))
}

// LookupInTransaction: writes the unparsed expression of key's attribute name (as the
// transaction sees it) to *out as a C string the caller frees with cadb_free.
// Returns cadbOK, or cadbMissing if absent (*out left NULL).
//
//export cadb_lookup_attr
func cadb_lookup_attr(h C.uintptr_t, key, name *C.char, out **C.char) C.int {
	v, ok := cgo.Handle(h).Value().(*db.Txn).LookupAttr(C.GoString(key), C.GoString(name))
	if !ok {
		*out = nil
		return cadbMissing
	}
	*out = C.CString(v)
	return cadbOK
}

// Frees a string returned by the library (e.g. cadb_lookup_attr).
//
//export cadb_free
func cadb_free(p *C.char) { C.free(unsafe.Pointer(p)) }
