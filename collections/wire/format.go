package wire

// On-the-wire constants for the ClassAd binary form.
//
// Ad layout:
//
//	[magic:1][version:1][flags:1]
//	if flags&flagStandalone:
//	    [internCount:uvarint] internCount * (uvarint(len) + name bytes)  // id order
//	[hotCount:uvarint]  hotCount * (uvarint(internID), uvarint(offset))  // popular attrs
//	[attrCount:uvarint]
//	attrCount * ( uvarint(internID) + node )
//
// Each attribute value is an expression node (see exprcode.go). A literal is
// simply a node with a literal tag, so the common "value" case is a 1-byte tag
// plus its payload. Nodes are self-delimiting (recursive), so the zero-copy
// accessor skips an attribute with a lightweight skipNode walk; the hot header
// stores absolute byte offsets so a popular attribute is reached without scanning.
const (
	magicByte      = 0xCA
	formatVer      = 1
	flagStandalone = 1 << 0 // an inline intern table is embedded in this ad
	// flagInlineNames: attribute keys are stored as inline names
	// (uvarint nameLen + bytes) instead of interned ids, and the ad's nodes use the
	// inline-name node variants (nAttrRefStr/nFuncStr/nSelectStr). Such an ad is
	// fully self-contained — it decodes with no InternTable. Used by the persistent
	// store, where records must be recoverable without a shared table.
	flagInlineNames = 1 << 1

	// flagHotClosure: the hot header holds the COMPLETE transitive closure of the
	// collection's match roots (e.g. Requirements) -- every attribute the match reads
	// from this ad, present in the ad, is in the hot header. A matcher can then read
	// the match-relevant attributes via ForEachHot and trust it is complete, without
	// scanning the ad. Set only when that closure is statically determinable (no eval()).
	flagHotClosure = 1 << 2

	// A hot-header pair is (internID, offset) in interned ads and
	// (nameHash32, offset) in inline-names ads; the offset points at the attribute
	// ENTRY (name+node for inline, node for interned) — see accessor.go.
)

// Expression node tags. A node is a tag byte followed by a tag-specific payload;
// child expressions are encoded inline (pre-order), so decoding is a recursive
// descent and encoding a recursive walk.
const (
	nUndefined = 0x00 // (no payload)
	nError     = 0x01 // (no payload)
	nBoolFalse = 0x02 // (no payload)
	nBoolTrue  = 0x03 // (no payload)
	nInt       = 0x04 // + zigzag varint int64
	nReal      = 0x05 // + 8 bytes IEEE-754 little-endian float64
	nString    = 0x06 // + uvarint(len) + bytes (string VALUE, not interned)
	nAttrRef   = 0x07 // + scope byte + uvarint(name internID)
	nBinOp     = 0x08 // + op byte + node(left) + node(right)
	nUnOp      = 0x09 // + op byte + node(expr)
	nList      = 0x0A // + uvarint(n) + node * n
	nRecord    = 0x0B // + <nested ad body: same layout, never standalone>
	nFunc      = 0x0C // + uvarint(name internID) + uvarint(argc) + node * argc
	nCond      = 0x0D // + node(cond) + node(true) + node(false)
	nElvis     = 0x0E // + node(left) + node(right)
	nSelect    = 0x0F // + node(record) + uvarint(attr internID)
	nSubscript = 0x10 // + node(container) + node(index)
	nParen     = 0x11 // + node(inner)
	nBinOpStr  = 0x12 // + uvarint(len) + op bytes + node(left) + node(right)  (fallback)
	nUnOpStr   = 0x13 // + uvarint(len) + op bytes + node(expr)                (fallback)
	// Inline-name node variants (used in flagInlineNames ads): identical to the
	// interned forms but with the name stored inline instead of as an interned id.
	nAttrRefStr = 0x14 // + scope byte + uvarint(len) + name bytes
	nFuncStr    = 0x15 // + uvarint(len) + name bytes + uvarint(argc) + node * argc
	nSelectStr  = 0x16 // + node(record) + uvarint(len) + name bytes
)

// nameHash32 is a case-insensitive 32-bit hash of an attribute name, used in the
// hot header of inline-names ads (attribute names are case-insensitive, so the
// hash folds case). FNV-1a over the lowercased bytes.
func nameHash32(name string) uint32 {
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

// Binary operator ids. Kept as a closed table for density; any operator not in
// the table falls back to nBinOpStr / nUnOpStr with an inline string, so the
// format never fails to round-trip an unexpected operator.
var binOps = []string{
	"+", "-", "*", "/", "%",
	"<", ">", "<=", ">=", "==", "!=",
	"&", "|", "^", "<<", ">>", ">>>",
	"&&", "||", "is", "isnt",
}

var unOps = []string{"-", "+", "!", "~"}

var binOpID, unOpID map[string]byte

func init() {
	binOpID = make(map[string]byte, len(binOps))
	for i, op := range binOps {
		binOpID[op] = byte(i)
	}
	unOpID = make(map[string]byte, len(unOps))
	for i, op := range unOps {
		unOpID[op] = byte(i)
	}
}
