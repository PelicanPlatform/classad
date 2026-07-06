package wire

import (
	"encoding/binary"

	"github.com/PelicanPlatform/classad/ast"
)

// Ad is a zero-copy view over an encoded ad (the non-standalone in-store form:
// magic/version/flags header followed by the ad body). Lookup and ForEach walk
// the bytes without allocating ast nodes, resolving attributes by interned id.
type Ad []byte

// cursor walks encoded bytes, advancing pos; ok reports whether the last read
// stayed in bounds.
type cursor struct {
	b      []byte
	pos    int
	ok     bool
	inline bool // attribute keys are inline names (flagInlineNames)
}

// skipKey advances past an attribute key (inline name or interned id).
func (c *cursor) skipKey() {
	if c.inline {
		c.skip(int(c.uvarint()))
	} else {
		c.uvarint()
	}
}

func (c *cursor) uvarint() uint64 {
	if !c.ok {
		return 0
	}
	v, n := binary.Uvarint(c.b[c.pos:])
	if n <= 0 {
		c.ok = false
		return 0
	}
	c.pos += n
	return v
}

func (c *cursor) byteAt() byte {
	if !c.ok || c.pos >= len(c.b) {
		c.ok = false
		return 0
	}
	v := c.b[c.pos]
	c.pos++
	return v
}

func (c *cursor) skip(n int) {
	if !c.ok || n < 0 || c.pos+n > len(c.b) {
		c.ok = false
		return
	}
	c.pos += n
}

// bodyStart positions a cursor at the start of the ad body, past the header and
// any embedded standalone intern table. Returns false if the header is invalid.
func (a Ad) bodyStart() (*cursor, bool) {
	if len(a) < 3 || a[0] != magicByte || a[1] != formatVer {
		return nil, false
	}
	c := &cursor{b: a, pos: 3, ok: true, inline: a[2]&flagInlineNames != 0}
	if a[2]&flagStandalone != 0 {
		n := c.uvarint()
		for i := uint64(0); i < n && c.ok; i++ {
			l := c.uvarint()
			c.skip(int(l))
		}
	}
	if !c.ok {
		return nil, false
	}
	return c, true
}

// Lookup returns the raw node bytes for the attribute with the given interned
// id, or (nil, false) if absent or malformed. The returned slice aliases a; it
// can be decoded with DecodeNode or handed to the vm.
//
// A popular ("hot") attribute is found in O(1) via the hot header: its node
// offset (relative to the start of the attribute-entries region) is read
// directly, skipping the linear scan of the body. Non-hot attributes fall back
// to a linear skip-scan.
func (a Ad) Lookup(id uint32) ([]byte, bool) {
	c, ok := a.bodyStart()
	if !ok {
		return nil, false
	}
	// Hot header: (internID, entries-relative offset-to-node) pairs. The offsets
	// resolve against the entries region, whose start is known only after the
	// attrCount that follows the hot header, so record a hit and resolve below.
	hotCount := c.uvarint()
	var hotOff uint32
	hotHit := false
	for i := uint64(0); i < hotCount && c.ok; i++ {
		hid := c.uvarint()
		off := c.uvarint()
		if c.ok && uint32(hid) == id {
			hotOff = uint32(off)
			hotHit = true
		}
	}
	attrCount := c.uvarint()
	if !c.ok {
		return nil, false
	}
	entriesStart := c.pos
	if hotHit {
		return nodeBytesAt(a, entriesStart+int(hotOff))
	}
	// Body: linear scan, skipping non-matching nodes.
	for i := uint64(0); i < attrCount && c.ok; i++ {
		aid := c.uvarint()
		start := c.pos
		skipNode(c, 0)
		if !c.ok {
			return nil, false
		}
		if uint32(aid) == id {
			return a[start:c.pos], true
		}
	}
	return nil, false
}

// ForEach calls fn with each attribute's interned id and raw node bytes, in
// stored order, until fn returns false or the ad ends. It skips the hot header
// (which only duplicates body entries). Returns false if the ad is malformed.
func (a Ad) ForEach(fn func(id uint32, node []byte) bool) bool {
	c, ok := a.bodyStart()
	if !ok {
		return false
	}
	hotCount := c.uvarint()
	for i := uint64(0); i < hotCount && c.ok; i++ {
		c.uvarint()
		c.uvarint()
	}
	attrCount := c.uvarint()
	for i := uint64(0); i < attrCount && c.ok; i++ {
		aid := c.uvarint()
		start := c.pos
		skipNode(c, 0)
		if !c.ok {
			return false
		}
		if !fn(uint32(aid), a[start:c.pos]) {
			return true
		}
	}
	return c.ok
}

// nodeBytesAt returns the node bytes starting at absolute offset off.
func nodeBytesAt(a Ad, off int) ([]byte, bool) {
	if off < 0 || off > len(a) {
		return nil, false
	}
	c := &cursor{b: a, pos: off, ok: true}
	skipNode(c, 0)
	if !c.ok {
		return nil, false
	}
	return a[off:c.pos], true
}

// DecodeNode decodes raw node bytes (as returned by Lookup/ForEach) into an
// ast.Expr, resolving interned ids via t.
func DecodeNode(node []byte, t *InternTable) (ast.Expr, error) {
	d := &decoder{b: node, resolve: t.Name}
	return d.node(0)
}

// DecodeNodeInline decodes raw node bytes from an inline-names ad (as returned by
// LookupByName), which need no intern table.
func DecodeNodeInline(node []byte) (ast.Expr, error) {
	d := &decoder{b: node, inline: true} // nested records carry inline keys too
	return d.node(0)
}

