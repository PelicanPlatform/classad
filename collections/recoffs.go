package collections

import "sort"

// WHERE A NEWEST-FIRST SCAN STARTS.
//
// Records are variable-length and carry only their FORWARD length, so there is no way to step
// backward through a segment: forEachVisibleRefReverse walks the segments backward but, inside
// each, first makes a forward pass reading every record's length header to collect its offset.
//
// For a newest-first query that stops after a handful of rows that pass is the whole cost. It
// reads one header per record, strided across the entire arena, so it touches every page of the
// segment to return five records that sit on one of them: at the 8 MiB default that is a 512x
// page amplification, at 256 MiB it is 16384x, and it was measured at about 99% of a cold
// LIMIT 5. Zone pruning does not help -- it only ever drops OLD segments, and the newest one,
// the only one such a query frames, is never prunable.
//
// A sealed segment already has a persisted per-record offset table (colSegment.offsB, mmapped
// from its sidecar). The segment a newest-first query frames first is the ACTIVE one -- still
// being appended to -- which has no sidecar. So the offsets are kept here, in memory, for that
// one segment: built once from the bytes already written and extended, not rebuilt, as the
// segment grows.
//
// Only the write target gets a table, and it is dropped when the segment stops being the write
// target, so the memory is bounded by one segment per shard rather than by the size of the
// store -- four bytes per record of the segment being appended to.

// recOffsSnap is one published view of a segment's record offsets.
//
// Immutable below len(offs): a later extension appends past that length (in place when the
// backing array has room, into a new array otherwise), so a reader holding this snapshot reads
// only words no one writes again.
type recOffsSnap struct {
	offs []uint32 // the offset of each record, ascending
	upto uint32   // the offset just past the last record covered
}

// offsRetired marks a segment that is no longer the shard's write target. It is one shared,
// empty snapshot, so a retired segment costs a pointer.
//
// A plain nil would not do: a reader that loaded the table just before the roll publishes with a
// compare-and-swap against what it loaded, and against nil that swap would succeed and put a
// table back on a segment that has just dropped one. Swapping against this cannot, so a sealed
// segment never holds a table and the memory stays bounded by the segment being appended to.
var offsRetired = &recOffsSnap{}

// reverseOffsets returns the offsets of w's records, ascending, for a newest-first walk to
// replay backward -- or nil when the caller must frame the window itself.
//
// It is nil for anything but the active segment: a sealed one would keep a table nothing drops.
// It is nil for a window with damage, whose walk steps over spans the framing does not describe,
// and for a window whose framing cannot be followed to its end (a damaged length), which is the
// walk's own business to notice and report.
//
// The returned slice may cover more than w: another scan's window may have reached further, and
// offsets at or past w.used are not this window's to see. They are trimmed off.
func reverseOffsets(w segWindow) []uint32 {
	if w.seg == nil {
		return nil
	}
	cur := w.seg.recOffs.Load()
	if cur == offsRetired {
		return nil
	}
	if !w.active {
		// No longer appended to (or never was): release whatever table it has. The write
		// target drops its own at the roll; this catches a segment that stopped being the
		// target some other way.
		if cur != nil {
			w.seg.dropRecOffs()
		}
		return nil
	}
	if w.dmg != nil {
		return nil
	}
	if cur != nil && int(cur.upto) >= w.used {
		return offsBelow(cur.offs, w.used)
	}
	w.seg.recOffsMu.Lock()
	defer w.seg.recOffsMu.Unlock()
	cur = w.seg.recOffs.Load()
	if cur == offsRetired {
		return nil
	}
	if cur != nil && int(cur.upto) >= w.used {
		return offsBelow(cur.offs, w.used)
	}
	next := extendRecOffs(cur, w)
	if next == nil {
		return nil
	}
	// A failed swap means the segment was retired as the write target while this ran (see
	// offsRetired); the table built here still describes bytes that never change, so this walk
	// uses it and nothing is published.
	w.seg.recOffs.CompareAndSwap(cur, next)
	return offsBelow(next.offs, w.used)
}

// extendRecOffs returns cur (nil: nothing yet) extended to cover w's records through w.used,
// following the same framing the walk does, or nil if it cannot be followed that far.
//
// Extending appends to cur's slice rather than copying it, which is what keeps the cost of a
// scan proportional to what was appended since the last one rather than to the segment. That is
// safe because a reader holds a snapshot whose length stops where this one starts: the words
// appended here are past the end of every slice already handed out.
//
// Caller holds the segment's recOffsMu.
func extendRecOffs(cur *recOffsSnap, w segWindow) *recOffsSnap {
	n := &recOffsSnap{}
	if cur != nil {
		n.offs, n.upto = cur.offs, cur.upto
	}
	off := int(n.upto)
	for off < w.used {
		o := uint32(off)
		total := recTotalLen(w.data, o)
		if total == 0 {
			break
		}
		n.offs = append(n.offs, o)
		off += int(total)
	}
	if off != w.used {
		// The framing ran out before the window's end, or past it: a damaged length. The walk
		// finds the same thing and reports it (noteFramingLost), and a table that stops short
		// of the window would quietly hide the records past the break.
		return nil
	}
	n.upto = uint32(off)
	return n
}

// offsBelow trims the offsets at or past used from an ascending offset slice.
func offsBelow(offs []uint32, used int) []uint32 {
	if len(offs) == 0 || int(offs[len(offs)-1]) < used {
		return offs
	}
	return offs[:sort.Search(len(offs), func(i int) bool { return int(offs[i]) >= used })]
}

// dropRecOffs releases a segment's record-offset table. Called when it stops being the shard's
// write target: it will not grow again, and keeping one per sealed segment would make the
// memory a function of the store rather than of the segment being written.
func (s *segment) dropRecOffs() {
	if s != nil {
		s.recOffs.Store(offsRetired)
	}
}

// recOffsState reports a segment's offset table for tests: how many records it covers and how
// far into the arena, or (0, 0, false) when it has none.
func (s *segment) recOffsState() (records int, upto uint32, ok bool) {
	c := s.recOffs.Load()
	if c == nil || c == offsRetired {
		return 0, 0, false
	}
	return len(c.offs), c.upto, true
}
