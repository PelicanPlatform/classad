package wire

import "encoding/binary"

// RE-INTERNING WITHOUT AN AST.
//
// Compaction rewrites every live record from the inline-names form the write path produces into
// the interned form a sealed segment stores, and it did that by decoding each record into an
// ast.ClassAd and encoding the ast back. On a real compaction pass that decode was 71% of the
// allocation -- 35% in readString alone, which copies every string value of every record into a
// fresh Go string only for the encoder to write it straight back out.
//
// None of that is needed. The two forms differ in exactly two places:
//
//	an attribute KEY   uvarint(nameLen) + name   becomes   uvarint(internID)
//	a name-carrying NODE  nAttrRefStr/nFuncStr/nSelectStr  becomes  nAttrRef/nFunc/nSelect
//
// Everything else -- every literal, every operator, every list -- is the same bytes in both forms.
// So the transcode copies, and only rewrites where the encoding genuinely differs.
//
// A sealed value is the exception it cannot handle. Its plaintext is the value's node encoding in
// the key form of the ad it was sealed in, so a record literal sealed by the inline-names write
// path carries its member names inline, and a reader of the interned ad opens it expecting ids:
// the name's length reads as an id, the name's bytes as a node, and the whole record fails to
// decode. Which sealed values hold a record cannot be known without opening them, and this layer
// holds no key, so a record with any sealed value is refused. The caller's fallback decodes with
// the key and re-encodes interned with the sealer, sealing each value afresh in the interned form;
// the cost is only that such records do not get the transcode's speedup.
//
// An id-keyed node (nAttrRef/nFunc/nSelect) inside an inline-names ad is refused as malformed. The
// inline encoder never writes one, and its id indexes no table this ad carries; copied into the
// interned output it would index the NEW table and name whatever attribute holds that id there.

// AppendInternedFromInline appends to dst the INTERNED form of src, an inline-names ad, and
// returns the extended buffer. Names are interned into t; ids present in hot get a hot-header
// entry, matching EncodeWithHotEnc.
//
// It reports false when src is not a plain inline-names ad -- standalone (carrying its own intern
// table) or already interned -- when it carries a sealed value (see the note at the top of this
// file), or when it is malformed. The caller falls back to decoding, which is the source of truth.
func AppendInternedFromInline(dst []byte, src Ad, t *InternTable, hot map[uint32]struct{}) ([]byte, bool) {
	c, ok := src.bodyStart()
	if !ok || !c.inline || src[2]&flagStandalone != 0 {
		return dst, false
	}
	hotCount := c.uvarint()
	for i := uint64(0); i < hotCount && c.ok; i++ {
		c.uvarint()
		c.uvarint()
	}
	n := c.uvarint()
	if !c.ok {
		return dst, false
	}

	entries := getScratch()
	defer func() { putScratch(entries) }()
	var hots []hotPair
	for i := uint64(0); i < n && c.ok; i++ {
		name := c.readNameBytes()
		if !c.ok {
			return dst, false
		}
		id := t.InternBytes(name)
		entries = binary.AppendUvarint(entries, uint64(id))
		nodeOff := len(entries)
		if !transcodeNode(c, &entries, t, 0) {
			return dst, false
		}
		if _, isHot := hot[id]; isHot {
			hots = append(hots, hotPair{id, uint32(nodeOff)})
		}
	}
	if !c.ok {
		return dst, false
	}
	return frame(dst, 0 /* interned: no flags */, hots, int(n), entries), true
}

