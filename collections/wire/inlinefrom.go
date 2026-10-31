package wire

import "encoding/binary"

// SHIPPING AN INTERNED RECORD WITHOUT AN AST.
//
// This is the mirror of transcode.go. That file rewrites an inline-names record into the
// interned form a sealed segment stores; this one rewrites an interned record into the
// self-contained inline-names form a wire-form ROW must have, because the consumer of a row
// (another process, across dbrpc) holds no intern table.
//
// The two forms differ in exactly the two places transcode.go names, read the other way:
//
//	an attribute KEY   uvarint(internID)   becomes   uvarint(nameLen) + name
//	a name-carrying NODE  nAttrRef/nFunc/nSelect  becomes  nAttrRefStr/nFuncStr/nSelectStr
//
// Everything else -- every literal, every operator, every list, every sealed value -- is the
// same bytes in both forms, so this copies and rewrites only where the encoding differs.
//
// The selection is fused into the same pass: an attribute the caller does not want is skipped
// before its node is rewritten, so a projected read of a 45-attribute ad converts the handful
// of entries it ships and not the rest. That is the same reason narrowProjected exists for the
// per-segment interned case; here the keep test does the narrowing itself.
//
// An interned record is NOT sealed by the write path (encodeAdInto only seals in inline mode),
// so an nEncrypted node is not expected here; it is handled anyway, on exactly the terms
// AppendAdSubsetInline handles it -- opened with the caller's key, and the entry dropped when
// there is no key. Dropping rather than shipping is what keeps a redacted read (open == nil)
// unable to produce the value at all.

// AppendAdSubsetInlineFromInterned is AppendAdSubsetInline for a src whose attribute keys are
// INTERNED ids: it assembles the same self-contained inline-names subset ad, resolving each id
// through resolve and rewriting the name-carrying nodes. The emitted ad carries no hot header.
//
// An inline-names src is passed straight to AppendAdSubsetInline, so a caller holding a mixed
// population (an in-memory store whose records are interned, a converted segment record that is
// already inline) can call this unconditionally.
//
// keep is called with the resolved attribute name and the attribute's node bytes AS STORED --
// i.e. the INTERNED node, before rewriting. A keep that only inspects the name (what the
// collection's wire-subset selector does) behaves identically on both forms; one that inspects
// node bytes must account for the encoding.
//
// Returns ok=false when src is malformed, when it is standalone (its ids index an embedded
// table, not resolve's), or when an id does not resolve -- the same contract
// AppendAdSubsetInline has, so a caller discards the partial append and falls back.
// keep=nil keeps every entry. sc carries the reusable name buffer; it must not be shared
// across goroutines.
func AppendAdSubsetInlineFromInterned(dst []byte, src Ad, resolve func(uint32) (string, bool),
	keep func(name, node []byte) bool, open Sealer, sc *SubsetScratch) ([]byte, bool) {
	c, ok := src.bodyStart()
	if !ok {
		return dst, false
	}
	if c.inline {
		return AppendAdSubsetInline(dst, src, keep, open)
	}
	if resolve == nil {
		return dst, false // an interned src's ids cannot be named without one
	}
	if src[2]&flagStandalone != 0 {
		return dst, false // ids index the embedded table, which resolve knows nothing about
	}
	hotCount := c.uvarint()
	for i := uint64(0); i < hotCount && c.ok; i++ {
		c.uvarint()
		c.uvarint()
	}
	attrCount := c.uvarint()
	if !c.ok {
		return dst, false
	}

	dst = append(dst, magicByte, formatVer, flagInlineNames)
	dst = append(dst, 0) // hotCount = 0
	countAt := len(dst)
	dst = append(dst, 0x80, 0x80, 0x80, 0x80, 0x00) // 5-byte uvarint placeholder, patched below
	kept := uint64(0)

	for i := uint64(0); i < attrCount && c.ok; i++ {
		id := c.id32()
		nodeStart := c.pos
		skipNode(c, 0)
		if !c.ok {
			return dst, false
		}
		node := src[nodeStart:c.pos]
		name, found := resolve(id)
		if !found {
			return dst, false // a record keyed by an id this table does not know: malformed
		}
		sc.nameBuf = append(sc.nameBuf[:0], name...)
		if keep != nil && !keep(sc.nameBuf, node) {
			continue
		}
		if IsEncryptedNode(node) {
			plain, opened := OpenEncryptedNode(node, open)
			if !opened {
				continue // no key (or tampered): the value is absent to this consumer
			}
			dst = appendInlineKey(dst, sc.nameBuf)
			pc := &cursor{b: plain, ok: true}
			if !inlineNode(pc, &dst, resolve, 0) {
				return dst, false
			}
			kept++
			continue
		}
		dst = appendInlineKey(dst, sc.nameBuf)
		nc := &cursor{b: node, ok: true}
		if !inlineNode(nc, &dst, resolve, 0) {
			return dst, false
		}
		kept++
	}
	if !c.ok {
		return dst, false
	}
	patchUvarint5(dst[countAt:countAt+5], kept)
	return dst, true
}

// appendInlineKey writes an attribute key in the inline-names encoding (uvarint len + bytes).
func appendInlineKey(dst []byte, name []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(name)))
	return append(dst, name...)
}

