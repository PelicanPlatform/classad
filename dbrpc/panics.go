package dbrpc

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync/atomic"
)

// Containing a panic to the request that raised it.
//
// Every request runs on its own goroutine, and before this a panic on one -- a bug reached by one
// query's shape against one table's data -- killed the daemon and every other connection with it. A
// READ that panics is now failed alone: the client gets an internal error on that request, the panic
// and its stack go to the log at error level, RequestPanics counts it, and the connection and the
// server keep serving.
//
// That is safe only because a read leaves nothing behind for a later request to trip on. A read
// changes no stored state, and the read paths that decode stored data while holding a lock (merging a
// delta chain, proving a segment free of CurrentTime, sizing sidecars) release it by defer, so a panic
// part-way through does not leave a shard locked against every later request. What it can leave is a segment pin: a
// segment retired while that read held it is then not unmapped and unlinked until the process
// restarts. That costs disk and address space, never an answer or a hang.
//
// Writes are deliberately left to crash. Committing a transaction holds the store's snapshot lock
// and the shard locks across work that is not released by defer, and applies a write set across
// shards and indexes that a panic would leave half-applied; recovering would hand the next request a
// lock nobody will release or a partial commit. A restart recovers the store from what is durable, so
// for a write the crash is the safe outcome. The same goes for table and view lifecycle, maintenance
// and restore, which rewrite durable state.
//
// A read's parallel scan workers run on goroutines the read starts, which a recover here cannot see;
// the collections package carries a worker's panic back to the read's goroutine (collections.
// WorkerPanic), so it is contained like any other.

// requestPanics counts the requests failed by a contained panic, process-wide.
var requestPanics atomic.Int64

// RequestPanics reports how many requests this process has failed because serving them panicked. Each
// is a bug: the request failed instead of the daemon, and the log holds the panic and its stack.
func RequestPanics() int64 { return requestPanics.Load() }

// ErrInternal reports that the server failed a request because serving it panicked -- a server bug,
// not a problem with the request. It arrives as a *ServerError. The server keeps serving, but the
// same request will most likely fail the same way, so surface it rather than retry.
//
// It is a stErr with a tagged message, not a status of its own. A client that predates it treats
// any status but a row or an error as nothing in a stream and then reads the end of the stream as
// success, so a new status would hand those clients a silently short answer.
var ErrInternal = errors.New("dbrpc: internal server error")

// internalErrorPrefix opens the message of a contained panic, which the client maps to ErrInternal.
const internalErrorPrefix = "internal error: "

// recoverable reports whether a panic while serving o can be contained to the request: the reads.
// Anything not listed here writes durable or shared state and is left to crash (see above).
func (o op) recoverable() bool {
	switch o {
	case opQuery, opQueryAsOf, opQueryRaw, opQueryRawProj, opQueryRawProjSeq, opQueryRawProjRefs,
		opQueryRawProjRefsStats, opQueryRawWire, opQueryRawWireOpts, opQueryValues, opQueryKeys,
		opTopK, opTopKStats, opMatchSorted, opMatchTables, opMatchExplain, opOrdered,
		opAggregate, opAggregateFiltered, opAggregateBucketed,
		opArchiveQuery, opArchiveAggregate, opArchiveAggregateFiltered, opArchiveAggregateStats, opArchiveList,
		opWatch, opWatchWire, opWatchHead, opWatchStop, opSnapshot,
		opLookupAd, opLookupAttr, opTxnQuery, opTxnQueryKeys,
		opDiag, opExplain, opListTables, opListViews, opListExporters, opGetExporter, opGetExporterState,
		opQueryStats, opAttrTypes:
		return true
	}
	return false
}

// containPanic fails a read whose serving panicked and lets the server carry on. dispatch defers it
// for a recoverable op.
//
// It is deferred DIRECTLY and calls recover itself: recover stops a panic only when the deferred
// function calls it, so a helper doing the recover from inside a deferred closure would return nil
// and let the panic through.
func (sc *serverConn) containPanic(reqID uint64, o op) {
	r := recover()
	if r == nil {
		return
	}
	requestPanics.Add(1)
	attrs := []any{
		"op", o.String(), "req", reqID, "peer", sc.opts.Peer,
		"panic", fmt.Sprint(r), "stack", string(debug.Stack()),
	}
	// A scan worker's panic is re-raised on this goroutine, so the stack above ends at the re-raise.
	// The frames that actually failed are the worker's. Asked for by method, so this builds against
	// a collections that predates collections.WorkerPanic.
	if wp, ok := r.(interface{ PanicStack() []byte }); ok {
		attrs = append(attrs, "worker_stack", string(wp.PanicStack()))
	}
	sc.s.logger().Error("dbrpc: request panicked; failing the request and continuing", attrs...)
	// An error frame, then the end of the stream. A streaming client stops at the error; the end
	// frame releases its pending entry (and any drain of it), and a unary client never reads it.
	sc.write(respErr(reqID, fmt.Sprintf("%spanic serving %s: %v", internalErrorPrefix, o, r)))
	sc.write(respHead(reqID, stStreamEnd))
}

// logger is the server's logger: Server.Logger, or the process default.
func (s *Server) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// Test-only hooks, nil in production. dispatchHook runs at the top of dispatch with the request's op,
// and writeHook inside a connection's write, under its lock, with the frame about to go out; a test
// panics from one of them to stand in for a bug in a handler or the transport, before any frame or
// part-way through a stream.
var (
	dispatchHook atomic.Pointer[func(op)]
	writeHook    atomic.Pointer[func([]byte)]
)
