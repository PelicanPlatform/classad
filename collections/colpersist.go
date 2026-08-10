package collections

import (
	"encoding/binary"
	"hash/crc32"
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
	// colSectionVersion 2 stores a LIST of row-group blocks per segment (see colGroupRows); v1
	// stored exactly one block covering the whole segment. A v1 section is rejected by
	// readColSection like any other section that cannot be trusted, so the segment simply rebuilds
	// its accelerator under the current layout -- derived state, so no migration is needed.
	colSectionVersion = 2
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
		dst = appendU32(dst, uint32(b.n))
		dst = appendBytes(dst, b.hot)
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
	}
	dst = appendU32(dst, uint32(len(cs.offs)))
	for _, o := range cs.offs {
		dst = appendU32(dst, o)
	}
	return dst
}

// unmarshalColSegment reconstructs a colSegment from marshalColSegment's output, attaching the
// segment's codec (for later decompression). Returns nil on malformed data.
//
// ZERO-COPY: the block's byte streams (hot + the three compressed cold streams) ALIAS data rather
// than being copied -- the hot column is then scanned strided directly over the mmap, and the cold
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
		n := int(c.u32())
		if n < 0 || c.err != nil {
			return nil
		}
		b := &columnarBlock{id: colBlockSeq.Add(1), schema: s, codec: codec, n: n}
		b.hot = c.bytes() // aliases data (the mmap); read-only, scanned in place
		b.coldNumComp = c.bytes()
		b.strComp = c.bytes()
		b.coldComp = c.bytes()
		b.strOff = readInts(c)
		b.coldOff = readInts(c)
		if c.err != nil {
			return nil
		}
		layoutColumnar(b, s, hotNum, n)
		blocks = append(blocks, b)
		total += n
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
	return &colSegment{blocks: blocks, offs: offs}
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
