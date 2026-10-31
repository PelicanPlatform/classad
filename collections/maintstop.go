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
// where no segment is mid-rewrite: between segments, between merges and between shards.
//
// Why abandoning a pass at one of those boundaries leaves no partial state. This is the whole
// value of the signal -- an operator will only use it if stopping cannot corrupt anything --
// so the argument is recorded per call site rather than asserted once:
//
//   - columnarizeSealed / recolumnarizeStale (colnative_build.go), between segments. One
//     segment's rewrite ends in commitSegmentRewrite, which fsyncs the staged output, writes a
//     marker naming every source, renames the output into place, publishes it under the shard
//     lock and only then unlinks the sources. The published state is therefore either the old
//     segment or the new one, never a mixture, and a crash anywhere inside it is completed or
//     discarded by finishPendingMerges. Stopping before the next segment leaves that segment
//     exactly as it was. The backlog does not grow: each segment is columnarized at most once,
//     so the next pass resumes from what is left.
//
//   - mergePass (archive_mergepolicy.go), between merges. Each merge is one
//     commitSegmentRewrite, so the same argument holds: the run is consolidated or it is not.
//     The runs the pass did not reach are untouched and still eligible next pass, and
//     nextMergeRun re-derives its candidates from the current segment list, so it does not
//     depend on where the previous pass stopped.
//
//   - Compact (compact.go), between shards. compactShard is already all-or-nothing for its
//     shard, and not because of this signal: it has internal abort paths (a dict that outgrows
//     its reserve, an unreadable source, a horizon it cannot raise) and every one of them
//     reaps the unpublished destination segments and returns with the shard's sources still in
//     place. Phase 3 does the install -- id assignment, supersession transfer, directory
//     rebuild and segment swap -- in a single critical section under the shard write lock.
//     Stopping between shards is indistinguishable from a pass whose remaining shards were
//     found to be under the compaction threshold, which is the ordinary no-op case.
//
//   - BackfillIndexes (index_backfill.go), between segments. A sidecar is built off-lock and
//     swapped in whole by installSidecar; a segment that is not reached keeps the index it
//     already has, which still answers for the attributes it covers. The remainder is not
//     lost track of: IndexBackfillPending recomputes it from the resident sidecars, so the
//     next pass sees the same work the stopped one would have done.
//
// What is NOT interrupted, deliberately: the rewrite of a single segment, and RetrainDict,
// which has no boundary a caller could stop at (train, swap the codec, recompress every shard
// is one operation). A pass abandoned mid-rewrite would be the one unsafe thing, since the
// shutdown that follows unmaps the segment the rewrite is still reading.
func (c *Collection) maintStopping() bool { return c.maintStop.Load() }

// StopMaintenance asks the archive's underlying collection to stop maintenance. See
// Collection.StopMaintenance.
func (a *Archive) StopMaintenance() { a.c.StopMaintenance() }

// MaintenanceStopping reports whether this collection has been asked to stop maintenance.
//
// Exported for the ORCHESTRATOR of a maintenance pass, not for the pass itself. One "pass" at
// the server layer is half a dozen independent steps -- merge, reindex, index backfill, group
// derivation, schema scan, hot-set refresh -- each of which honours the signal internally but
// none of which knows it is one of six. Without this, stopping during the first step still
// paid for the other five, which on an archive at history scale is most of the shutdown an
// operator was waiting out.
func (c *Collection) MaintenanceStopping() bool { return c.maintStopping() }

// MaintenanceStopping reports whether the archive's underlying collection has been asked to
// stop maintenance. See Collection.MaintenanceStopping.
func (a *Archive) MaintenanceStopping() bool { return a.c.MaintenanceStopping() }
