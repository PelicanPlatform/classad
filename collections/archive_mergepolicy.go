package collections

// Which segments to merge.
//
// The mechanism (mergeSegments) collapses an adjacent run; this decides which runs, and it
// answers to one number: how many segments the archive may hold. That number is set by
// mappings, not by query cost -- every sealed segment is mmap'd at open and each queried
// sidecar adds a second, so a table costs about 2 mappings per segment against a process-wide
// vm.max_map_count (65530 by default, shared with every other table in the same database).
//
// Two shapes have to be respected at once, and they disagree:
//
//   - Fewer segments is better for mappings, and for open cost (recovery is O(segments)).
//   - Smaller segments are better for queries, because the OPEN segment is rescanned on
//     every index-resident query -- its size is a per-query cost, forever.
//
// Merging cold runs while leaving the newest segments alone satisfies both: the tail stays
// finely divided where queries land, and the body consolidates where only mappings care.
//
// Run size is driven by BYTES, not by a fixed fan-in. Segments are not uniformly full -- a
// seal can land early, and a resealed segment is sized exactly to its contents -- so "merge 8
// at a time" produces wildly different results depending on how full they happen to be. The
// target size is derived from the goal rather than configured: to land at TargetSegments, the
// average segment has to be about total/TargetSegments, so runs grow toward that (clamped),
// which makes the policy self-adjusting as the archive grows.

// MergeOptions tunes a merge pass. The zero value is usable: it fills in the defaults below.
type MergeOptions struct {
	// TargetSegments is the LOW watermark: once a pass starts it merges until the archive
	// is at or below this. Default defaultTargetSegments.
	TargetSegments int
	// TriggerSegments is the HIGH watermark: a pass does nothing until the archive reaches
	// it. The gap between the two is what stops the policy from merging a run on every pass
	// forever once it is sitting on the target -- rewriting data continuously to hold a line
	// that does not need holding to the segment. Default: TargetSegments plus a quarter.
	TriggerSegments int
	// MaxSegmentBytes caps a merged segment. Segment offsets are uint32 throughout the
	// record and sidecar formats, so 4 GiB is a hard structural ceiling; the default stays
	// well under it. A run stops before exceeding this.
	MaxSegmentBytes int64
	// MinMergeBytes is the smallest output worth producing. A run below it is still merged
	// if it is the best available -- reducing the count is the point -- but the policy
	// prefers to keep accumulating.
	MinMergeBytes int64
	// MaxRun bounds how many segments one merge consumes, so a single merge's work and
	// memory stay predictable however small the segments are.
	MaxRun int
	// KeepRecent leaves the newest sealed segments untouched, keeping the hot end of the
	// archive finely divided (and away from the open segment the writer is appending to).
	KeepRecent int
	// MaxMerges bounds one pass, so maintenance stays interruptible and a large backlog is
	// worked down over several passes instead of one long stall.
	MaxMerges int
	// MaxBytesPerPass bounds the SOURCE bytes one pass rewrites. MaxMerges alone does not
	// bound the work, because a merge's cost is its inputs' size, not its count -- one pass
	// of large runs can move orders of magnitude more than another of the same length.
	//
	// This exists for the catch-up case: an archive that grew while merging was off has a
	// backlog measured in the size of the archive, and without a byte bound the first pass
	// would try to rewrite all of it at once. Steady state does not need it -- a pass stops
	// at the low watermark long before this -- so the default is sized to cap one pass at a
	// tolerable stall (~25s at the ~160 MB/s merging sustains) rather than to throttle.
	//
	// Sustained RATE is the scheduler's job, not this: pace passes so the long-run average
	// stays within the byte budget the deployment allows.
	MaxBytesPerPass int64
}

const (
	// defaultTargetSegments is deliberately far below the mapping ceiling. At ~2 mappings
	// per segment this is ~16k for one table, leaving room for the other tables sharing the
	// process's vm.max_map_count.
	defaultTargetSegments  = 8192
	defaultMaxSegmentBytes = 1 << 30 // 1 GiB; the uint32 offset ceiling is 4 GiB
	defaultMinMergeBytes   = 64 << 20
	defaultMaxRun          = 64
	defaultKeepRecent      = 16
	defaultMaxMerges       = 64
	defaultMaxBytesPerPass = 4 << 30 // ~25s of merging at the measured throughput
)

func (o MergeOptions) withDefaults() MergeOptions {
	if o.TargetSegments <= 0 {
		o.TargetSegments = defaultTargetSegments
	}
	if o.TriggerSegments <= o.TargetSegments {
		o.TriggerSegments = o.TargetSegments + o.TargetSegments/4
	}
	if o.MaxSegmentBytes <= 0 {
		o.MaxSegmentBytes = defaultMaxSegmentBytes
	}
	if o.MinMergeBytes <= 0 {
		o.MinMergeBytes = defaultMinMergeBytes
	}
	if o.MaxRun <= 1 {
		o.MaxRun = defaultMaxRun
	}
	if o.KeepRecent < 0 {
		o.KeepRecent = defaultKeepRecent
	}
	if o.MaxMerges <= 0 {
		o.MaxMerges = defaultMaxMerges
	}
	if o.MaxBytesPerPass <= 0 {
		o.MaxBytesPerPass = defaultMaxBytesPerPass
	}
	return o
}

