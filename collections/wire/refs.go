package wire

import (
	"encoding/binary"

	"github.com/PelicanPlatform/classad/ast"
)

// AppendNodeRefIDs appends to dst the interned id of every attribute reference
// inside node that could resolve against the enclosing ad -- unscoped and
// MY.-scoped references (TARGET./PARENT. refer to a different ad) -- returning
// the extended slice. It walks the node's TLV structure directly (the same walk
// as skipNode), so collecting a node's references costs no decode, no name
// resolution and no allocation beyond dst's growth; a caller building a
// projection's reference closure recycles dst across nodes and passes.
//
// The collection is a superset: references inside nested record literals are
// included even though record-member resolution could shadow them, and duplicate
// ids are appended as-is (the caller's wanted-set absorbs both). An encrypted
// node is opaque and contributes nothing. Returns dst unchanged (beyond any
// partial appends) with no error indication on a malformed node -- the caller's
// subsequent render of the same bytes surfaces the corruption.
func AppendNodeRefIDs(node []byte, dst []uint32) []uint32 {
	c := &cursor{b: node, ok: true}
	return refNode(c, 0, dst)
}

// AppendNodeRefNames is AppendNodeRefIDs for an INLINE-NAME ad: it appends the inline
// name of every attribute reference inside node that could resolve against the enclosing
// ad -- unscoped and MY.-scoped -- returning the extended slice.
//
// A persistent (inline-name) collection stores references as names rather than interned
// ids, so the id collector finds nothing in one; this is what lets a projection over such
// a collection build the same reference closure. The appended names are sub-slices of
// node, valid for as long as node is, and are NOT copied -- a caller that retains them
// past the record's buffer must copy.
//
// Same superset semantics as AppendNodeRefIDs: nested record members are included even
// though member resolution could shadow them, duplicates are appended as-is, and an
// encrypted node contributes nothing.
func AppendNodeRefNames(node []byte, dst [][]byte) [][]byte {
	c := &cursor{b: node, ok: true}
	return refNodeNames(c, 0, dst)
}

func refNode(c *cursor, depth int, dst []uint32) []uint32 {
	if !c.ok || depth > maxDepth {
		c.ok = false
		return dst
	}
	tag := c.byteAt()
	switch tag {
	case nUndefined, nError, nBoolFalse, nBoolTrue:
		// no payload
	case nInt:
		_, n := binary.Varint(c.b[c.pos:])
		if n <= 0 {
			c.ok = false
			return dst
		}
		c.pos += n
	case nReal:
		c.skip(8)
	case nString:
		c.skip(int(c.uvarint()))
	case nAttrRef:
		scope := c.byteAt()
		id := c.uvarint()
		if c.ok && (ast.AttributeScope(scope) == ast.NoScope || ast.AttributeScope(scope) == ast.MyScope) {
			dst = append(dst, uint32(id))
		}
	case nAttrRefStr:
		c.skip(1)                // scope byte
		c.skip(int(c.uvarint())) // inline name: not an interned id, nothing to collect
	case nBinOp:
		c.skip(1) // op id
		dst = refNode(c, depth+1, dst)
		dst = refNode(c, depth+1, dst)
	case nBinOpStr:
		c.skip(int(c.uvarint())) // op string
		dst = refNode(c, depth+1, dst)
		dst = refNode(c, depth+1, dst)
	case nUnOp:
		c.skip(1)
		dst = refNode(c, depth+1, dst)
	case nUnOpStr:
		c.skip(int(c.uvarint()))
		dst = refNode(c, depth+1, dst)
	case nList:
		n := c.uvarint()
		for i := uint64(0); i < n && c.ok; i++ {
			dst = refNode(c, depth+1, dst)
		}
	case nRecord:
		dst = refAdBody(c, depth+1, dst)
	case nFunc:
		c.uvarint() // function name id: not an attribute reference
		n := c.uvarint()
		for i := uint64(0); i < n && c.ok; i++ {
			dst = refNode(c, depth+1, dst)
		}
	case nFuncStr:
		c.skip(int(c.uvarint())) // inline function name
		n := c.uvarint()
		for i := uint64(0); i < n && c.ok; i++ {
			dst = refNode(c, depth+1, dst)
		}
	case nCond:
		dst = refNode(c, depth+1, dst)
		dst = refNode(c, depth+1, dst)
		dst = refNode(c, depth+1, dst)
	case nElvis, nSubscript:
		dst = refNode(c, depth+1, dst)
		dst = refNode(c, depth+1, dst)
	case nSelect:
		dst = refNode(c, depth+1, dst)
		c.uvarint() // selected member id: resolves inside the record, not the ad
	case nSelectStr:
		dst = refNode(c, depth+1, dst)
		c.skip(int(c.uvarint()))
	case nParen:
		dst = refNode(c, depth+1, dst)
	case nEncrypted:
		c.skip(int(c.uvarint())) // nonce
		c.skip(int(c.uvarint())) // ciphertext: opaque, no visible references
	default:
		c.ok = false
	}
	return dst
}

