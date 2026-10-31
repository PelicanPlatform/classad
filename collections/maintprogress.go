package collections

import "time"

// Maintenance progress reporting.
//
// A maintenance pass is bounded but not short: the columnar budget alone caps a pass at 64
// segment rewrites, which on a history-scale archive is minutes of I/O. While one runs, the
// only externally visible evidence used to be CPU on a thread and an unchanged RSS -- an
// operator watching a daemon that had been told to stop could not tell a wedged process from
// a working one.
//
// What is published here is deliberately readable WITHOUT the maintenance lock, and without
// any shard lock: a pass stores an immutable *MaintProgress and readers load the pointer. A
// scrape therefore always answers, even in the middle of the pass it is describing. The cost
// is that the numbers may be a moment stale (a segment finished between the publish and the
// load). That trade is the right way round for this: a diagnostic that blocks is worth less
// than an approximate one that does not, because the time you need it is exactly the time
// something is holding the lock.

// MaintPhase names one kind of maintenance pass, for progress reporting and for the cumulative
// per-phase timing counters in OpStats. The values are stable strings: they are exported as a
// Prometheus label.
type MaintPhase string

const (
	// MaintColumnarize rewrites sealed segments into columnar-native form (ColumnarizeSealed,
	// recolumnarizeStale).
	MaintColumnarize MaintPhase = "columnarize"
	// MaintCompact reclaims dead space (Compact, Rewrite).
	MaintCompact MaintPhase = "compact"
	// MaintRetrain trains a fresh dictionary and recompresses under it (RetrainDict).
	MaintRetrain MaintPhase = "retrain"
	// MaintRotate drops whole oldest segments to honour retention (Rotate).
	MaintRotate MaintPhase = "rotate"
	// MaintMerge consolidates cold segment runs in an archive (MergePass).
	MaintMerge MaintPhase = "merge"
	// MaintReindex rebuilds segment sidecars (Reindex), including the one that follows a
	// compaction or a merge.
	MaintReindex MaintPhase = "reindex"
	// MaintBackfill carries a changed index set to older sealed segments (BackfillIndexes).
	MaintBackfill MaintPhase = "backfill"
)

// MaintProgress is a point-in-time view of the maintenance pass running on a collection, or
// the zero value when none is. It is published by the pass itself at each safe boundary and
// read locklessly, so reading it never contends with the pass it describes.
type MaintProgress struct {
	// Phase is the pass that is running. Empty when Active is false.
	Phase MaintPhase
	// Active reports whether a pass is in flight right now.
	Active bool
	// Started is when the current pass began.
	Started time.Time
	// Done is the units of work finished so far (segments, for every pass that reports a
	// Total). Total is how many the pass budgeted, or 0 when the pass cannot say up front --
	// a merge, for instance, discovers each run as it goes, so it reports Done only.
	Done  int
	Total int
}

// Elapsed is how long the current pass has been running, or 0 when none is.
func (p MaintProgress) Elapsed() time.Duration {
	if !p.Active || p.Started.IsZero() {
		return 0
	}
	return time.Since(p.Started)
}

// Remaining is the units of work the pass still expects to do, or 0 when it did not report a
// total (never negative: a pass may finish early, and one that over-runs its own estimate
// should read as "nearly done", not as a negative backlog).
func (p MaintProgress) Remaining() int {
	if p.Total <= p.Done {
		return 0
	}
	return p.Total - p.Done
}

// MaintProgress reports the maintenance pass running on this collection, or the zero value
// when none is.
//
// It takes NO lock -- not maintMu, not a shard lock -- so it answers promptly while the pass
// it describes holds both. See the package comment above for why staleness is the acceptable
// side of that trade.
func (c *Collection) MaintProgress() MaintProgress {
	if p := c.maintProg.Load(); p != nil {
		return *p
	}
	return MaintProgress{}
}

// MaintProgress reports the maintenance pass running on the archive's underlying collection.
// See Collection.MaintProgress.
func (a *Archive) MaintProgress() MaintProgress { return a.c.MaintProgress() }

// maintPass is one in-flight pass's handle. It publishes progress at the pass's own safe
// boundaries and, on finish, folds the pass's wall time into the matching cumulative counter
// (the same opCounter family OpStats already reports, so a phase has one timing total and not
// two disagreeing ones).
//
// Passes nest -- a compaction ends in a reindex, a merge does too -- so a pass restores
// whatever was published before it on finish, rather than clearing the slot. An operator then
// sees the innermost thing actually running, and the enclosing pass reappears when it returns.
type maintPass struct {
	c     *Collection
	prev  *MaintProgress
	cur   MaintProgress
	start time.Time
}

// beginMaint publishes the start of a pass. total is the work it budgeted, or 0 when it
// cannot say. The returned handle must have finish called on it, normally deferred.
func (c *Collection) beginMaint(phase MaintPhase, total int) *maintPass {
	now := time.Now()
	m := &maintPass{
		c:     c,
		prev:  c.maintProg.Load(),
		cur:   MaintProgress{Phase: phase, Active: true, Started: now, Total: total},
		start: now,
	}
	m.publish()
	return m
}

// step records one more unit of work finished and republishes. Called at a pass's safe
// boundary -- between segments, not inside one -- so the published Done never counts a unit
// that is still half-written.
func (m *maintPass) step() {
	if m == nil {
		return
	}
	m.cur.Done++
	m.publish()
}

// setTotal revises the budget once the pass knows it (a columnarize pass learns how many
// candidate segments it actually has only after it has listed them).
func (m *maintPass) setTotal(total int) {
	if m == nil || m.cur.Total == total {
		return
	}
	m.cur.Total = total
	m.publish()
}

// finish folds the pass's wall time into its cumulative counter and restores whatever was
// published before it.
func (m *maintPass) finish() {
	if m == nil {
		return
	}
	if ctr := m.c.opm.phase(m.cur.Phase); ctr != nil {
		ctr.observe(time.Since(m.start))
	}
	m.c.maintProg.Store(m.prev)
}

func (m *maintPass) publish() {
	cur := m.cur // copy: what is published must never be mutated under a reader
	m.c.maintProg.Store(&cur)
}

// phase maps a MaintPhase to the cumulative counter OpStats reports it under, or nil for a
// phase with no counter of its own.
func (o *opMetrics) phase(p MaintPhase) *opCounter {
	switch p {
	case MaintColumnarize:
		return &o.columnarize
	case MaintCompact:
		return &o.compact
	case MaintRetrain:
		return &o.retrain
	case MaintRotate:
		return &o.rotate
	case MaintMerge:
		return &o.merge
	case MaintReindex:
		return &o.reindex
	}
	return nil // MaintBackfill is counted per segment by BackfillIndexes itself
}
