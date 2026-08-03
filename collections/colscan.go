package collections

import "github.com/PelicanPlatform/classad/collections/wire"

// colSegment is a sealed segment's columnar accelerator: the PAX block plus offs, mapping each
// block record k to its arena offset so a scan can read the record's live MVCC seq/sup.
type colSegment struct {
	block *columnarBlock
	offs  []uint32
}

// EnableSchemaScan builds a columnar block over every currently-sealed segment (skipping the
// active append target) for the given schema and hot field set, and publishes it on each
// segment. Additive and opt-in: with no block a scan falls back to the row path, so a
// collection that never calls this is unaffected. Reads immutable sealed bytes only.
func (c *Collection) EnableSchemaScan(s *adSchema, hot []int) {
	if c.schemaCache.Load() == nil {
		if bc, err := newBlockCache(256 << 20); err == nil { // ~256 MiB of decompressed blocks
			c.schemaCache.Store(bc)
		}
	}
	for _, sh := range c.shards {
		sh.mu.RLock()
		act := sh.act
		segs := make([]*segment, 0, len(sh.segs))
		for _, seg := range sh.segs {
			if seg != nil && seg != act && seg.used > 0 && seg.colblk.Load() == nil {
				segs = append(segs, seg) // sealed ⇒ data/used immutable, safe to read off-lock
			}
		}
		sh.mu.RUnlock()
		for _, seg := range segs {
			blk, offs := buildColumnarFromSegment(seg.data, seg.used, seg.codec, s, hot)
			seg.colblk.Store(&colSegment{block: blk, offs: offs})
		}
	}
}

// schemaScanIntCount counts records whose numeric field (s.fields[fieldIdx]) is present, live,
// and satisfies match -- using each sealed segment's columnar block (hot column: no decode;
// cold column: one decode) and each window's live MVCC visibility. A window with no block
// (the active segment, or one built before EnableSchemaScan) falls back to the row scan. A
// record whose value is escaped out of the hot column is reconstructed and re-checked (its
// value may live in the cold tail, e.g. an out-of-width int).
func (c *Collection) schemaScanIntCount(s *adSchema, fieldIdx int, match func(int64) bool) int {
	fieldID := s.fields[fieldIdx].id
	bc := c.schemaCache.Load()
	count := 0
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			cs := w.seg.colblk.Load()
			if cs != nil && cs.block.schema == s {
				cs.block.scanInt(fieldIdx, bc, func(k int, present bool, v int64) {
					o := cs.offs[k]
					if !(recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0) {
						return // not visible at this snapshot
					}
					if present {
						if match(v) {
							count++
						}
						return
					}
					if rec, err := cs.block.reconstruct(k, bc); err == nil { // escaped: check the cold tail
						if v2, ok := intFieldOf(s, rec, fieldID); ok && match(v2) {
							count++
						}
					}
				})
				continue
			}
			count += bruteIntCount(w, s0, fieldID, match)
		}
		releaseWindows(wins)
	}
	return count
}

// bruteIntCount is the row-scan fallback: walk a window's visible records, read the field
// from the wire ad, and count int matches.
func bruteIntCount(w segWindow, s0 uint64, fieldID uint32, match func(int64) bool) int {
	count := 0
	var buf []byte
	for off := 0; off < w.used; {
		o := uint32(off)
		total := recTotalLen(w.data, o)
		if total == 0 {
			break
		}
		if !recIsMarker(w.data, o) && recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0 {
			if ww, err := w.codec.Decompress(buf[:0], recAd(w.data, o)); err == nil {
				buf = ww
				if node, ok := wire.Ad(ww).Lookup(fieldID); ok {
					if lit, ok := wire.LiteralValue(node); ok && lit.Kind == wire.LitInt && match(lit.Int) {
						count++
					}
				}
			}
		}
		off += int(total)
	}
	return count
}

// intFieldOf returns the int value of fieldID in a reconstructed schema record, if present as
// an integer (missing or non-integer ⇒ false).
func intFieldOf(s *adSchema, rec []byte, fieldID uint32) (int64, bool) {
	var out int64
	var found bool
	s.forEach(rec, func(id uint32, node []byte) bool {
		if id != fieldID {
			return true
		}
		if lit, ok := wire.LiteralValue(node); ok && lit.Kind == wire.LitInt {
			out, found = lit.Int, true
		}
		return false
	})
	return out, found
}
