package collections

// StopMaintenance asks any maintenance pass running on this collection to stop at its next
// safe boundary. It does not wait, and it does not interrupt work already in progress on a
// single segment -- a pass abandoned mid-rewrite would be the one thing a caller must never
// do, since the shutdown that follows unmaps the segments a rewrite is still reading.
//
// It exists because a maintenance pass is bounded but not short. The columnar budget caps a
// pass at 64 segment rewrites so a schema change converges over several passes instead of
// stalling on one, which keeps a pass unnoticeable in steady state -- but a caller closing
// the server waits for the pass in flight to finish, and on an archive at history scale that
// is tens of seconds of apparently-hung process after the logs say it stopped.
//
// The abandoned work is not lost: every segment is rewritten at most once, so the backlog
// only shrinks, and the next pass after reopen picks up exactly where this one left off.
//
// Once stopped a collection stays stopped; this is a shutdown signal, not a pause.
func (c *Collection) StopMaintenance() { c.maintStop.Store(true) }

// maintStopping reports whether StopMaintenance has been called. Maintenance loops check it
// where no segment is mid-rewrite: between segments and between shards.
func (c *Collection) maintStopping() bool { return c.maintStop.Load() }

// StopMaintenance asks the archive's underlying collection to stop maintenance. See
// Collection.StopMaintenance.
func (a *Archive) StopMaintenance() { a.c.StopMaintenance() }
