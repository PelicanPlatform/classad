package collections

import "strings"

// SegmentProfile reports one sealed segment's retention profile for diagnostics. It is what
// makes a size-driven eviction explainable after the fact: which segments are orderable by
// value, how much of each sits below a given point, and which are not orderable at all.
type SegmentProfile struct {
	Segment int    // the segment's index, which is also its id
	Attr    string // the attribute profiled; empty when the segment has no profile
	// Bytes is the stored bytes the profile accounts for; Unknown is the part of it whose
	// value could not be bounded (an expression, a sealed value, a non-numeric, or absent).
	// Unknown bytes are never freed by value -- only the positional fallback reclaims them.
	Bytes   int64
	Unknown int64
	// ValueMin/ValueMax span the values that WERE bounded. Meaningless when HasValues is
	// false, which is a segment whose every record was unbounded.
	ValueMin, ValueMax float64
	HasValues          bool
	// Buckets is how many breakpoints the profile carries, i.e. how finely a cutoff can be
	// placed within this segment.
	Buckets int
	// ReceivedMin/ReceivedMax are the store's own clock when the segment was sealed (unix
	// seconds). They exist for ClockSuspect below, not for retention, which never consults
	// them -- evicting on when a store RECEIVED a record is how a replayed backlog looks
	// brand new and never ages out.
	ReceivedMin, ReceivedMax int64
	// ClockSuspect marks a segment whose profiled values sit inside its own receipt window.
	// Records' timestamps describe events that happened before this store heard about them,
	// so a segment whose values track its receipt time is one whose source stamped ITS OWN
	// clock rather than deriving the value from the record. Such a segment orders correctly
	// against others from the same source and wrongly against everything else.
	//
	// Advisory only: it never changes an eviction decision. A source that is genuinely
	// writing records as they happen looks the same, which is why this is surfaced for an
	// operator to interpret rather than acted on.
	ClockSuspect bool
}

// SegmentProfiles reports the retention profile of every sealed segment, oldest segment
// first. Diagnostic only.
func (c *Collection) SegmentProfiles() []SegmentProfile {
	if len(c.shards) == 0 {
		return nil
	}
	sh := c.shards[0] // profiles exist only on an append-only collection: one shard
	sh.mu.RLock()
	defer sh.mu.RUnlock()

	out := make([]SegmentProfile, 0, len(sh.segs))
	for i, seg := range sh.segs {
		if seg == nil || seg == sh.act {
			continue
		}
		sp := SegmentProfile{Segment: i}
		if p := seg.profile; p != nil {
			sp.Attr = p.attr
			sp.Bytes, sp.Unknown = p.totalBytes, p.unknownBytes
			sp.ValueMin, sp.ValueMax, sp.HasValues = p.valueMin, p.valueMax, p.haveValues
			sp.Buckets = len(p.breaks)
			sp.ReceivedMin, sp.ReceivedMax = p.recvMin, p.recvMax
			sp.ClockSuspect = clockSuspect(p)
		}
		out = append(out, sp)
	}
	return out
}

// clockSuspect reports whether a segment's profiled values lie within its own receipt
// window. See SegmentProfile.ClockSuspect.
//
// The window is widened by a margin because a value and a receipt time taken moments apart
// are not expected to be equal, only close; without it every freshly tailed segment would
// trip. It is not configurable: the check is a hint to look, not a threshold anyone tunes.
func clockSuspect(p *segProfile) bool {
	const margin = 300 // seconds
	if p == nil || !p.haveValues || p.recvMin == 0 || p.recvMax == 0 {
		return false
	}
	lo := float64(p.recvMin - margin)
	hi := float64(p.recvMax + margin)
	return p.valueMin >= lo && p.valueMax <= hi
}

// SegmentProfiles reports every sealed segment's retention profile (see
// Collection.SegmentProfiles).
func (a *Archive) SegmentProfiles() []SegmentProfile { return a.c.SegmentProfiles() }

// EvictionAttr is the attribute size-driven eviction orders this archive by, or "" when it
// falls back to dropping the oldest segment by position.
func (a *Archive) EvictionAttr() string { return a.c.Retention().evictAttr() }

// HighMarkStats reports the seal-time size evictions (see Collection.HighMarkStats).
func (a *Archive) HighMarkStats() (passes, dropped int64) { return a.c.HighMarkStats() }

// ProfiledSegments counts how many sealed segments carry a profile over attr, and how many
// are sealed in total. A size pass can only order the profiled ones; the rest fall back to
// position, so a large gap is why an archive is still evicting by arrival order.
func (a *Archive) ProfiledSegments() (profiled, sealed int) {
	attr := a.EvictionAttr()
	for _, p := range a.SegmentProfiles() {
		sealed++
		if attr != "" && p.Attr != "" && strings.EqualFold(p.Attr, attr) {
			profiled++
		}
	}
	return profiled, sealed
}
