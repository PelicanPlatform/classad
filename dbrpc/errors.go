package dbrpc

import "errors"

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
