package collections

import (
	"encoding/binary"
	"hash/crc32"
	"math"
	"sort"
)

// Serialization of a sealed segment's columnar accelerator (schema + block streams + the
// arena-offset map), so it can be persisted beside the segment's index sidecar and rebuilt on
// reopen instead of re-transcoding the whole segment. The layout (field offsets, hot/cold
// partition) is recomputed from the persisted (schema, hot set, n) via layoutSchema /
// layoutColumnar -- the same helpers the encoder uses -- so a decoded block is identical to a
// freshly built one. Only the payloads and offsets are stored.
//
// The marshaled block is framed with a magic/version/upto/CRC header before it goes into the
// sidecar's columnar section, so a reload rejects a truncated, bit-rotted, or stale section (one
// built from a different segment byte length) and rebuilds instead of trusting corrupt bytes --
// the same "derived state, any doubt rebuilds" contract the attribute-index sidecar follows.
const (
	colSectionMagic = 0x434f4c58 // "COLX"
	// colSectionVersion history. v1 stored exactly one block covering the whole segment; v2 stores a
	// LIST of row-group blocks (see colGroupRows); v3 compresses the block's regions with the
	// dictionary-less base codec instead of the segment's dictionary codec (see
	// Collection.regionCodec). An older section is rejected by readColSection like any other section
	// that cannot be trusted, so the segment rebuilds its accelerator under the current rules --
	// derived state, so no migration is needed. v3 in particular MUST be a version bump rather than
	// a silent change: a v2 section's regions were compressed with a trained dictionary, so decoding
	// them with the base codec would fail or, worse, be attempted against whatever dictionary the
	// segment currently carries. v4 adds each block's per-field numeric zones.
	//
	// The zones are persisted rather than recomputed on reload because computing them costs a full
	// pass over every column -- exactly the work they exist to skip -- so a reopened table would lose
	// block pruning until something rebuilt its blocks. A missing zone is SAFE (mayMatch never rules
	// a block out without one), so this is a performance property, which is precisely the kind that
	// disappears quietly.
	//
	// v5 is one layout bump carrying two changes, because a sidecar bump forces every deployed table to
	// rebuild its accelerator and doing that twice for two halves of the same idea is wasteful. First, the
	// single uncompressed region splits in two: the per-record escape/bool bitsets, and the hot numerics
	// COLUMNAR rather than row-major. Second, a low-cardinality string field is stored as a dictionary
	// plus a columnar code column instead of a positional per-record region.
	//
	// Neither can be read as v4 and both would produce plausible values rather than an error if
	// misinterpreted, so this must be a version bump. As with every bump here the section is derived
	// state: an old one is rejected and the segment rebuilds under the current rules.
	//
	// v6 appends the GROUP SCHEMAS and their per-block selections. A v5 reader would stop at the
	// arena-offset map and silently produce a segment with no groups -- correct answers, but every
	// group column read the slow way -- so this is a bump rather than an optional trailer. It also
	// adds each block's per-field escape classes, which tell a MISSING escape from an EXCEPTIONAL
	// one without a cold-tail lookup (see colescclass.go). Both ride the same bump deliberately: a
	// section bump forces every deployed table to rebuild its accelerator, and doing that twice for
	// two changes that land together is waste.
	colSectionVersion = 6
	colSectionHdr     = 4 + 2 + 4 + 4 // magic u32 | version u16 | upto u32 | crc u32
)

// wrapColSection frames a marshaled columnar block for the sidecar. upto is the segment byte
// length the block was built from.
func wrapColSection(body []byte, upto int) []byte {
	b := make([]byte, 0, colSectionHdr+len(body))
	b = appendU32(b, colSectionMagic)
	b = appendU16(b, colSectionVersion)
	b = appendU32(b, uint32(upto))
	b = appendU32(b, crc32.ChecksumIEEE(body))
	return append(b, body...)
}

