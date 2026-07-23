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

// ForEachNamed calls fn with each attribute's name and raw node bytes. Inline-name
// ads (flagInlineNames, written by a persistent collection) yield their stored names
// directly; interned ads resolve each id to a name via t (an id t cannot resolve is
// skipped). Unlike ForEach -- which always reads the key as an interned id and so is
// wrong for inline ads -- this works for both encodings. Returns false if fn stopped
// early or the ad is malformed.
func (a Ad) ForEachNamed(t *InternTable, fn func(name string, node []byte) bool) bool {
	return a.forEachNamed(t, false, fn)
}

// ForEachNamedRedact is ForEachNamed except attributes whose name the table
// classified as private (see NewInternTableWithPrivacy) are skipped -- their
// nodes are never decoded and fn never sees them. Interned attributes are
// filtered by their precomputed per-id flag (O(1), no name resolution); inline
// attributes fall back to the table's privacy predicate on the stored name.
func (a Ad) ForEachNamedRedact(t *InternTable, fn func(name string, node []byte) bool) bool {
	return a.forEachNamed(t, true, fn)
}

// ForEachHotInlineBuf is ForEachHotBuf's inline-names counterpart: an inline
// ad's hot pairs are (nameHash32, offset) where the offset points at the whole
// attribute ENTRY (inline name, then node). It yields the folded name hash, the
// raw name bytes and the node bytes; the caller must verify the name (hashes
// can collide) with foldEqualBytes before trusting a match. scratch as in
// ForEachHotBuf. Reports false for an interned ad.
func (a Ad) ForEachHotInlineBuf(scratch []uint32, fn func(hash uint32, name, node []byte) bool) ([]uint32, bool) {
	c, ok := a.bodyStart()
	if !ok || !c.inline {
		return scratch, false
	}
	hotCount := c.uvarint()
	if hotCount == 0 {
		return scratch, c.ok
	}
	scratch = scratch[:0]
	for i := uint64(0); i < hotCount && c.ok; i++ {
		scratch = append(scratch, uint32(c.uvarint()), uint32(c.uvarint()))
	}
	c.uvarint() // attrCount
	if !c.ok {
		return scratch, false
	}
	entriesStart := c.pos
	for i := 0; i+1 < len(scratch); i += 2 {
		ec := &cursor{b: a, pos: entriesStart + int(scratch[i+1]), ok: true, inline: true}
		name := ec.readNameBytes()
		if !ec.ok {
			return scratch, false
		}
		nodeStart := ec.pos
		skipNode(ec, 0)
		if !ec.ok {
			return scratch, false
		}
		if !fn(scratch[i], name, a[nodeStart:ec.pos]) {
			return scratch, true
		}
	}
	return scratch, c.ok
}

// ForEachNameNode iterates an inline-names ad's attribute entries as raw (name
// bytes, node bytes) pairs, allocating nothing -- the projection walk for a
// persistent collection filters on the name bytes before rendering anything.
// Reports false (calling fn for nothing) for an interned ad.
func (a Ad) ForEachNameNode(fn func(name, node []byte) bool) bool {
	c, ok := a.bodyStart()
	if !ok || !c.inline {
		return false
	}
	hotCount := c.uvarint()
	for i := uint64(0); i < hotCount && c.ok; i++ {
		c.uvarint()
		c.uvarint()
	}
	attrCount := c.uvarint()
	for i := uint64(0); i < attrCount && c.ok; i++ {
		name := c.readNameBytes()
		if !c.ok {
			return false
		}
		nodeStart := c.pos
		skipNode(c, 0)
		if !c.ok {
			return false
		}
		if !fn(name, a[nodeStart:c.pos]) {
			return true
		}
	}
	return c.ok
}

// NameHash32 exposes the case-insensitive 32-bit hash used by inline hot-header
// pairs, so a projected reader can precompute its wanted-name hashes.
func NameHash32(name string) uint32 { return nameHash32(name) }

// NameHash32Bytes is NameHash32 over raw name bytes (no string conversion).
func NameHash32Bytes(name []byte) uint32 {
	const off, prime = 2166136261, 16777619
	h := uint32(off)
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		h = (h ^ uint32(c)) * prime
	}
	return h
}

// FoldEqualBytes reports whether the raw attribute-name bytes equal name
// case-insensitively (ASCII fold, matching ClassAd attribute-name semantics).
func FoldEqualBytes(b []byte, name string) bool { return foldEqualBytes(b, name) }

// ForEachIDNode iterates the ad's attribute entries as raw (interned id, node
// bytes) pairs, resolving nothing: the caller filters by id -- a projection's
// wanted-set test, a privacy flag -- before paying for name resolution or value
// rendering. Skipped entries cost one uvarint plus a TLV length hop. Returns
// false (calling fn for nothing) for an inline-names ad, whose entries carry no
// interned ids; fn returning false stops the walk early.
func (a Ad) ForEachIDNode(fn func(id uint32, node []byte) bool) bool {
	c, ok := a.bodyStart()
	if !ok || c.inline {
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
		if !c.ok {
			return false
		}
		nodeStart := c.pos
		skipNode(c, 0)
		if !c.ok {
			return false
		}
		if !fn(uint32(aid), a[nodeStart:c.pos]) {
			return true
		}
	}
	return c.ok
}

