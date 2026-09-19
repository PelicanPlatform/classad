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
// Everything else -- every literal, every operator, every list, every sealed value -- is the same
// bytes in both forms. So the transcode copies, and only rewrites where the encoding genuinely
// differs.
//
// It never opens a sealed value, which is not just a saving: recordToInternedDict decodes with the
// collection's key and must remember to re-seal on the way out, and the one time it did not, the
// secret reached the derived schema and the on-disk sidecar in the clear. A transcode copies the
// nEncrypted node's bytes without being able to read them, so that class of bug cannot occur.

// AppendInternedFromInline appends to dst the INTERNED form of src, an inline-names ad, and
// returns the extended buffer. Names are interned into t; ids present in hot get a hot-header
// entry, matching EncodeWithHotEnc (a sealed value is never hot).
//
// It reports false when src is not a plain inline-names ad -- standalone (carrying its own intern
// table) or already interned -- or when it is malformed. The caller falls back to decoding, which
// is the source of truth.
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
		sealed := c.pos < len(c.b) && c.b[c.pos] == nEncrypted
		if !transcodeNode(c, &entries, t, 0) {
			return dst, false
		}
		// Matching EncodeWithHotEnc: a sealed attribute is never hot, because the hot header is
		// there to be read without decoding and a sealed value cannot be.
		if !sealed {
			if _, isHot := hot[id]; isHot {
				hots = append(hots, hotPair{id, uint32(nodeOff)})
			}
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
		c.skip(int(c.uvarint())) // nonce
		c.skip(int(c.uvarint())) // ciphertext
		return copyFrom()        // copied unopened; see the note at the top of this file
	case nAttrRef:
		c.skip(1) // scope byte
		c.uvarint()
		return copyFrom()

	case nAttrRefStr:
		// scope byte + inline name -> scope byte + id.
		scope := c.b[c.pos]
		c.skip(1)
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

	case nFunc:
		c.uvarint() // name id
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
	case nSelect:
		if !copyFrom() {
			return false
		}
		if !transcodeNode(c, out, t, depth+1) { // the record
			return false
		}
		at := c.pos
		c.uvarint()
		if !c.ok {
			return false
		}
		*out = append(*out, c.b[at:c.pos]...)
		return true
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
