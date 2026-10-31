package collections

import (
	"sort"
	"sync"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// VALUES A COLUMNARIZED SEGMENT LEFT IN ITS RECORDS.
//
// Columnarizing a sealed segment moves every LITERAL into its columnar payload and leaves everything
// else -- expressions, nested ads, lists, sealed values -- in the record (see storableInColumn). The
// payload's blocks are built from the literal half alone, so to the block a schema field whose value
// stayed in the record looks exactly like a field the record does not have: the escape bit is set, the
// cold tail has no entry, and the escape class says MISSING.
//
// Every reader that answered "escaped and not in the cold tail" with UNDEFINED was therefore wrong for
// those cells, and not slightly: `MemoryUsage is undefined` counted every record whose MemoryUsage is
// `((ResidentSetSize + 1023) / 1024)`, which on a history table is nearly all of them. The count, the
// numeric GROUP BY and the presence count all agreed with one another, because they share the column
// readers; only a scan, which reassembles the record, disagreed.
//
// The block cannot tell the two apart and the on-disk format records nothing that could, so the answer
// comes from the records themselves: one pass over the segment's remnants, the first time a reader
// needs it, noting which records hold which schema field. That is the exact set of cells the columns
// do not know about. A cell outside it that is escaped and absent from the cold tail really is absent.
//
// The pass reads only remnants, which hold what the columns did not take and are usually a header and
// a key, and it runs once per segment per process. Only schema fields are recorded -- base and group --
// so the index is as sparse as the exceptions it describes.

// heldIndex is a columnarized segment's map of the schema-field cells its records still hold.
type heldIndex struct {
	once sync.Once
	ok   bool                // false: the remnants could not be read, so nothing may be assumed absent
	recs map[uint32][]uint32 // global attribute id -> ascending segment record indices holding it

	// uncomputed caches, per (block's first record, field index), how many of the block's held cells
	// of that field have no computed value -- the per-record exception budget (heldValueNode).
	excMu      sync.Mutex
	uncomputed map[[2]int]int
}

// heldIndexBuilds counts remnant passes, so a test can see the index is built once per segment.
var heldIndexBuilds atomic.Int64

// held returns the segment's held-cell index, building it on first use. nil for a segment that is not
// columnarized, whose blocks hold every value they describe.
//
// The caller must have the segment pinned (a reader holding its window), since the pass reads the
// arena.
func (cs *colSegment) held() *heldIndex {
	if cs == nil || cs.arena == nil {
		return nil
	}
	h := cs.heldIdx
	h.once.Do(func() {
		heldIndexBuilds.Add(1)
		h.recs, h.ok = cs.scanHeld()
	})
	return h
}

// scanHeld walks the segment's remnants and records, per schema field, the records holding it.
func (cs *colSegment) scanHeld() (map[uint32][]uint32, bool) {
	c, seg := cs.owner, cs.arena
	if c == nil || c.intern == nil {
		return nil, false
	}
	fields := map[uint32]struct{}{}
	if sch := cs.schema(); sch != nil {
		for _, f := range sch.fields {
			fields[f.id] = struct{}{}
		}
	}
	for _, g := range cs.groups {
		for _, f := range g.schema.fields {
			fields[f.id] = struct{}{}
		}
	}
	d := seg.dict.Load()
	recs := map[uint32][]uint32{}
	var buf []byte
	n := cs.offsLen()
	dm := seg.damaged()
	var exp []byte
	bc := c.sharedColCache()
	bi, blkEnd := 0, 0
	for i := 0; i < n; i++ {
		// The base block record i belongs to, whose dictionary its remnant's references point into.
		for i >= blkEnd && bi < len(cs.blocks) {
			blkEnd += cs.blocks[bi].n
			bi++
		}
		if i >= blkEnd {
			return nil, false // a row past every block: the payload does not describe this record
		}
		blk := cs.blocks[bi-1]
		o := cs.offAt(i)
		if int(o) >= len(seg.data) {
			return nil, false
		}
		if dm.covers(o) {
			continue // a damaged record's row: nothing of the record can be read
		}
		raw, err := seg.codec.Decompress(buf[:0], recAd(seg.data, o))
		if err != nil {
			return nil, false
		}
		buf = raw
		// Expanded: the walk below must step over every node, and a reference is not one wire knows.
		if e, expanded, eerr := blk.expandRemnant(raw, bc, exp[:0]); eerr != nil {
			return nil, false
		} else if expanded {
			exp, raw = e, e
		}
		bad := false
		// A walk that stops short of the record's end leaves every attribute after the stop out of the
		// index, and a cell the index omits is read as absent -- so a remnant that does not parse to
		// its end makes the whole index untrusted, exactly as an unresolvable name does.
		whole := wire.Ad(raw).ForEachRaw(func(id uint32, name string, node []byte) bool {
			gid, ok := c.globalIDOf(d, id, name)
			if !ok {
				// An attribute the remnant names but this process cannot resolve. It cannot be a
				// field the columns describe under a name we know, but it cannot be ruled out
				// either, so the index is not trusted.
				bad = true
				return false
			}
			if _, isField := fields[gid]; isField {
				recs[gid] = append(recs[gid], uint32(i))
			}
			return true
		})
		if bad || !whole {
			return nil, false
		}
	}
	return recs, true
}

// heldAt reports whether record k of this block holds field id in its record rather than in the
// columns -- a value the block cannot read. Always false for a block that is not part of a
// columnarized segment, and always true when the segment's remnants could not be indexed: a reader
// told true declines to the record, which is never wrong.
func (b *columnarBlock) heldAt(k int, id uint32) bool {
	h := b.native.held()
	if h == nil {
		return false
	}
	if !h.ok {
		return true
	}
	list := h.recs[id]
	if len(list) == 0 {
		return false
	}
	want := uint32(b.segBase + k)
	i := sort.Search(len(list), func(i int) bool { return list[i] >= want })
	return i < len(list) && list[i] == want
}

// heldKnown is heldAt without the benefit of the doubt: true only when the index is readable and
// lists record k as holding field id. A caller that SKIPS work on true uses this; a caller that
// declines on true uses heldAt.
func (b *columnarBlock) heldKnown(k int, id uint32) bool {
	h := b.native.held()
	if h == nil || !h.ok {
		return false
	}
	list := h.recs[id]
	want := uint32(b.segBase + k)
	i := sort.Search(len(list), func(i int) bool { return list[i] >= want })
	return i < len(list) && list[i] == want
}

// heldAny reports whether any record of this block holds field id in its record. See heldAt.
func (b *columnarBlock) heldAny(id uint32) bool {
	h := b.native.held()
	if h == nil {
		return false
	}
	if !h.ok {
		return true
	}
	list := h.recs[id]
	if len(list) == 0 {
		return false
	}
	lo := uint32(b.segBase)
	i := sort.Search(len(list), func(i int) bool { return list[i] >= lo })
	return i < len(list) && list[i] < lo+uint32(b.n)
}

// bindNative ties a columnarized segment's blocks to it, so a block can ask which of its cells the
// records still hold. Called once when the payload is published.
func (cs *colSegment) bindNative(c *Collection, seg *segment) {
	cs.owner, cs.arena = c, seg
	cs.heldIdx = &heldIndex{}
	base := 0
	for _, b := range cs.blocks {
		b.native, b.segBase = cs, base
		base += b.n
	}
}