// refAdBody walks a nested record body ([hotCount][hot][attrCount][key+node]*),
// collecting references from each member's value node.
func refAdBody(c *cursor, depth int, dst []uint32) []uint32 {
	if !c.ok || depth > maxDepth {
		c.ok = false
		return dst
	}
	hotCount := c.uvarint()
	for i := uint64(0); i < hotCount && c.ok; i++ {
		c.uvarint()
		c.uvarint()
	}
	attrCount := c.uvarint()
	for i := uint64(0); i < attrCount && c.ok; i++ {
		c.skipKey()
		dst = refNode(c, depth+1, dst)
	}
	return dst
}

func refNodeNames(c *cursor, depth int, dst [][]byte) [][]byte {
	if !c.ok || depth > maxDepth {
		c.ok = false
		return dst
	}
	tag := c.byteAt()
	switch tag {
	case nUndefined, nError, nBoolFalse, nBoolTrue:
		// no payload
	case nInt:
		_, n := binary.Varint(c.b[c.pos:])
		if n <= 0 {
			c.ok = false
			return dst
		}
		c.pos += n
	case nReal:
		c.skip(8)
	case nString:
		c.skip(int(c.uvarint()))
	case nAttrRef:
		c.skip(1)   // scope byte
		c.uvarint() // interned id: an inline-name ad resolves by name, nothing to collect
	case nAttrRefStr:
		scope := c.byteAt()
		n := int(c.uvarint())
		start := c.pos
		c.skip(n)
		if c.ok && (ast.AttributeScope(scope) == ast.NoScope || ast.AttributeScope(scope) == ast.MyScope) {
			dst = append(dst, c.b[start:start+n])
		}
	case nBinOp:
		c.skip(1) // op id
		dst = refNodeNames(c, depth+1, dst)
		dst = refNodeNames(c, depth+1, dst)
	case nBinOpStr:
		c.skip(int(c.uvarint())) // op string
		dst = refNodeNames(c, depth+1, dst)
		dst = refNodeNames(c, depth+1, dst)
	case nUnOp:
		c.skip(1)
		dst = refNodeNames(c, depth+1, dst)
	case nUnOpStr:
		c.skip(int(c.uvarint()))
		dst = refNodeNames(c, depth+1, dst)
	case nList:
		n := c.uvarint()
		for i := uint64(0); i < n && c.ok; i++ {
			dst = refNodeNames(c, depth+1, dst)
		}
	case nRecord:
		dst = refAdBodyNames(c, depth+1, dst)
	case nFunc:
		c.uvarint() // function name id: not an attribute reference
		n := c.uvarint()
		for i := uint64(0); i < n && c.ok; i++ {
			dst = refNodeNames(c, depth+1, dst)
		}
	case nFuncStr:
		c.skip(int(c.uvarint())) // inline function name
		n := c.uvarint()
		for i := uint64(0); i < n && c.ok; i++ {
			dst = refNodeNames(c, depth+1, dst)
		}
	case nCond:
		dst = refNodeNames(c, depth+1, dst)
		dst = refNodeNames(c, depth+1, dst)
		dst = refNodeNames(c, depth+1, dst)
	case nElvis, nSubscript:
		dst = refNodeNames(c, depth+1, dst)
		dst = refNodeNames(c, depth+1, dst)
	case nSelect:
		dst = refNodeNames(c, depth+1, dst)
		c.uvarint() // selected member id: resolves inside the record, not the ad
	case nSelectStr:
		dst = refNodeNames(c, depth+1, dst)
		c.skip(int(c.uvarint()))
	case nParen:
		dst = refNodeNames(c, depth+1, dst)
	case nEncrypted:
		c.skip(int(c.uvarint())) // nonce
		c.skip(int(c.uvarint())) // ciphertext: opaque, no visible references
	default:
		c.ok = false
	}
	return dst
}

// refAdBody walks a nested record body ([hotCount][hot][attrCount][key+node]*),
// collecting references from each member's value node.
func refAdBodyNames(c *cursor, depth int, dst [][]byte) [][]byte {
	if !c.ok || depth > maxDepth {
		c.ok = false
		return dst
	}
	hotCount := c.uvarint()
	for i := uint64(0); i < hotCount && c.ok; i++ {
		c.uvarint()
		c.uvarint()
	}
	attrCount := c.uvarint()
	for i := uint64(0); i < attrCount && c.ok; i++ {
		c.skipKey()
		dst = refNodeNames(c, depth+1, dst)
	}
	return dst
}