// MergePass merges cold segment runs when the archive has reached opts.TriggerSegments,
// continuing until it is at or below opts.TargetSegments, no eligible run remains, or
// opts.MaxMerges merges have been done. It returns the number of merges performed.
//
// The two watermarks are the point: merging is rewriting, so it should happen in occasional
// batches with headroom either side, not continuously to pin the count at one value. Below
// the trigger a pass is a cheap no-op (a segment count and a comparison), so it is safe to
// call often.
//
// Safe to call on a live archive: merges take the maintenance lock (so they never overlap a
// reseal or another pass), the writer's open segment is never a candidate, and an in-flight
// scan holding a source keeps its mapping alive through the usual pin/reap path.
func (a *Archive) MergePass(opts MergeOptions) int {
	return a.c.mergePass(opts.withDefaults())
}

func (c *Collection) mergePass(opts MergeOptions) int {
	if !c.appendOnly() {
		return 0
	}
	c.maintMu.Lock()
	defer c.maintMu.Unlock()

	if c.sealedSegmentCount() < opts.TriggerSegments {
		return 0 // below the high watermark: leave it alone
	}
	merges := 0
	var moved int64
	for merges < opts.MaxMerges && moved < opts.MaxBytesPerPass {
		run, ok := c.nextMergeRun(opts, opts.MaxBytesPerPass-moved)
		if !ok {
			break
		}
		var runBytes int64
		for _, s := range run {
			runBytes += int64(s.used)
		}
		if !c.mergeSegments(c.shards[0], run) {
			break // a failed merge leaves its sources in place; do not spin on them
		}
		merges++
		moved += runBytes
	}
	if merges > 0 {
		// The merged segments have no sidecar yet; build them so queries do not fall back
		// to scanning what they just consolidated.
		c.Reindex()
	}
	return merges
}

// nextMergeRun picks the oldest adjacent run worth merging, or ok=false when the archive is
// already at or below target, or nothing eligible remains.
//
// Oldest-first is deliberate: the oldest data is the coldest, so merging it disturbs the
// fewest queries and the page cache least, and it produces the size gradient the two
// competing constraints want -- large at the tail of the archive, small at its head.
// remaining is what is left of the pass's byte budget; a run is capped by it as well as by
// MaxSegmentBytes, so a pass overshoots its budget by at most the last segment it adds rather
// than by a whole run -- which, with a large target, can be the entire archive.
func (c *Collection) nextMergeRun(opts MergeOptions, remaining int64) ([]*segment, bool) {
	sh := c.shards[0]
	sh.mu.RLock()
	defer sh.mu.RUnlock()

	// Sealed segments in append order (id order), excluding the open segment.
	var sealed []*segment
	var total int64
	for _, s := range sh.segs {
		if s == nil || s == sh.act || s.used == 0 {
			continue
		}
		sealed = append(sealed, s)
		total += int64(s.used)
	}
	if len(sealed) <= opts.TargetSegments {
		return nil, false
	}
	// Leave the newest KeepRecent alone: that is where queries land, and where the size of
	// a segment is a per-query cost rather than just a mapping.
	eligible := sealed
	if opts.KeepRecent < len(eligible) {
		eligible = eligible[:len(eligible)-opts.KeepRecent]
	} else {
		return nil, false
	}
	if len(eligible) < 2 {
		return nil, false
	}

	// Size runs from the goal: landing at TargetSegments means an average segment of about
	// total/TargetSegments, so grow toward that rather than toward a configured constant.
	want := total / int64(opts.TargetSegments)
	if want < opts.MinMergeBytes {
		want = opts.MinMergeBytes
	}
	if want > opts.MaxSegmentBytes {
		want = opts.MaxSegmentBytes
	}

	sizeCap := opts.MaxSegmentBytes
	if remaining > 0 && remaining < sizeCap {
		sizeCap = remaining
	}
	if want > sizeCap {
		want = sizeCap
	}

	var run []*segment
	var bytes int64
	for _, s := range eligible {
		sz := int64(s.used)
		// Skip past segments that are already at the target size. Without this the run
		// always restarts on the segment the previous pass just produced, growing it by one
		// small neighbour at a time: a merge per segment, and the big segment rewritten
		// every time -- quadratic in bytes moved, for a result the first merge nearly had.
		if len(run) == 0 && sz >= want {
			continue
		}
		if len(run) > 0 && (bytes+sz > sizeCap || len(run) >= opts.MaxRun) {
			break // this run is as large as it may get
		}
		run = append(run, s)
		bytes += sz
		if bytes >= want && len(run) >= 2 {
			break
		}
	}
	if len(run) < 2 {
		return nil, false // a single segment already at the size cap: nothing to do here
	}
	return run, true
}

// sealedSegmentCount counts the segments a merge pass could act on: sealed, non-empty, and
// not the open append target.
func (c *Collection) sealedSegmentCount() int {
	sh := c.shards[0]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	n := 0
	for _, s := range sh.segs {
		if s != nil && s != sh.act && s.used > 0 {
			n++
		}
	}
	return n
}
