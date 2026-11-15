package collections

// Trimming a segment that straddles an eviction cutoff.
//
// Whole-segment eviction handles the common case: a segment whose newest value is below the
// cutoff is unlinked, costing nothing but the unlink. A segment holding data on BOTH sides of
// the cutoff cannot be dropped that way without taking records that must be kept, so it is
// rewritten without the part below the cutoff.
//
// This is the expensive side of retention and it is deliberately last. A trim costs the bytes
// it KEEPS -- dropping 1 MiB from an 8 MiB segment rewrites 7 MiB -- so a pass takes every
// whole segment it can first and only then trims, under a budget, stopping as soon as it is
// under its target.
//
// It reuses the merge's staging and swap (commitSegmentRewrite): stage under a name recovery
// ignores, fsync, write an intent marker naming the source, rename, publish, unlink the
// source. A crash anywhere leaves a state recovery can finish or discard, and nothing about
// that protocol cares which kind of rewrite produced the file.

// trimBudgetBytes bounds how much one pass rewrites. A pass that has to trim is already the
// expensive case; without a bound, an archive that fell a long way behind its ceiling would
// try to rewrite its way back in a single pass. The next pass continues where this one
// stopped, as the merge policy's byte budget does.
const trimBudgetBytes = 64 << 20

// trimSegmentToCutoff rewrites seg without the records whose attr value is below cutoff,
// returning how many records were dropped. Records whose value cannot be bounded are KEPT
// (see resealFilter.keeps).
//
// The caller holds maintMu and must NOT hold the shard lock: the swap takes it.
func (c *Collection) trimSegmentToCutoff(sh *shard, seg *segment, attr string, cutoff float64) (int, bool) {
	if seg == nil || seg.used == 0 || sh.allocNamed == nil || sh.segDir == "" {
		return 0, false
	}
	id, ok := c.intern.LookupID(attr)
	if !ok {
		return 0, false
	}
	f := &resealFilter{attr: zoneAttr{name: attr, id: id}, cutoff: cutoff}
	out, filtered := c.resealSegmentsFiltered(sh, []*segment{seg}, c.currentCodec(), mergeTmpPrefix, f)
	if out == nil {
		return 0, false // damaged framing, a value that will not seal, or nothing left to write
	}
	if filtered == 0 {
		// The profile said this segment held data below the cutoff and the records disagree.
		// Publishing an identical segment would churn the file for nothing, so drop the staged
		// output and leave the original in place.
		out.retire()
		_ = out.reapAndHook() //nolint:errcheck // best effort: the staged file is already out of service
		return 0, false
	}
	if !c.commitSegmentRewrite(sh, []*segment{seg}, out, nil, nil) {
		return 0, false
	}
	return filtered, true
}
