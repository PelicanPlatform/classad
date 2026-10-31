package dbrpc

import (
	"errors"
	"strings"
)

// The error taxonomy a caller needs to drive reconnection policy. A dbrpc call
// fails in one of a few distinct ways, and the right response differs for each:
//
//   - *db.ConflictError (from Commit): an optimistic write-write conflict. The
//     connection is healthy; retry the transaction on the SAME connection.
//   - ErrConnClosed: the transport failed or the Client was closed. Every in-flight
//     and future call fails with it (wrapping the underlying cause). Redial, and --
//     if the unit of work is idempotent -- replay it on the fresh connection.
//   - *ServerError: the server rejected the request (bad constraint, unknown table,
//     malformed op). Deterministic; a replay fails identically, so surface it.
//   - ErrTableReadOnly (a *ServerError): the table belongs to another writer on the
//     server, so this connection may read it but not modify it. Deterministic; do not
//     retry.
//   - ErrInternal (a *ServerError): serving the request panicked -- a server bug. The
//     server failed only that request and keeps serving; a replay most likely fails
//     the same way, so surface it.
//   - context.Canceled / context.DeadlineExceeded: the caller's context ended while
//     waiting. Surface it; do not retry against the caller's wishes.
//
// Callers classify with errors.Is / errors.As rather than matching message text.

// ErrConnClosed reports that the dbrpc connection is no longer usable. It wraps the
// underlying transport cause, so errors.Is(err, ErrConnClosed) identifies the class
// while errors.Unwrap reaches the specific I/O error.
var ErrConnClosed = errors.New("dbrpc: connection closed")

// ErrServerClosed is returned by ServeConn/ServeConnOpts when the Server has been closed:
// either the connection was handed to a server that was already closing, or a frame
// arrived on it after Close began. The server takes no new work once Close starts, so the
// requests it waits for cannot grow while it waits. Not retryable on this Server.
var ErrServerClosed = errors.New("dbrpc: server closed")

// ServerError is a logical error the server returned for a request (wire status
// stErr): a bad constraint, an unknown table, a malformed op. It is deterministic
// -- replaying the request fails the same way -- so callers should surface it
// rather than retry.
type ServerError struct{ Msg string }

func (e *ServerError) Error() string { return "dbrpc: " + e.Msg }

// Is matches the prefix-tagged refusals -- a per-table write refusal and an
// op-does-not-serve-an-archive refusal -- so errors.Is tells each apart from other server
// errors while errors.As still yields the *ServerError.
func (e *ServerError) Is(target error) bool {
	switch target {
	case ErrTableReadOnly:
		return strings.HasPrefix(e.Msg, readOnlyTablePrefix)
	case ErrArchiveTable:
		return strings.HasPrefix(e.Msg, archiveTablePrefix)
	case ErrCursorExpired:
		return strings.HasPrefix(e.Msg, cursorExpiredPrefix)
	case ErrInternal:
		return strings.HasPrefix(e.Msg, internalErrorPrefix)
	}
	return false
}

// ErrTableReadOnly reports that the server refused to modify a table this connection may
// only read (ServeOptions.TableWritable) -- typically one an in-process writer owns, such
// as a mirror of another store. It arrives as a *ServerError and, like every ServerError,
// is deterministic: replaying the write fails the same way, so surface it, never retry.
var ErrTableReadOnly = errors.New("dbrpc: table is read-only")

// ErrArchiveTable reports that the named table exists and is an append-only archive, and
// that the op the caller used does not serve one. It arrives as a *ServerError whose
// message names what to use instead, or why nothing applies. Deterministic: the call fails
// the same way on replay, so surface it rather than retry -- and do not read it as the
// table being absent, which is what "no such table" means and this does not.
var ErrArchiveTable = errors.New("dbrpc: table is an append-only archive")

// ErrCursorExpired reports that a paginated read's cursor names a snapshot the server has
// since reclaimed versions of -- its lease ran out before the listing finished, or the
// server restarted -- so the rest of that listing cannot be read. It arrives as a
// *ServerError before any row of the page. Replaying the same cursor fails the same way:
// restart the listing from the zero cursor, and surface the restart to whoever holds the
// page token rather than presenting the rows so far as the whole list.
var ErrCursorExpired = errors.New("dbrpc: pagination cursor expired")

// cursorExpiredPrefix opens the message of a cursor-expired refusal, which the client maps to
// ErrCursorExpired.
const cursorExpiredPrefix = "cursor expired: "