// readColSection validates a framed columnar section against the segment's current byte length and
// the body CRC, returning the inner marshaled block bytes, or nil if the section is
// absent/short/wrong-version/stale/corrupt (the caller then row-scans and rebuilds).
func readColSection(data []byte, upto int) []byte {
	if len(data) < colSectionHdr ||
		binary.LittleEndian.Uint32(data[0:]) != colSectionMagic ||
		binary.LittleEndian.Uint16(data[4:]) != colSectionVersion ||
		int(binary.LittleEndian.Uint32(data[6:])) != upto {
		return nil
	}
	body := data[colSectionHdr:]
	if binary.LittleEndian.Uint32(data[10:]) != crc32.ChecksumIEEE(body) {
		return nil
	}
	return body
}

// readColSectionSchemaOnly validates a framed section WITHOUT checking its version, returning the body so a
// caller can recover just the leading schema from a section whose BLOCK format it cannot read.
//
// The version check exists because block payloads change; the schema prefix has not. Rejecting the whole
// section on a version bump therefore threw away a schema that was still perfectly readable -- and since the
// derived schema is only recovered from a loaded section, that turned every format bump into "the columnar
// accelerator is off" rather than "the blocks rebuild". Magic, extent and CRC are still checked, so this
// never decodes bytes that are not a section this store wrote.
func readColSectionSchemaOnly(data []byte, upto int) []byte {
	if len(data) < colSectionHdr ||
		binary.LittleEndian.Uint32(data[0:]) != colSectionMagic ||
		int(binary.LittleEndian.Uint32(data[6:])) != upto {
		return nil
	}
	body := data[colSectionHdr:]
	if binary.LittleEndian.Uint32(data[10:]) != crc32.ChecksumIEEE(body) {
		return nil
	}
	return body
}

// unmarshalColSchemaOnly decodes just the schema and hot set from a section body, ignoring everything after.
// Returns nil on anything unexpected, so an older layout that happens to differ recovers nothing rather than
// something wrong.
func unmarshalColSchemaOnly(data []byte, internName func(string) uint32) (*adSchema, []int) {
	c := &cursor{b: data}
	s := readAdSchema(c, internName)
	if s == nil {
		return nil, nil
	}
	hn := int(c.u32())
	if hn < 0 || hn > len(s.fields) || c.err != nil {
		return nil, nil
	}
	hot := make([]int, hn)
	for i := range hot {
		hot[i] = int(c.u32())
	}
	if c.err != nil {
		return nil, nil
	}
	return s, hot
}

// marshalAdSchema writes a schema's fields as (name, kind, width, unsigned); the layout is
// re-derived on read, not stored. Fields are keyed by NAME, not by their runtime intern id: a
// persistent collection's global intern ids are assigned in first-seen order and differ across
// reopen, so a stored id would resolve to the wrong attribute on the next Open. nameOf resolves a
// field's current id to its name (c.intern.Name); readAdSchema re-resolves the name to whatever id
// the reopened table assigns.
func marshalAdSchema(dst []byte, s *adSchema, nameOf func(uint32) (string, bool)) []byte {
	dst = appendU32(dst, uint32(len(s.fields)))
	for _, f := range s.fields {
		name, _ := nameOf(f.id) // "" only if the id is unknown, which cannot happen for a schema field
		dst = appendBytes(dst, []byte(name))
		dst = appendU16(dst, uint16(f.kind))
		dst = appendU16(dst, uint16(f.width))
		u := uint16(0)
		if f.unsigned {
			u = 1
		}
		dst = appendU16(dst, u)
	}
	return dst
}

// readAdSchema reads a name-keyed schema and re-binds each field to the current intern id via
// internName (c.intern.Intern). Names are collected before any interning so a torn/corrupt read
// (c.err) is rejected without polluting the intern table with junk names.
func readAdSchema(c *cursor, internName func(string) uint32) *adSchema {
	n := int(c.u32())
	if n < 0 || n > 1<<20 || !c.need(0) {
		return nil
	}
	type raw struct {
		name     string
		kind     adKind
		width    int
		unsigned bool
	}
	raws := make([]raw, 0, n)
	for i := 0; i < n; i++ {
		name := string(c.bytes())
		raws = append(raws, raw{name, adKind(c.u16()), int(c.u16()), c.u16() != 0})
	}
	if c.err != nil {
		return nil
	}
	fields := make([]adField, n)
	for i, r := range raws {
		fields[i] = adField{id: internName(r.name), kind: r.kind, width: r.width, unsigned: r.unsigned}
	}
	return layoutSchema(fields)
}

