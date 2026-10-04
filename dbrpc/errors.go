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
//   - context.Canceled / context.DeadlineExceeded: the caller's context ended while
//     waiting. Surface it; do not retry against the caller's wishes.
//
// Callers classify with errors.Is / errors.As rather than matching message text.

// ErrConnClosed reports that the dbrpc connection is no longer usable. It wraps the
// underlying transport cause, so errors.Is(err, ErrConnClosed) identifies the class
// while errors.Unwrap reaches the specific I/O error.
var ErrConnClosed = errors.New("dbrpc: connection closed")

// ServerError is a logical error the server returned for a request (wire status
// stErr): a bad constraint, an unknown table, a malformed op. It is deterministic
// -- replaying the request fails the same way -- so callers should surface it
// rather than retry.
type ServerError struct{ Msg string }

func (e *ServerError) Error() string { return "dbrpc: " + e.Msg }

// Is matches ErrTableReadOnly for a per-table write refusal, so errors.Is tells that
// refusal apart from other server errors while errors.As still yields the *ServerError.
func (e *ServerError) Is(target error) bool {
	return target == ErrTableReadOnly && strings.HasPrefix(e.Msg, readOnlyTablePrefix)
}

// ErrTableReadOnly reports that the server refused to modify a table this connection may
// only read (ServeOptions.TableWritable) -- typically one an in-process writer owns, such
// as a mirror of another store. It arrives as a *ServerError and, like every ServerError,
// is deterministic: replaying the write fails the same way, so surface it, never retry.
var ErrTableReadOnly = errors.New("dbrpc: table is read-only")
