package collections

// Serialization of a sealed segment's columnar accelerator (schema + block streams + the
// arena-offset map), so it can be persisted beside the segment's index sidecar and rebuilt on
// reopen instead of re-transcoding the whole segment. The layout (field offsets, hot/cold
// partition) is recomputed from the persisted (schema, hot set, n) via layoutSchema /
// layoutColumnar -- the same helpers the encoder uses -- so a decoded block is identical to a
// freshly built one. Only the payloads and offsets are stored.

// marshalAdSchema writes a schema's fields as (id, kind, width, unsigned); the layout is
// re-derived on read, not stored.
func marshalAdSchema(dst []byte, s *adSchema) []byte {
	dst = appendU32(dst, uint32(len(s.fields)))
	for _, f := range s.fields {
		dst = appendU32(dst, f.id)
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

func readAdSchema(c *cursor) *adSchema {
	n := int(c.u32())
	if n < 0 || n > 1<<20 || !c.need(0) {
		return nil
	}
	fields := make([]adField, 0, n)
	for i := 0; i < n; i++ {
		fields = append(fields, adField{
			id:       c.u32(),
			kind:     adKind(c.u16()),
			width:    int(c.u16()),
			unsigned: c.u16() != 0,
		})
	}
	if c.err != nil {
		return nil
	}
	return layoutSchema(fields)
}

// marshalColSegment serializes cs (its block's schema, hot set, record count, byte streams, and
// per-record offsets, plus offs -- the block->arena map for MVCC visibility).
func marshalColSegment(cs *colSegment) []byte {
	b := cs.block
	dst := marshalAdSchema(nil, b.schema)
	dst = appendU32(dst, uint32(len(b.hotNum)))
	for _, i := range b.hotNum {
		dst = appendU32(dst, uint32(i))
	}
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
	dst = appendU32(dst, uint32(len(cs.offs)))
	for _, o := range cs.offs {
		dst = appendU32(dst, o)
	}
	return dst
}

// unmarshalColSegment reconstructs a colSegment from marshalColSegment's output, attaching the
// segment's codec (for later decompression). Returns nil on malformed data.
func unmarshalColSegment(data []byte, codec Codec) *colSegment {
	c := &cursor{b: data}
	s := readAdSchema(c)
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
	n := int(c.u32())
	b := &columnarBlock{id: colBlockSeq.Add(1), schema: s, codec: codec, n: n}
	b.hot = append([]byte(nil), c.bytes()...)
	b.coldNumComp = append([]byte(nil), c.bytes()...)
	b.strComp = append([]byte(nil), c.bytes()...)
	b.coldComp = append([]byte(nil), c.bytes()...)
	b.strOff = readInts(c)
	b.coldOff = readInts(c)
	offs := readU32s(c)
	if c.err != nil || n < 0 {
		return nil
	}
	layoutColumnar(b, s, hotNum, n)
	return &colSegment{block: b, offs: offs}
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