// NodeRefs is the reference set of one node, for a caller that must know it is COMPLETE: one
// deciding whether a narrowed ad evaluates exactly as the whole ad would. AppendNodeRefIDs and
// AppendNodeRefNames are deliberately lenient -- they serve a projection that ships what it finds --
// and stay silent about what they cannot see. This reports it instead.
type NodeRefs struct {
	IDs   []uint32 // interned-id references (nAttrRef), unscoped or MY.
	Names [][]byte // inline-name references (nAttrRefStr), unscoped or MY.; sub-slices of the node
	// Sealed reports that the node holds a sealed value, whose plaintext may refer to anything.
	// Whether that matters is the caller's question: a reader that opens sealed values and
	// follows their references must treat the set as incomplete, one that does not need not.
	Sealed bool
}

// AppendNodeRefsExact appends node's unscoped and MY. references to r and reports whether the set
// is complete. It is not complete when the node calls eval(), whose string argument names an
// attribute only at run time, or when it does not parse. A sealed value, whose plaintext the walk
// cannot see, sets r.Sealed and contributes nothing. inline is the enclosing ad's key encoding, which a nested record
// literal's members share. isEval reports whether an interned function-name id is eval; nil means
// no interned id is (an inline-keyed ad names its functions inline).
//
// References inside nested record literals are included, as the lenient walks include them: a
// superset errs toward keeping more, which is the safe direction for a completeness test.
func AppendNodeRefsExact(node []byte, inline bool, r *NodeRefs, isEval func(uint32) bool) bool {
	c := &cursor{b: node, ok: true, inline: inline}
	return refsExact(c, 0, r, isEval) && c.ok
}

func refsExact(c *cursor, depth int, r *NodeRefs, isEval func(uint32) bool) bool {
	if !c.ok || depth > maxDepth {
		return false
	}
	tag := c.byteAt()
	switch tag {
	case nUndefined, nError, nBoolFalse, nBoolTrue:
	case nInt:
		_, n := binary.Varint(c.b[c.pos:])
		if n <= 0 {
			return false
		}
		c.pos += n
	case nReal:
		c.skip(8)
	case nString:
		c.skip(int(c.uvarint()))
	case nAttrRef:
		scope := c.byteAt()
		id := c.uvarint()
		if c.ok && (ast.AttributeScope(scope) == ast.NoScope || ast.AttributeScope(scope) == ast.MyScope) {
			r.IDs = append(r.IDs, uint32(id))
		}
	case nAttrRefStr:
		scope := c.byteAt()
		n := int(c.uvarint())
		start := c.pos
		c.skip(n)
		if c.ok && (ast.AttributeScope(scope) == ast.NoScope || ast.AttributeScope(scope) == ast.MyScope) {
			r.Names = append(r.Names, c.b[start:start+n])
		}
	case nBinOp, nUnOp:
		c.skip(1)
		if !refsExact(c, depth+1, r, isEval) {
			return false
		}
		if tag == nBinOp && !refsExact(c, depth+1, r, isEval) {
			return false
		}
	case nBinOpStr, nUnOpStr:
		c.skip(int(c.uvarint()))
		if !refsExact(c, depth+1, r, isEval) {
			return false
		}
		if tag == nBinOpStr && !refsExact(c, depth+1, r, isEval) {
			return false
		}
	case nList:
		n := c.uvarint()
		for i := uint64(0); i < n && c.ok; i++ {
			if !refsExact(c, depth+1, r, isEval) {
				return false
			}
		}
	case nRecord:
		hotCount := c.uvarint()
		for i := uint64(0); i < hotCount && c.ok; i++ {
			c.uvarint()
			c.uvarint()
		}
		attrCount := c.uvarint()
		for i := uint64(0); i < attrCount && c.ok; i++ {
			c.skipKey()
			if !refsExact(c, depth+1, r, isEval) {
				return false
			}
		}
	case nFunc, nFuncStr:
		if tag == nFunc {
			id := c.uvarint()
			if isEval != nil && isEval(uint32(id)) {
				return false
			}
		} else {
			n := int(c.uvarint())
			start := c.pos
			c.skip(n)
			if c.ok && foldEqualBytes(c.b[start:start+n], "eval") {
				return false
			}
		}
		n := c.uvarint()
		for i := uint64(0); i < n && c.ok; i++ {
			if !refsExact(c, depth+1, r, isEval) {
				return false
			}
		}
	case nCond:
		for range 3 {
			if !refsExact(c, depth+1, r, isEval) {
				return false
			}
		}
	case nElvis, nSubscript:
		for range 2 {
			if !refsExact(c, depth+1, r, isEval) {
				return false
			}
		}
	case nSelect:
		if !refsExact(c, depth+1, r, isEval) {
			return false
		}
		c.uvarint()
	case nSelectStr:
		if !refsExact(c, depth+1, r, isEval) {
			return false
		}
		c.skip(int(c.uvarint()))
	case nParen:
		return refsExact(c, depth+1, r, isEval)
	case nEncrypted:
		c.skip(int(c.uvarint())) // nonce
		c.skip(int(c.uvarint())) // ciphertext
		r.Sealed = true
	default:
		return false // a tag this walk does not know
	}
	return c.ok
}