// appendInlineName writes a name-carrying node's inline name (uvarint len + bytes).
func appendInlineName(dst *[]byte, resolve func(uint32) (string, bool), id uint32) bool {
	name, ok := resolve(id)
	if !ok {
		return false
	}
	*dst = binary.AppendUvarint(*dst, uint64(len(name)))
	*dst = append(*dst, name...)
	return true
}

// inlineNode copies one node from c to out, rewriting the interned name-carrying variants into
// their inline forms and leaving everything else byte-identical. It mirrors skipNode's grammar
// case for case -- a tag handled there and missed here would silently truncate a record, so the
// two are meant to be read side by side -- and it is transcodeNode read backwards.
func inlineNode(c *cursor, out *[]byte, resolve func(uint32) (string, bool), depth int) bool {
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
		return copyFrom()        // copied unopened, exactly as the inline subset copies it

	case nAttrRef:
		// scope byte + id -> scope byte + inline name.
		if c.pos >= len(c.b) {
			c.ok = false
			return false
		}
		scope := c.b[c.pos]
		c.skip(1)
		id := c.id32()
		if !c.ok {
			return false
		}
		*out = append(*out, nAttrRefStr, scope)
		return appendInlineName(out, resolve, id)
	case nAttrRefStr:
		// Already inline (a record that mixes forms): copy through.
		c.skip(1)                // scope byte
		c.skip(int(c.uvarint())) // inline name
		return copyFrom()

	case nBinOp:
		c.skip(1) // op id
		if !copyFrom() {
			return false
		}
		return inlineNode(c, out, resolve, depth+1) && inlineNode(c, out, resolve, depth+1)
	case nBinOpStr:
		c.skip(int(c.uvarint())) // op string, kept as-is
		if !copyFrom() {
			return false
		}
		return inlineNode(c, out, resolve, depth+1) && inlineNode(c, out, resolve, depth+1)
	case nUnOp:
		c.skip(1)
		if !copyFrom() {
			return false
		}
		return inlineNode(c, out, resolve, depth+1)
	case nUnOpStr:
		c.skip(int(c.uvarint()))
		if !copyFrom() {
			return false
		}
		return inlineNode(c, out, resolve, depth+1)
	case nList:
		n := c.uvarint()
		if !copyFrom() {
			return false
		}
		for i := uint64(0); i < n; i++ {
			if !inlineNode(c, out, resolve, depth+1) {
				return false
			}
		}
		return true
	case nRecord:
		// byteAt already consumed the tag, so there is nothing to skip here.
		*out = append(*out, nRecord)
		return inlineAdBody(c, out, resolve, depth+1)

	case nFunc:
		id := c.id32()
		n := c.uvarint()
		if !c.ok {
			return false
		}
		*out = append(*out, nFuncStr)
		if !appendInlineName(out, resolve, id) {
			return false
		}
		*out = binary.AppendUvarint(*out, n)
		for i := uint64(0); i < n; i++ {
			if !inlineNode(c, out, resolve, depth+1) {
				return false
			}
		}
		return true
	case nFuncStr:
		c.skip(int(c.uvarint())) // inline name
		n := c.uvarint()
		if !copyFrom() {
			return false
		}
		for i := uint64(0); i < n; i++ {
			if !inlineNode(c, out, resolve, depth+1) {
				return false
			}
		}
		return true

	case nCond:
		if !copyFrom() {
			return false
		}
		return inlineNode(c, out, resolve, depth+1) &&
			inlineNode(c, out, resolve, depth+1) &&
			inlineNode(c, out, resolve, depth+1)
	case nElvis, nSubscript:
		if !copyFrom() {
			return false
		}
		return inlineNode(c, out, resolve, depth+1) && inlineNode(c, out, resolve, depth+1)
	case nSelect:
		// node(record) + id -> node(record) + inline name. The TAG has to be written before the
		// record node, so it cannot share the copyFrom path.
		*out = append(*out, nSelectStr)
		if !inlineNode(c, out, resolve, depth+1) { // the record
			return false
		}
		id := c.id32()
		if !c.ok {
			return false
		}
		return appendInlineName(out, resolve, id)
	case nSelectStr:
		*out = append(*out, nSelectStr)
		if !inlineNode(c, out, resolve, depth+1) { // the record
			return false
		}
		at := c.pos
		c.skip(int(c.uvarint())) // inline name
		if !c.ok {
			return false
		}
		*out = append(*out, c.b[at:c.pos]...)
		return true
	case nParen:
		if !copyFrom() {
			return false
		}
		return inlineNode(c, out, resolve, depth+1)

	default:
		c.ok = false
		return false
	}
}

// inlineAdBody is inlineNode for a nested record literal: the same key rewrite as the top-level
// ad, with no hot header of its own (nested bodies never carry one).
//
// A nested body's keys are in the SAME encoding as the enclosing ad -- skipAdBody reads them
// through the top-level cursor's mode -- so an interned ad's nested records are keyed by id too.
func inlineAdBody(c *cursor, out *[]byte, resolve func(uint32) (string, bool), depth int) bool {
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
		id := c.id32()
		if !c.ok {
			return false
		}
		if !appendInlineName(out, resolve, id) {
			return false
		}
		if !inlineNode(c, out, resolve, depth+1) {
			return false
		}
	}
	return true
}