func (a Ad) forEachNamed(t *InternTable, redact bool, fn func(name string, node []byte) bool) bool {
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
		var name string
		if c.inline {
			nm := c.readNameBytes()
			if !c.ok {
				return false
			}
			name = string(nm)
			if redact && t.isPrivateName(name) {
				skipNode(c, 0) // private: never surface its node
				if !c.ok {
					return false
				}
				continue
			}
		} else {
			aid := c.uvarint()
			if !c.ok {
				return false
			}
			if redact && t.IsPrivate(uint32(aid)) {
				skipNode(c, 0) // private: skip before even resolving the name
				if !c.ok {
					return false
				}
				continue
			}
			n, ok := t.Name(uint32(aid))
			if !ok {
				skipNode(c, 0) // unresolved id: skip its node and continue
				continue
			}
			name = n
		}
		nodeStart := c.pos
		skipNode(c, 0)
		if !c.ok {
			return false
		}
		if !fn(name, a[nodeStart:c.pos]) {
			return true
		}
	}
	return c.ok
}

// HotClosureComplete reports whether the ad's hot header holds the complete match
// closure (flagHotClosure): ForEachHot then yields every attribute the match reads,
// so the matcher can trust it without scanning the ad body.
func (a Ad) HotClosureComplete() bool {
	return len(a) >= 3 && a[0] == magicByte && a[1] == formatVer && a[2]&flagHotClosure != 0
}

// AttrCount returns the number of attributes stored in the ad (0 if malformed). It
// reads only the header (past the hot index), so it is cheap enough to gate width-
// dependent decode strategies.
func (a Ad) AttrCount() int {
	c, ok := a.bodyStart()
	if !ok {
		return 0
	}
	hotCount := c.uvarint()
	for i := uint64(0); i < hotCount && c.ok; i++ {
		c.uvarint()
		c.uvarint()
	}
	n := c.uvarint()
	if !c.ok {
		return 0
	}
	return int(n)
}

// ForEachHot calls fn with the interned id and raw node bytes of each attribute
// recorded in the hot header, in header order, resolving each via its stored
// entries-relative offset (no scan). Returns false if fn stopped early or the ad is
// malformed. Cost is O(hotCount), independent of the total attribute count -- so a
// collection whose hot set is the match closure can read exactly the match-relevant
// attributes of a very wide ad without touching the cold ones.
// ForEachHotBuf is ForEachHot with caller-provided scratch for the header's
// (id, offset) pairs, grown as needed and returned for reuse -- a scan calling
// it once per ad performs no per-ad allocation (ForEachHot allocates two slices
// per call). scratch holds the pairs interleaved: id, offset, id, offset, ...
// Interned ads only: an inline ad's hot pairs are (nameHash32, offset-to-ENTRY)
// -- see ForEachHotInlineBuf -- so it reports false rather than mis-parse.
func (a Ad) ForEachHotBuf(scratch []uint32, fn func(id uint32, node []byte) bool) ([]uint32, bool) {
	c, ok := a.bodyStart()
	if !ok || c.inline {
		return scratch, false
	}
	hotCount := c.uvarint()
	if hotCount == 0 {
		return scratch, c.ok
	}
	scratch = scratch[:0]
	for i := uint64(0); i < hotCount && c.ok; i++ {
		scratch = append(scratch, uint32(c.uvarint()), uint32(c.uvarint()))
	}
	c.uvarint() // attrCount
	if !c.ok {
		return scratch, false
	}
	entriesStart := c.pos
	for i := 0; i+1 < len(scratch); i += 2 {
		node, ok := nodeBytesAt(a, entriesStart+int(scratch[i+1]))
		if !ok {
			return scratch, false
		}
		if !fn(scratch[i], node) {
			return scratch, true
		}
	}
	return scratch, c.ok
}

func (a Ad) ForEachHot(fn func(id uint32, node []byte) bool) bool {
	c, ok := a.bodyStart()
	if !ok {
		return false
	}
	hotCount := c.uvarint()
	if hotCount == 0 {
		return c.ok
	}
	ids := make([]uint32, hotCount)
	offs := make([]uint32, hotCount)
	for i := uint64(0); i < hotCount && c.ok; i++ {
		ids[i] = uint32(c.uvarint())
		offs[i] = uint32(c.uvarint())
	}
	c.uvarint() // attrCount
	if !c.ok {
		return false
	}
	entriesStart := c.pos
	for i := range ids {
		node, ok := nodeBytesAt(a, entriesStart+int(offs[i]))
		if !ok {
			return false
		}
		if !fn(ids[i], node) {
			return true
		}
	}
	return true
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
	case nEncrypted:
		c.skip(int(c.uvarint())) // nonce
		c.skip(int(c.uvarint())) // ciphertext
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