// transcodeNode copies one node from c to out, rewriting the inline-name variants into their
// interned forms and leaving everything else byte-identical. It mirrors skipNode's grammar case
// for case; a tag handled there and missed here would silently truncate a record, so the two are
// meant to be read side by side.
func transcodeNode(c *cursor, out *[]byte, t *InternTable, depth int) bool {
	if !c.ok || depth > maxDepth {
		c.ok = false
		return false
	}
	start := c.pos
	tag := c.byteAt()
	// copyFrom emits the bytes from start through the cursor's current position: the tag and a
	// payload that needs no rewriting.
	copyFrom := func() bool {
		if !c.ok {
			return false
		}
		*out = append(*out, c.b[start:c.pos]...)
		return true
	}

	switch tag {
	case nUndefined, nError, nBoolFalse, nBoolTrue:
		return copyFrom()
	case nInt:
		_, w := binary.Varint(c.b[c.pos:])
		if w <= 0 {
			c.ok = false
			return false
		}
		c.pos += w
		return copyFrom()
	case nReal:
		c.skip(8)
		return copyFrom()
	case nString:
		c.skip(int(c.uvarint()))
		return copyFrom()
	case nEncrypted:
		// Refused, not malformed: the plaintext may hold inline names that cannot be rewritten
		// without opening it. See the note at the top of this file.
		return false
	case nAttrRef, nFunc, nSelect:
		// An id with no table behind it: see the note at the top of this file.
		c.ok = false
		return false

	case nAttrRefStr:
		// scope byte + inline name -> scope byte + id.
		scope := c.byteAt()
		name := c.readNameBytes()
		if !c.ok {
			return false
		}
		*out = append(*out, nAttrRef, scope)
		*out = binary.AppendUvarint(*out, uint64(t.InternBytes(name)))
		return true

	case nBinOp:
		c.skip(1) // op id
		if !copyFrom() {
			return false
		}
		return transcodeNode(c, out, t, depth+1) && transcodeNode(c, out, t, depth+1)
	case nBinOpStr:
		c.skip(int(c.uvarint())) // op string, kept as-is
		if !copyFrom() {
			return false
		}
		return transcodeNode(c, out, t, depth+1) && transcodeNode(c, out, t, depth+1)
	case nUnOp:
		c.skip(1)
		if !copyFrom() {
			return false
		}
		return transcodeNode(c, out, t, depth+1)
	case nUnOpStr:
		c.skip(int(c.uvarint()))
		if !copyFrom() {
			return false
		}
		return transcodeNode(c, out, t, depth+1)
	case nList:
		n := c.uvarint()
		if !copyFrom() {
			return false
		}
		for i := uint64(0); i < n; i++ {
			if !transcodeNode(c, out, t, depth+1) {
				return false
			}
		}
		return true
	case nRecord:
		// byteAt already consumed the tag, so there is nothing to skip here.
		*out = append(*out, nRecord)
		return transcodeAdBody(c, out, t, depth+1)

	case nFuncStr:
		name := c.readNameBytes()
		if !c.ok {
			return false
		}
		n := c.uvarint()
		if !c.ok {
			return false
		}
		*out = append(*out, nFunc)
		*out = binary.AppendUvarint(*out, uint64(t.InternBytes(name)))
		*out = binary.AppendUvarint(*out, n)
		for i := uint64(0); i < n; i++ {
			if !transcodeNode(c, out, t, depth+1) {
				return false
			}
		}
		return true

	case nCond:
		if !copyFrom() {
			return false
		}
		return transcodeNode(c, out, t, depth+1) &&
			transcodeNode(c, out, t, depth+1) &&
			transcodeNode(c, out, t, depth+1)
	case nElvis, nSubscript:
		if !copyFrom() {
			return false
		}
		return transcodeNode(c, out, t, depth+1) && transcodeNode(c, out, t, depth+1)
	case nSelectStr:
		// node(record) + inline name -> node(record) + id. The TAG has to be written before the
		// record node, so it cannot share the copyFrom path.
		*out = append(*out, nSelect)
		if !transcodeNode(c, out, t, depth+1) {
			return false
		}
		name := c.readNameBytes()
		if !c.ok {
			return false
		}
		*out = binary.AppendUvarint(*out, uint64(t.InternBytes(name)))
		return true
	case nParen:
		if !copyFrom() {
			return false
		}
		return transcodeNode(c, out, t, depth+1)

	default:
		c.ok = false
		return false
	}
}

// transcodeAdBody is transcodeNode for a nested record literal: the same key rewrite as the
// top-level ad, with no hot header of its own (nested bodies never carry one).
func transcodeAdBody(c *cursor, out *[]byte, t *InternTable, depth int) bool {
	if !c.ok || depth > maxDepth {
		c.ok = false
		return false
	}
	hotCount := c.uvarint()
	for i := uint64(0); i < hotCount && c.ok; i++ {
		c.uvarint()
		c.uvarint()
	}
	n := c.uvarint()
	if !c.ok {
		return false
	}
	*out = binary.AppendUvarint(*out, 0) // hot header: empty
	*out = binary.AppendUvarint(*out, n)
	for i := uint64(0); i < n; i++ {
		name := c.readNameBytes()
		if !c.ok {
			return false
		}
		*out = binary.AppendUvarint(*out, uint64(t.InternBytes(name)))
		if !transcodeNode(c, out, t, depth+1) {
			return false
		}
	}
	return true
}