// marshalColSegment serializes cs: the schema and hot set ONCE (every row group in a segment shares
// them, so storing them per group would multiply a ~kB blob by the group count), then each block's
// record count, byte streams and per-record offsets, then offs -- the segment-wide record->arena map
// for MVCC visibility. Returns nil for a colSegment carrying no schema, which cannot be reloaded.
func marshalColSegment(cs *colSegment, nameOf func(uint32) (string, bool)) []byte {
	sch := cs.schema()
	if sch == nil {
		return nil
	}
	hotNum := cs.hotNum()
	dst := marshalAdSchema(nil, sch, nameOf)
	dst = appendU32(dst, uint32(len(hotNum)))
	for _, i := range hotNum {
		dst = appendU32(dst, uint32(i))
	}
	dst = appendU32(dst, uint32(len(cs.blocks)))
	for _, b := range cs.blocks {
		dst = appendColBlock(dst, b)
	}
	dst = appendU32(dst, uint32(len(cs.offs)))
	for _, o := range cs.offs {
		dst = appendU32(dst, o)
	}
	// Group schemas and their selections, after the base blocks so a reader that has already
	// validated the base segment can reject a bad group section on its own.
	dst = appendU32(dst, uint32(len(cs.groups)))
	for _, g := range cs.groups {
		// The schema alone: a group's membership ids ARE its schema's field ids (see
		// deriveGroupSchemas), so storing them twice could only introduce a disagreement.
		dst = marshalAdSchema(dst, g.schema, nameOf)
		dst = appendU32(dst, uint32(len(g.blocks)))
		for _, gb := range g.blocks {
			dst = appendBytes(dst, gb.members)
			dst = appendU32(dst, uint32(len(gb.exceptions)))
			for _, e := range gb.exceptions {
				dst = appendU32(dst, e)
			}
			// rank is derived from members, so recomputing it on load is cheaper than storing
			// it and cannot disagree with the bitmap it indexes.
			if gb.blk == nil {
				dst = appendU32(dst, 0)
				continue
			}
			dst = appendU32(dst, 1)
			dst = appendColBlock(dst, gb.blk)
		}
	}
	return dst
}

// appendColBlock serializes one block's payload. Shared by the base blocks and the group blocks so
// the two cannot drift apart -- a group block is a columnarBlock, and any field added here has to
// reach both or a reloaded group column would be silently short.
func appendColBlock(dst []byte, b *columnarBlock) []byte {
	dst = appendU32(dst, uint32(b.n))
	dst = appendBytes(dst, b.bits)
	dst = appendBytes(dst, b.hotCol)
	dst = appendBytes(dst, b.coldNumComp)
	dst = appendBytes(dst, b.strComp)
	dst = appendBytes(dst, b.coldComp)
	dst = appendU32(dst, uint32(len(b.strOff)))
	for _, v := range b.strOff {
		dst = appendU32(dst, uint32(v))
	}
	dst = appendU32(dst, uint32(len(b.coldOff)))
	for _, v := range b.coldOff {
		dst = appendU32(dst, uint32(v))
	}
	dst = appendU32(dst, uint32(len(b.zones)))
	for idx, z := range b.zones {
		dst = appendU32(dst, uint32(idx))
		dst = appendU64(dst, math.Float64bits(z.Min))
		dst = appendU64(dst, math.Float64bits(z.Max))
		esc := uint32(0)
		if z.escaped {
			esc = 1
		}
		dst = appendU32(dst, esc)
	}
	// String dictionaries: the two compressed regions, then where each field lives in them. Written in
	// sorted field order so the sidecar is reproducible; a map's iteration order would make the same
	// segment serialize to different bytes on different runs.
	// Per-field escape classes, then the exceptional-record lists of whatever fields are mixed.
	// Sorted field order so the sidecar is reproducible.
	dst = appendBytes(dst, b.escClass)
	dst = appendU32(dst, uint32(len(b.escExcRecs)))
	excIdxs := make([]int, 0, len(b.escExcRecs))
	for idx := range b.escExcRecs {
		excIdxs = append(excIdxs, idx)
	}
	sort.Ints(excIdxs)
	for _, idx := range excIdxs {
		dst = appendU32(dst, uint32(idx))
		dst = appendU32(dst, uint32(len(b.escExcRecs[idx])))
		for _, k := range b.escExcRecs[idx] {
			dst = appendU32(dst, k)
		}
	}
	dst = appendBytes(dst, b.strDictComp)
	dst = appendBytes(dst, b.strCodeComp)
	dst = appendU32(dst, uint32(len(b.strDict)))
	dictIdxs := make([]int, 0, len(b.strDict))
	for idx := range b.strDict {
		dictIdxs = append(dictIdxs, idx)
	}
	sort.Ints(dictIdxs)
	for _, idx := range dictIdxs {
		info := b.strDict[idx]
		dst = appendU32(dst, uint32(idx))
		dst = appendU32(dst, uint32(info.codeStart))
		dst = appendU32(dst, uint32(info.codeWidth))
		dst = appendU32(dst, uint32(info.dictStart))
		dst = appendU32(dst, uint32(info.count))
		ne := uint32(0)
		if info.noEscape {
			ne = 1
		}
		dst = appendU32(dst, ne)
	}

	return dst
}

