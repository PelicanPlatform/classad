package dbrpc

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/db"
)

// An in-process reader: the same catalog, the same authorization decision, and none of the
// serialization.
//
// A Flight read in this process reaches storage through a dbrpc LOOPBACK -- a net.Pipe with a
// server on the far end -- so that it is authorized exactly as a remote client is, with no second
// policy to keep in sync. That is the right reason, and it does not require the DATA to make the
// trip: a read encodes its columns into a frame, copies them through the pipe, and decodes them
// back into the columns it started as, which measured at about a third of the read.
//
// What a read is actually authorized by is one bit. ServeOptions.ReadOnly and .Privileged gate
// writes and admin ops; a read is governed by IncludePrivate alone, decided once when the session
// is opened. The two places it is consulted -- forcing redaction, and refusing a constraint that
// reads a private attribute -- are called from here, not reimplemented.
//
// What the connection loop does BESIDES authorization is the part worth being careful about, and
// is kept: the shutdown barrier, so Close cannot race a read that is touching the catalog; panic
// containment, so a scan that panics fails its own read instead of the daemon; and the query log.

// LocalSession is one in-process reader, scoped by the ServeOptions its session was opened with.
//
// PROTOTYPE. The batches it yields are the producer's own and are only valid until the callback
// returns -- see collections.Batch -- which is the same contract the Arrow encoder already works
// to, since it copies into its builders.
type LocalSession struct {
	s    *Server
	opts ServeOptions
	qlog func(QueryLog)
}

// LocalSession opens an in-process reader under opts, which the caller has already derived from
// its own authorization exactly as it would for ServeConnOpts.
func (s *Server) LocalSession(opts ServeOptions) *LocalSession {
	return &LocalSession{s: s, opts: opts, qlog: s.queryLogger(opts)}
}

// ErrLocalUnsupported says this table cannot serve a column scan, so the caller falls back to the
// framed path rather than to a different answer.
//
// It is a FALL-THROUGH, not a failure: the column source is an accelerator, and a table whose rows
// are not in columnar form -- an in-memory table, or one whose segments have not been rewritten yet
// -- is read perfectly well by the record scan. The framed op makes the same decision before it has
// written anything, which is what lets it fall through cleanly; this returns before yielding a
// batch for the same reason.
var ErrLocalUnsupported = errors.New("dbrpc: table cannot serve an in-process column scan")

// ScanColumns reads the table as columns, handing each batch to yield. Returning false stops it.
//
// redact is the caller's own request; an unprivileged session redacts whatever it asks, exactly as
// over the wire.
func (ls *LocalSession) ScanColumns(ctx context.Context, table, constraint string, attrs []string,
	redact bool, yield func(*collections.Batch) bool) (err error) {
	if len(attrs) == 0 {
		return errors.New("dbrpc: a column scan needs at least one attribute")
	}
	// The shutdown barrier, as every framed request joins it: once Close has begun this read is
	// not served at all, and until it ends Close waits for it.
	if !ls.s.beginRequest() {
		return ErrServerClosed
	}
	defer ls.s.endRequest()

	// Deferred DIRECTLY and calling recover itself, for the reason containPanic documents: a
	// helper that recovers from inside a deferred closure returns nil and lets the panic through.
	// A scan that panics must fail this read, not the daemon.
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		requestPanics.Add(1)
		attrs := []any{"op", "LocalScanColumns", "peer", ls.opts.Peer,
			"panic", fmt.Sprint(r), "stack", string(debug.Stack())}
		if wp, ok := r.(interface{ PanicStack() []byte }); ok {
			attrs = append(attrs, "worker_stack", string(wp.PanicStack()))
		}
		ls.s.logger().Error("dbrpc: in-process read panicked; failing the read and continuing", attrs...)
		err = fmt.Errorf("%spanic serving a local column scan: %v", internalErrorPrefix, r)
	}()

	// THE AUTHORIZATION, both halves, called rather than copied.
	redact = redact || !ls.opts.IncludePrivate
	if gerr := privateConstraintError(constraint, ls.opts.IncludePrivate); gerr != nil {
		return gerr
	}

	start := time.Now()
	ref := ls.s.resolveTable(table)
	if ref.seals() {
		// A continuous aggregate holds half its rows in a backing table and half in an archive,
		// and a column scan reads one source. It declines here as the wire op declines.
		return ErrLocalUnsupported
	}
	var colStats collections.ColumnScanStats
	var seq func(func(*collections.Batch) bool)
	var qerr error
	switch ref.kind {
	case kindTable, kindView:
		seq, qerr = ref.db.QueryColumnsStats(constraint, attrs, redact, &colStats)
	case kindArchive:
		seq, qerr = ref.arch.QueryColumnsStats(constraint, attrs, redact, &colStats)
	default:
		return fmt.Errorf("no such table: %s", table)
	}
	if qerr != nil {
		if errors.Is(qerr, db.ErrColumnsUnsupported) {
			return ErrLocalUnsupported // fall through to the framed path; see above
		}
		return qerr
	}
	n := 0
	for b := range seq {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n += b.N
		if !yield(b) {
			break
		}
	}
	if ls.qlog != nil {
		ls.qlog(QueryLog{Op: "LocalScanColumns", Table: table, Constraint: constraint,
			Rows: n, Projection: len(attrs), Duration: time.Since(start), Columns: colStats})
	}
	return nil
}
