package collections

import (
	"errors"
	"sync"
	"sync/atomic"

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

	// localOf translates the payload schema's GLOBAL intern ids into this segment's local
	// dictionary ids, resolved once when the payload is published.
	//
	// It was a dictionary probe per field per record: reassembling a record walks every field the
	// schema carries, and each one resolved global id -> name -> local id. Measured on 1500 real
	// OSPool ads that was 3.5s of an 11.2s scan -- 31% of the whole scan spent re-deriving a
	// mapping that is fixed for the life of the segment. nil when the records are not interned,
	// where no translation is needed at all.
	localOf map[uint32]uint32
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
			// VERIFY the payload before trusting it. Every other region of a segment is either
			// replaceable (the sidecar rebuilds) or one record's worth of damage; these bytes are
			// the only copy of every schema'd attribute in the segment, so a flipped bit here is a
			// silently wrong value with nothing to fall back on.
			//
			// The record CRC is otherwise checked in exactly one place -- recovery's extent scan,
			// which the recorded-extent shortcut normally skips -- so it functions as a torn-tail
			// detector rather than an integrity check. That is fine for a row record and not fine
			// for this one, which is why it is checked here explicitly rather than relied upon.
			if seg.persistent && !recVerifyCRC(seg.data, o) {
				colNativeCRCFailures.Add(1)
				seg.colDamaged.Store(true)
				return
			}
			blob := recAd(seg.data, o)
			// The REGION codec, not the segment's record codec: a block's regions are
			// compressed dictionary-less (see Collection.regionCodec), and decoding them with
			// the segment's trained dictionary fails or, worse, succeeds into nonsense.
			cs := unmarshalColSegment(blob, c.regionCodec(), func(name string) uint32 {
				return c.intern.Intern(name)
			})
			if cs != nil {
				cs.schemaOnly = true
			}
			if cs == nil {
				colNativeCRCFailures.Add(1)
				seg.colDamaged.Store(true)
				return
			}
			// The collection's ONE shared block cache, not a fresh per-segment cache: a cache
			// per segment made ristretto's fixed admission metadata scale with segment count (a
			// multi-GB reopen leak on a large archive). A nil cache (creation failed) is valid --
			// blockCache methods then decompress every time -- so it never damages the segment.
			cn := &colNative{seg: cs, byOff: make(map[uint32]int, len(cs.offs)), cache: c.sharedColCache()}
			for i, ro := range cs.offs {
				cn.byOff[ro] = i
			}
			cn.dict = seg.dict.Load()
			// The payload's cold tails are keyed by the SEGMENT's dictionary, so readers translate
			// through that dictionary rather than trusting this process's intern numbering.
			// Only for a dictionary-keyed payload. A globally-keyed one already carries its own
			// translation, built from the names the section stores, and overwriting it with a
			// dictionary-derived map would point every cold-tail lookup at the wrong attribute.
			if rm := segIDRemap(c, cn.dict); rm != nil && cs.dictKeyed {
				for _, b := range cs.blocks {
					b.remap = rm
				}
				for _, g := range cs.groups {
					setGroupRemap(g.blocks, rm)
				}
			}
			if cn.dict != nil {
				cn.localOf = make(map[uint32]uint32, 256)
				schemas := make([]*adSchema, 0, len(cs.blocks)+len(cs.groups))
				for _, b := range cs.blocks {
					schemas = append(schemas, b.schema)
				}
				for _, g := range cs.groups {
					// The group schemas too: reassembly translates their fields into the record's id
					// space exactly as it does base fields, and doing that with a dictionary probe per
					// field per record is the cost this cache exists to remove.
					schemas = append(schemas, g.schema)
				}
				for _, sch := range schemas {
					if sch == nil {
						continue
					}
					for _, f := range sch.fields {
						if _, done := cn.localOf[f.id]; done {
							continue
						}
						name, ok := c.intern.Name(f.id)
						if !ok {
							continue
						}
						if lid, ok := cn.dict.lookup(name); ok {
							cn.localOf[f.id] = lid
						}
					}
				}
			}
			seg.colNative.Store(cn)
			// Publish it as the segment's read-path columnar block too. It IS a colSegment, so
			// every existing columnar reader -- the aggregate scans, the presence count, the
			// per-record resolver, the vectorized evaluator -- works against the in-segment copy
			// unchanged. That is what makes the sidecar's copy redundant rather than merely
			// duplicated: the builder skips a segment that already has one (see enableSchemaScan),
			// so nothing rebuilds it and nothing writes it.
			seg.colblk.Store(cs)
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
// colNativeBlob returns a columnarized segment's payload for the sidecar writer, which must NOT
// duplicate it: the segment already holds the authoritative copy, and writing it again is the exact
// waste this format removes.
func colNativeBlobRedundant(seg *segment) bool { return seg != nil && seg.colNative.Load() != nil }

func (c *Collection) recordWire(seg *segment, off uint32, buf []byte) ([]byte, error) {
	return c.recordWireIn(seg, seg.data, off, buf)
}

// recordWireIn is recordWire reading through a caller-supplied view of the segment's bytes.
//
// A scan holds a frozen WINDOW over the segment, and that window is what keeps the mapping alive: a
// sealed segment can be retired and unmapped while a scan still walks it, so reading seg.data
// directly would be a use-after-munmap. The window's slice is passed in instead.
func (c *Collection) recordWireIn(seg *segment, data []byte, off uint32, buf []byte) ([]byte, error) {
	raw, err := seg.codec.Decompress(buf[:0], recAd(data, off))
	if err != nil {
		return nil, err
	}
	cn := seg.colNative.Load()
	if cn == nil {
		// A segment holding a columnar record whose payload could not be trusted. Its records were
		// written WITHOUT the attributes that payload holds, so serving them would return ads
		// missing half their content -- indistinguishable, to a caller, from ads that never had it.
		// Fail instead: a read error is recoverable attention, a silently short ad is not.
		if seg.colDamaged.Load() {
			return nil, errColDamaged
		}
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
	rec, err := blk.reconstructInto(getRecBuf(), local, cn.cache)
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
	blk.schema.forEachKeyed(rec, func(id uint32, node []byte, fromCold bool) bool {
		name := ""
		out := id
		if fromCold && sd == nil {
			// A cold-tail id in a segment with NO dictionary is the id the WRITING process assigned,
			// and this process numbers names differently -- so it has to be mapped back before the
			// name is resolved. Skipping this emitted an attribute under whatever name now holds that
			// number: an ad came back with `owner` twice, once correctly and once carrying another
			// field's integer.
			//
			// An interned segment needs no such step: its cold-tail ids are the segment dictionary's,
			// which is the record's own key space, so they are emitted exactly as stored.
			id = blk.remap.current(id)
		}
		if sd != nil && !fromCold {
			// A SCHEMA field's id belongs to this process and has to be translated into the record's.
			// A COLD TAIL entry is already keyed by the segment dictionary -- translating it too looks
			// up a local id in a global table, misses, and silently drops the whole record.
			lid, ok := cn.localOf[id]
			if !ok {
				bad = true
				return false
			}
			out = lid
		} else if inline {
			nm, ok := c.intern.Name(id)
			if !ok {
				bad = true
				return false
			}
			name = nm
		}
		extra = wire.AppendKey(extra, inline, out, name)
		extra = append(extra, node...)
		added++
		return true
	})
	putRecBuf(rec) // rec's nodes were copied into extra above; done with it
	if bad {
		return nil, errBadRemnant
	}
	// The GROUP columns. A group's attributes are not base schema fields, so the loop above does not
	// see them, and for a record that belongs to a group WHOLLY they are not in the record either --
	// the rewrite took them into that group's column. Without this they would be missing from the
	// reassembled ad entirely.
	//
	// Only whole members. A record holding part of a group was never put in the column and kept its
	// values in the record, so it needs nothing here; adding it would duplicate what the remnant
	// already supplied.
	if len(cn.seg.groups) > 0 {
		var gerr error
		extra, added, gerr = cn.appendGroupValues(c, extra, added, k, inline, sd, nil)
		if gerr != nil {
			return nil, gerr
		}
	}
	out := wire.BuildAd(nil, hdr, n+added, entries)
	return append(out, extra...), nil
}

// errBadRemnant marks a columnarized record whose halves cannot be rejoined. Returned rather than
// papered over: half an ad is a wrong answer, and this region is authoritative, so there is no
// correct fallback -- the caller must surface it.
var errBadRemnant = errors.New("collections: columnarized record cannot be reassembled")

// errColDamaged marks a segment whose columnar payload failed verification. Its records are short
// of every attribute the payload held, so there is no partial answer worth giving.
var errColDamaged = errors.New("collections: segment columnar payload failed verification")

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

// colNativeCRCFailures counts columnar payloads refused for a bad checksum. A nonzero value means a
// segment is missing the attributes its records no longer carry, which is data loss rather than a
// slow path -- so it is counted rather than merely logged, and a test can hold the line that a
// corrupt payload is refused instead of read.
var colNativeCRCFailures atomic.Int64

// ColNativeCRCFailures reports how many columnar payloads have been refused for a bad checksum
// since the process started.
func ColNativeCRCFailures() int64 { return colNativeCRCFailures.Load() }

// columnarizedSegments counts segments successfully rewritten into columnar form, and
// columnarizedBytesSaved the arena bytes the rewrites removed (negative if a rewrite grew a
// segment, which is worth seeing rather than clamping).
var (
	columnarizedSegments   atomic.Int64
	columnarizedBytesSaved atomic.Int64
)

// ColumnarizedSegments reports how many segments have been rewritten into columnar form, and how
// many arena bytes that removed, since the process started.
func ColumnarizedSegments() (segments, bytesSaved int64) {
	return columnarizedSegments.Load(), columnarizedBytesSaved.Load()
}

// Record-form scratch for reassembly.
//
// A pool rather than a buffer on the colNative, because a colNative is shared by every reader of its
// segment and the parallel scan runs several at once -- one buffer there would be a data race that
// shows up as two records' values interleaved. A pool rather than a per-call allocation, because
// this is once per record on every columnar full-ad read, which is exactly the allocation rate that
// made the garbage collector the largest single cost in a columnar scan's profile.
var recBufPool = sync.Pool{New: func() any { b := make([]byte, 0, 4096); return &b }}

// getRecBuf takes a buffer OUT of the pool and does not return it: putRecBuf does that, once the
// caller is finished. Returning it here would hand the same backing array to the next concurrent
// reader while this one still held it, which is the data race a pool is supposed to prevent.
func getRecBuf() []byte {
	return (*recBufPool.Get().(*[]byte))[:0]
}

// putRecBuf returns a buffer to the pool, keeping whatever capacity it grew to so the next record
// reuses it.
func putRecBuf(b []byte) {
	if cap(b) == 0 {
		return
	}
	b = b[:0]
	recBufPool.Put(&b)
}

// appendGroupValues appends the group-column values of record k, for every group the record belongs
// to wholly, in the key encoding the remnant uses.
// want, when non-nil, restricts the output to those global intern ids, so a projected read appends
// only the group attributes it was asked for.
func (cn *colNative) appendGroupValues(c *Collection, dst []byte, added, k int, inline bool, sd *segDictHandle, want map[uint32]struct{}) ([]byte, int, error) {
	gs := getGroupScopes()
	defer putGroupScopes(gs)
	bi, local := cn.blockIndexAndLocal(k)
	if bi < 0 {
		return dst, added, errBadRemnant
	}
	for gi, g := range cn.seg.groups {
		if bi >= len(g.blocks) {
			continue
		}
		gb := g.blocks[bi]
		if gb == nil {
			continue
		}
		idx, member := gb.index(local)
		if !member {
			continue // not in the column: either holds none of the group, or is an exception whose
			// values stayed in the record
		}
		sub := gs.scopeFor(c, cn, gi, gb)
		sub.k, sub.fellBack = idx, false
		for fi := range gb.blk.schema.fields {
			f := &gb.blk.schema.fields[fi]
			if want != nil {
				if _, in := want[f.id]; !in {
					continue
				}
			}
			out := f.id
			name := ""
			if inline {
				nm, ok := c.intern.Name(f.id)
				if !ok {
					return dst, added, errBadRemnant
				}
				name = nm
			}
			if sd != nil {
				lid, ok := cn.localOf[f.id]
				if !ok {
					return dst, added, errBadRemnant
				}
				out = lid
			}
			if testBit(gb.blk.escapeAt(idx), fi) {
				node, found, err := gb.blk.escapedNode(idx, f.id, sub.bc)
				if err != nil {
					return dst, added, err
				}
				if !found {
					continue
				}
				dst = wire.AppendKey(dst, inline, out, name)
				dst = append(dst, node...)
				added++
				continue
			}
			var ok bool
			dst = wire.AppendKey(dst, inline, out, name)
			dst, ok = sub.appendSlotNode(dst, fi, f)
			if !ok || sub.fellBack {
				return dst, added, errBadRemnant
			}
			added++
		}
	}
	return dst, added, nil
}

// groupScopes is a reusable set of per-group sub-scopes for reading group columns.
//
// Pooled rather than kept on the colNative, for the same reason the record buffer is: a colNative is
// shared by every reader of its segment and the parallel scan runs several at once, so a scope hanging
// off it would be two readers writing each other's block binding and record index -- values from the
// wrong record, silently.
type groupScopes struct {
	scopes []*colScope
}

var groupScopePool = sync.Pool{New: func() any { return &groupScopes{} }}

func getGroupScopes() *groupScopes { return groupScopePool.Get().(*groupScopes) }

func putGroupScopes(gs *groupScopes) {
	for _, sub := range gs.scopes {
		if sub != nil {
			sub.blk = nil // drop the reference so a retired block is not pinned by the pool
		}
	}
	groupScopePool.Put(gs)
}

// scopeFor returns the sub-scope for group gi bound to gb's block, mirroring colScope.groupScope: a
// group column is read through exactly the same readers as a base column rather than a parallel copy
// that could diverge from them.
func (gs *groupScopes) scopeFor(c *Collection, cn *colNative, gi int, gb *colGroupBlock) *colScope {
	for len(gs.scopes) <= gi {
		gs.scopes = append(gs.scopes, nil)
	}
	sub := gs.scopes[gi]
	if sub == nil {
		sub = &colScope{bc: cn.cache, c: c}
		gs.scopes[gi] = sub
	}
	sub.bc = cn.cache
	if sub.blk != gb.blk {
		sub.setBlock(gb.blk)
	}
	return sub
}

// blockIndexAndLocal maps a segment-wide record index to its block position and index within it.
func (cn *colNative) blockIndexAndLocal(k int) (int, int) {
	for i, b := range cn.seg.blocks {
		if k < b.n {
			return i, k
		}
		k -= b.n
	}
	return -1, 0
}

// segIDMapper returns a global-intern-id -> segment-dictionary-id function, or nil when the segment
// has no dictionary and therefore no durable id space of its own.
//
// This is what lets a columnar payload survive a restart without storing a name table beside it: the
// segment already carries a dictionary naming every attribute its records use, so keying the payload
// by that dictionary makes both sides of a restart agree by construction rather than by luck.
func (c *Collection) segIDMapper(d *segDictHandle) func(uint32) (uint32, bool) {
	if d == nil {
		return nil
	}
	return func(id uint32) (uint32, bool) {
		name, ok := c.intern.Name(id)
		if !ok {
			return 0, false
		}
		return d.lookup(name)
	}
}

// segIDRemap rebuilds a reader's translation between this process's intern ids and the segment
// dictionary's, from the dictionary alone.
func segIDRemap(c *Collection, d *segDictHandle) *idRemap {
	if d == nil {
		return nil
	}
	n := d.count()
	r := &idRemap{toStored: make(map[uint32]uint32, n), toCurrent: make(map[uint32]uint32, n)}
	for lid := uint32(0); lid < n; lid++ {
		nm := d.name(lid)
		if nm == nil {
			continue
		}
		gid := c.intern.Intern(string(nm))
		r.toStored[gid] = lid
		r.toCurrent[lid] = gid
	}
	return r
}
