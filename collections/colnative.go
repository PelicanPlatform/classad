package collections

import (
	"errors"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// COLUMNAR-NATIVE SEALED SEGMENTS.
//
// A sealed segment normally stores each record's whole ad, compressed on its own, and the columnar
// accelerator keeps a SECOND copy of the same values beside it in the sidecar. Measured on real
// OSPool machine ads, dictionary-compressed both ways:
//
//	row form, per record   3445 B/rec
//	columnar, same records  673 B/rec      (0.195x)
//
// Per-record compression cannot see across records; a column can, and on ads that share most of
// their values with their neighbours that is worth 5x. Keeping both copies means paying the larger
// one for nothing.
//
// So a columnarized segment stores each attribute exactly once. A record's own bytes carry only
// what the schema does NOT cover, and the schema'd values live in one columnar record at the end of
// the segment -- the same framing the per-segment dictionary already uses, so it shares the
// segment's record CRC, its fsync, and its lifetime. There is no way to open a segment and find its
// columns missing, which is the property that lets them be authoritative.
//
// WHAT CHANGES, AND WHAT DOES NOT
//
// The sidecar goes back to being purely derived: indexes and statistics, rebuildable from the
// segment at any time. The segment gains a region that is NOT derived, which is the real cost of
// this design -- losing it is data loss rather than a rebuild. That is why it lives inside the
// segment file rather than beside it.
//
// Only SEALED segments are columnarized, at the same point interning already rewrites them. The
// active segment stays whole-record, because it is still being appended to and a column cannot be
// extended in place. So a collection holds both shapes at once and every reader must handle both;
// recordWire is the one place that difference is resolved.

// colNative is a sealed segment's authoritative columnar payload, published on recovery the way a
// segment dictionary is.
type colNative struct {
	seg *colSegment // schema + blocks + per-record arena offsets
	// byOff maps an arena record offset to its index among the segment's data records, so a
	// reader holding an offset (from the key index, say) can find its columns. Built once when
	// the payload is published; the alternative is a binary search over seg.offs on every read.
	byOff map[uint32]int
	// cache decompresses the columnar regions once per block rather than per record read.
	cache *blockCache
	// dict is the segment's attribute dictionary when its records are interned, for translating
	// the schema's global ids into this segment's local ones.
	dict *segDictHandle
}

// publishColNative finds a segment's columnar record and parses it, as publishSegDict does for the
// dictionary. A segment without one is whole-record and reads unchanged.
func publishColNative(c *Collection, seg *segment) {
	if seg == nil || seg.colNative.Load() != nil {
		return
	}
	for off := 0; off < seg.used; {
		o := uint32(off)
		total := recTotalLen(seg.data, o)
		if total == 0 {
			break
		}
		if recIsCol(seg.data, o) {
			blob := recAd(seg.data, o)
			// The REGION codec, not the segment's record codec: a block's regions are
			// compressed dictionary-less (see Collection.regionCodec), and decoding them with
			// the segment's trained dictionary fails or, worse, succeeds into nonsense.
			cs := unmarshalColSegment(blob, c.regionCodec(), func(name string) uint32 {
				return c.intern.Intern(name)
			})
			if cs == nil {
				return // unreadable: leave it unpublished, and the segment reads as whole-record
			}
			bc, err := newBlockCache(64 << 20)
			if err != nil {
				return
			}
			cn := &colNative{seg: cs, byOff: make(map[uint32]int, len(cs.offs)), cache: bc}
			for i, ro := range cs.offs {
				cn.byOff[ro] = i
			}
			cn.dict = seg.dict.Load()
			seg.colNative.Store(cn)
			return
		}
		off += int(total)
	}
}

// recordWire returns record `off`'s FULL wire form: its own bytes for a whole-record segment, or
// its remnant spliced with its columnar values for a columnarized one.
//
// This is the one place the two shapes are reconciled. Every reader that wants a whole ad goes
// through it; readers that want a single attribute should ask the columns directly, which is the
// entire point of storing them that way.
func (c *Collection) recordWire(seg *segment, off uint32, buf []byte) ([]byte, error) {
	raw, err := seg.codec.Decompress(buf[:0], recAd(seg.data, off))
	if err != nil {
		return nil, err
	}
	cn := seg.colNative.Load()
	if cn == nil {
		return raw, nil
	}
	k, ok := cn.byOff[off]
	if !ok {
		return raw, nil // not a columnarized record (a marker, or written after the transform)
	}
	return cn.spliceInto(c, raw, k, nil)
}

// spliceInto rebuilds a full ad from a record's remnant and its columnar values.
//
// Appends rather than merges in schema order: a ClassAd is a set of attributes, and every consumer
// here reads it by name or id, so the order the two halves are concatenated in does not change any
// answer. Merging in a canonical order would cost a sort per record for nothing.
func (cn *colNative) spliceInto(c *Collection, remnant []byte, k int, dst []byte) ([]byte, error) {
	blk, local := cn.blockFor(k)
	if blk == nil {
		return remnant, nil
	}
	hdr, entries, n, inline, ok := wire.Ad(remnant).SplitBody()
	if !ok {
		return nil, errBadRemnant
	}
	rec, err := blk.reconstruct(local, cn.cache)
	if err != nil {
		return nil, err
	}
	// The column half, appended in the remnant's own key encoding. A persistent segment keys by
	// NAME and the schema keys by interned id, so the id is resolved on the way out; an in-memory
	// collection shares the id space and the lookup is skipped.
	// The remnant's key space, which is not the schema's. A persistent segment keys by NAME; an
	// INTERNED one keys by an id that means something only against that segment's dictionary. The
	// schema keys by global intern id in both cases, so it is translated on the way out -- writing
	// a global id into an interned record would resolve to whatever attribute happens to hold that
	// local id.
	extra := dst[:0]
	added := 0
	bad := false
	sd := cn.dict
	blk.schema.forEach(rec, func(id uint32, node []byte) bool {
		name := ""
		out := id
		if inline || sd != nil {
			nm, ok := c.intern.Name(id)
			if !ok {
				bad = true
				return false
			}
			name = nm
		}
		if sd != nil {
			lid, ok := sd.lookup(name)
			if !ok {
				bad = true
				return false
			}
			out = lid
		}
		extra = wire.AppendKey(extra, inline, out, name)
		extra = append(extra, node...)
		added++
		return true
	})
	if bad {
		return nil, errBadRemnant
	}
	out := wire.BuildAd(nil, hdr, n+added, entries)
	return append(out, extra...), nil
}

// errBadRemnant marks a columnarized record whose halves cannot be rejoined. Returned rather than
// papered over: half an ad is a wrong answer, and this region is authoritative, so there is no
// correct fallback -- the caller must surface it.
var errBadRemnant = errors.New("collections: columnarized record cannot be reassembled")

// blockFor maps a segment-wide record index to its block and the index within it.
func (cn *colNative) blockFor(k int) (*columnarBlock, int) {
	for _, b := range cn.seg.blocks {
		if k < b.n {
			return b, k
		}
		k -= b.n
	}
	return nil, 0
}

// columnarized reports whether the segment stores its schema'd attributes columnar.
func (s *segment) columnarized() bool { return s != nil && s.colNative.Load() != nil }