// LookupByName returns the raw node bytes for the named attribute in an
// inline-names ad, or (nil, false) if absent, not an inline ad, or malformed.
// Attribute names are compared case-insensitively. A hot attribute is found in
// O(1) via the hot header (keyed by a case-folded name hash, verified against the
// stored name); others fall back to a linear scan.
func (a Ad) LookupByName(name string) ([]byte, bool) {
	c, ok := a.bodyStart()
	if !ok || !c.inline {
		return nil, false
	}
	h := nameHash32(name)
	hotCount := c.uvarint()
	var hotEntryOff uint32
	hotHit := false
	for i := uint64(0); i < hotCount && c.ok; i++ {
		hh := c.uvarint()  // nameHash32
		off := c.uvarint() // entries-relative offset to the (name, node) entry
		if c.ok && uint32(hh) == h {
			hotEntryOff, hotHit = uint32(off), true
		}
	}
	attrCount := c.uvarint()
	if !c.ok {
		return nil, false
	}
	entriesStart := c.pos
	if hotHit {
		ec := &cursor{b: a, pos: entriesStart + int(hotEntryOff), ok: true, inline: true}
		nm := ec.readNameBytes()
		if ec.ok && foldEqualBytes(nm, name) {
			nodeStart := ec.pos
			skipNode(ec, 0)
			if ec.ok {
				return a[nodeStart:ec.pos], true
			}
		}
		// hash collision or malformed hot entry: fall through to the linear scan.
	}
	for i := uint64(0); i < attrCount && c.ok; i++ {
		nm := c.readNameBytes()
		nodeStart := c.pos
		skipNode(c, 0)
		if !c.ok {
			return nil, false
		}
		if foldEqualBytes(nm, name) {
			return a[nodeStart:c.pos], true
		}
	}
	return nil, false
}

// readNameBytes reads an inline name (uvarint len + bytes) as a subslice of the
// cursor's buffer (no allocation).
func (c *cursor) readNameBytes() []byte {
	n := int(c.uvarint())
	if !c.ok || n < 0 || c.pos+n > len(c.b) {
		c.ok = false
		return nil
	}
	b := c.b[c.pos : c.pos+n]
	c.pos += n
	return b
}

// foldEqualBytes reports whether name bytes b equal string s, case-insensitively
// over ASCII (attribute names are case-insensitive).
func foldEqualBytes(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i := 0; i < len(b); i++ {
		cb, cs := b[i], s[i]
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if cs >= 'A' && cs <= 'Z' {
			cs += 'a' - 'A'
		}
		if cb != cs {
			return false
		}
	}
	return true
}

// skipNode advances c past exactly one node without allocating.
func skipNode(c *cursor, depth int) {
	if !c.ok {
		return
	}
	if depth > maxDepth {
		c.ok = false
		return
	}
	tag := c.byteAt()
	switch tag {
	case nUndefined, nError, nBoolFalse, nBoolTrue:
		// no payload
	case nInt:
		_, n := binary.Varint(c.b[c.pos:])
		if n <= 0 {
			c.ok = false
			return
		}
		c.pos += n
	case nReal:
		c.skip(8)
	case nString:
		c.skip(int(c.uvarint()))
	case nAttrRef:
		c.skip(1) // scope byte
		c.uvarint()
	case nAttrRefStr:
		c.skip(1)                // scope byte
		c.skip(int(c.uvarint())) // inline name
	case nBinOp:
		c.skip(1) // op id
		skipNode(c, depth+1)
		skipNode(c, depth+1)
	case nBinOpStr:
		c.skip(int(c.uvarint())) // op string
		skipNode(c, depth+1)
		skipNode(c, depth+1)
	case nUnOp:
		c.skip(1)
		skipNode(c, depth+1)
	case nUnOpStr:
		c.skip(int(c.uvarint()))
		skipNode(c, depth+1)
	case nList:
		n := c.uvarint()
		for i := uint64(0); i < n && c.ok; i++ {
			skipNode(c, depth+1)
		}
	case nRecord:
		skipAdBody(c, depth+1)
	case nFunc:
		c.uvarint() // name id
		n := c.uvarint()
		for i := uint64(0); i < n && c.ok; i++ {
			skipNode(c, depth+1)
		}
	case nFuncStr:
		c.skip(int(c.uvarint())) // inline name
		n := c.uvarint()
		for i := uint64(0); i < n && c.ok; i++ {
			skipNode(c, depth+1)
		}
	case nCond:
		skipNode(c, depth+1)
		skipNode(c, depth+1)
		skipNode(c, depth+1)
	case nElvis, nSubscript:
		skipNode(c, depth+1)
		skipNode(c, depth+1)
	case nSelect:
		skipNode(c, depth+1)
		c.uvarint()
	case nSelectStr:
		skipNode(c, depth+1)     // record
		c.skip(int(c.uvarint())) // inline name
	case nParen:
		skipNode(c, depth+1)
	default:
		c.ok = false
	}
}

// skipAdBody advances c past a nested ad body ([hotCount][hot][attrCount][entries]).
func skipAdBody(c *cursor, depth int) {
	if !c.ok || depth > maxDepth {
		c.ok = false
		return
	}
	hotCount := c.uvarint()
	for i := uint64(0); i < hotCount && c.ok; i++ {
		c.uvarint()
		c.uvarint()
	}
	attrCount := c.uvarint()
	for i := uint64(0); i < attrCount && c.ok; i++ {
		c.skipKey()
		skipNode(c, depth+1)
	}
}