// readColBlock reads one block payload written by appendColBlock, returning nil on malformed data.
// The byte streams ALIAS the cursor's buffer (see unmarshalColSegment's lifetime contract).
func readColBlock(c *cursor, s *adSchema, hotNum []int, codec Codec) *columnarBlock {
	n := int(c.u32())
	if n < 0 || c.err != nil {
		return nil
	}
	b := &columnarBlock{id: colBlockSeq.Add(1), schema: s, codec: codec, n: n}
	// Both alias data (the mmap): read-only, scanned in place. The hot numerics being columnar means a
	// whole column is one contiguous span OF THE MMAP, which is what a vector load needs.
	b.bits = c.bytes()
	b.hotCol = c.bytes()
	b.coldNumComp = c.bytes()
	b.strComp = c.bytes()
	b.coldComp = c.bytes()
	b.strOff = readInts(c)
	b.coldOff = readInts(c)
	if nz := int(c.u32()); nz > 0 {
		if nz > len(s.fields) || !c.need(0) {
			return nil
		}
		b.zones = make(map[int]blockZone, nz)
		for j := 0; j < nz; j++ {
			idx := int(c.u32())
			z := blockZone{zoneRange: zoneRange{
				Min: math.Float64frombits(c.u64()),
				Max: math.Float64frombits(c.u64()),
			}}
			z.escaped = c.u32() != 0
			if idx < 0 || idx >= len(s.fields) {
				return nil
			}
			b.zones[idx] = z
		}
	}
	b.escClass = c.bytes()
	if len(b.escClass) != 0 && len(b.escClass) != len(s.fields) {
		return nil // a class per field, or none at all
	}
	if nx := int(c.u32()); nx > 0 {
		if nx > len(s.fields) || !c.need(0) {
			return nil
		}
		b.escExcRecs = make(map[int][]uint32, nx)
		for j := 0; j < nx; j++ {
			idx := int(c.u32())
			cnt := int(c.u32())
			if idx < 0 || idx >= len(s.fields) || cnt < 0 || !c.need(0) {
				return nil
			}
			recs := make([]uint32, 0, cnt)
			for r := 0; r < cnt; r++ {
				k := c.u32()
				if int(k) >= n {
					return nil // an exceptional record outside the block
				}
				recs = append(recs, k)
			}
			b.escExcRecs[idx] = recs
		}
	}
	b.strDictComp = c.bytes()
	b.strCodeComp = c.bytes()
	if nd := int(c.u32()); nd > 0 {
		if nd > len(s.fields) || !c.need(0) {
			return nil
		}
		b.strDict = make(map[int]strDictField, nd)
		for j := 0; j < nd; j++ {
			idx := int(c.u32())
			info := strDictField{
				codeStart: int(c.u32()),
				codeWidth: int(c.u32()),
				dictStart: int(c.u32()),
				count:     int(c.u32()),
			}
			info.noEscape = c.u32() != 0
			if idx < 0 || idx >= len(s.fields) || (info.codeWidth != 1 && info.codeWidth != 2) {
				return nil
			}
			b.strDict[idx] = info
		}
	}
	if c.err != nil {
		return nil
	}
	layoutColumnar(b, s, hotNum, n)
	return b
}

