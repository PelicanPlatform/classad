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
	"iter"
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

// Starts a watch that signals notify_fd (a pipe/eventfd the C side created and polls
// in DaemonCore) whenever events are queued; drain them with cadb_watch_next. Returns
// an opaque watch handle, or 0 on error. The fd is owned by the caller (never closed
// here). Events start from now.
//
//export cadb_watch_start
func cadb_watch_start(h C.uintptr_t, notify_fd C.int) C.uintptr_t {
	d := cgo.Handle(h).Value().(*db.DB)
	w, err := db.NewWatcher(d, int(notify_fd), nil)
	if err != nil {
		return 0
	}
	return C.uintptr_t(cgo.NewHandle(w))
}

// Dequeues the next watch event without blocking. Returns 1 and fills *out_type (0
// upsert, 1 delete, 2 reset), *out_key, and *out_ad (a C string, or NULL for a
// delete/reset) -- both freed with cadb_free; 0 when the queue is empty (drain until
// then after a wakeup); the key is NULL when 0.
//
//export cadb_watch_next
func cadb_watch_next(h C.uintptr_t, outType *C.int, outKey **C.char, outAd **C.char) C.int {
	ev, ok := cgo.Handle(h).Value().(*db.Watcher).Next()
	if !ok {
		*outKey = nil
		*outAd = nil
		return 0
	}
	*outType = C.int(ev.Kind)
	*outKey = C.CString(ev.Key)
	if ev.Ad != nil {
		*outAd = C.CString(ev.Ad.String())
	} else {
		*outAd = nil
	}
	return 1
}

// Stops and frees a watch handle. The notify fd is not closed.
//
//export cadb_watch_stop
func cadb_watch_stop(h C.uintptr_t) {
	hd := cgo.Handle(h)
	hd.Value().(*db.Watcher).Stop()
	hd.Delete()
}

// queryIter holds a pull-style cursor over a Query's push iterator, so the C side can drain
// results one at a time (cadb_query_next) without materializing the whole result set. stop
// releases the underlying scan (pins/snapshot); it must be called exactly once via
// cadb_query_free.
type queryIter struct {
	next func() (*classad.ClassAd, bool)
	stop func()
}

// Query starts a scan of the store for ads matching a ClassAd constraint expression (the
// same text a query ad's Requirements holds, e.g. `Arch == "X86_64" && Memory >= 1024`).
// Returns an opaque query handle to drain with cadb_query_next, or 0 on a malformed
// constraint. The scan is a consistent snapshot taken at query time; the handle MUST be
// released with cadb_query_free.
//
//export cadb_query
func cadb_query(h C.uintptr_t, constraint *C.char) C.uintptr_t {
	d := cgo.Handle(h).Value().(*db.DB)
	seq, err := d.Query(C.GoString(constraint))
	if err != nil {
		return 0
	}
	next, stop := iter.Pull(seq)
	return C.uintptr_t(cgo.NewHandle(&queryIter{next: next, stop: stop}))
}

// QueryNext writes the next matching ad's old-ClassAd text (the `Attr = expr` wire form,
// symmetric with cadb_new_classad) to *out -- a C string the caller frees with cadb_free --
// and returns cadbOK. Returns cadbMissing when the query is exhausted (*out left NULL).
//
//export cadb_query_next
func cadb_query_next(qh C.uintptr_t, out **C.char) C.int {
	qi := cgo.Handle(qh).Value().(*queryIter)
	ad, ok := qi.next()
	if !ok {
		*out = nil
		return cadbMissing
	}
	*out = C.CString(ad.MarshalOld())
	return cadbOK
}

// Stops the scan behind a query handle and frees it. Safe to call before the query is
// fully drained (it abandons the rest).
//
//export cadb_query_free
func cadb_query_free(qh C.uintptr_t) {
	hd := cgo.Handle(qh)
	hd.Value().(*queryIter).stop()
	hd.Delete()
}

// Frees a string returned by the library (e.g. cadb_lookup_attr).
//
//export cadb_free
func cadb_free(p *C.char) { C.free(unsafe.Pointer(p)) }
