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