// unmarshalColSegment reconstructs a colSegment from marshalColSegment's output, attaching the
// segment's codec (for later decompression). Returns nil on malformed data.
//
// ZERO-COPY: the block's byte streams (the two uncompressed regions + the three compressed cold streams)
// ALIAS data rather than being copied -- a hot column is then one contiguous span of the mmap, and the cold
// streams decompress lazily into the bounded block cache only when touched, so a reloaded block
// adds no per-segment stream heap. This is the same lifetime contract as the mmap'd attribute
// index (mmapSegIndex): both alias the segment's <seg>.idx mapping, which is released together on
// reap and re-published together when reindex swaps the sidecar. Callers must therefore keep data
// (the mapping, or a heap buffer in tests) alive for the block's lifetime.
func unmarshalColSegment(data []byte, codec Codec, internName func(string) uint32) *colSegment {
	c := &cursor{b: data}
	s := readAdSchema(c, internName)
	if s == nil {
		return nil
	}
	hn := int(c.u32())
	if hn < 0 || hn > len(s.fields) {
		return nil
	}
	hotNum := make([]int, hn)
	for i := range hotNum {
		hotNum[i] = int(c.u32())
	}
	nb := int(c.u32())
	if nb < 0 || !c.need(0) {
		return nil
	}
	blocks := make([]*columnarBlock, 0, nb)
	total := 0
	for i := 0; i < nb; i++ {
		b := readColBlock(c, s, hotNum, codec)
		if b == nil {
			return nil
		}
		blocks = append(blocks, b)
		total += b.n
	}
	offs := readU32s(c)
	if c.err != nil {
		return nil
	}
	// The blocks' record counts must sum to the offs length, or a scan would map a record to the
	// wrong arena offset and read another record's MVCC seq -- a wrong answer rather than a slow
	// one. Reject instead, and let the segment rebuild.
	if total != len(offs) {
		return nil
	}
	cs := &colSegment{blocks: blocks, offs: offs}
	ng := int(c.u32())
	if c.err != nil || ng < 0 {
		return nil
	}
	for i := 0; i < ng; i++ {
		g := &colGroup{}
		if g.schema = readAdSchema(c, internName); g.schema == nil {
			return nil
		}
		for _, f := range g.schema.fields {
			g.ids = append(g.ids, f.id)
		}
		sort.Slice(g.ids, func(a, b int) bool { return g.ids[a] < g.ids[b] })
		nbl := int(c.u32())
		if nbl != len(blocks) {
			// One selection per base block, or a membership bitmap would be indexed against
			// the wrong records. Reject and rebuild rather than read the wrong values.
			return nil
		}
		for j := 0; j < nbl; j++ {
			gb := &colGroupBlock{members: c.bytes()}
			ne := int(c.u32())
			if ne < 0 || !c.need(0) {
				return nil
			}
			for e := 0; e < ne; e++ {
				idx := c.u32()
				if int(idx) >= blocks[j].n {
					return nil
				}
				gb.exceptions = append(gb.exceptions, idx)
			}
			if len(gb.members) != (blocks[j].n+7)/8 {
				return nil
			}
			if c.u32() != 0 {
				if gb.blk = readColBlock(c, g.schema, nil, codec); gb.blk == nil {
					return nil
				}
			}
			gb.buildRank(blocks[j].n)
			// The bitmap's population must equal the group block's record count, or a rank
			// would address past the block's end (or short of it) and read another record's
			// values -- silently, since both are well-formed columns.
			if pop := gb.population(); pop != gb.memberCount() {
				return nil
			}
			g.blocks = append(g.blocks, gb)
		}
		cs.groups = append(cs.groups, g)
	}
	if c.err != nil {
		return nil
	}
	return cs
}

func readInts(c *cursor) []int {
	n := int(c.u32())
	if n < 0 || !c.need(0) {
		return nil
	}
	out := make([]int, n)
	for i := range out {
		out[i] = int(c.u32())
	}
	return out
}

func readU32s(c *cursor) []uint32 {
	n := int(c.u32())
	if n < 0 || !c.need(0) {
		return nil
	}
	out := make([]uint32, n)
	for i := range out {
		out[i] = c.u32()
	}
	return out
}
