package wire

import (
	"encoding/binary"
	"math"

	"github.com/PelicanPlatform/classad/ast"
)

// encoder appends bytes to buf. When inline is false, names (attribute keys, and
// the names inside attribute-reference/function/select nodes) are interned into t
// and stored as ids. When inline is true, names are stored verbatim and t is
// unused, producing a self-contained ad (see flagInlineNames).
type encoder struct {
	buf    []byte
	t      *InternTable
	inline bool
}

// putKey writes an attribute key: an interned id, or an inline name in inline mode.
func (e *encoder) putKey(name string) {
	if e.inline {
		e.putString(name)
	} else {
		e.buf = binary.AppendUvarint(e.buf, uint64(e.t.Intern(name)))
	}
}

// Encode appends the binary form of ad to dst and returns the extended slice,
// using the shared intern table t (mutating it to add any new names). The
// resulting bytes are NOT self-contained: decoding requires the same table.
func Encode(dst []byte, ad *ast.ClassAd, t *InternTable) []byte {
	e := encoder{buf: dst, t: t}
	e.buf = append(e.buf, magicByte, formatVer, 0 /* flags */)
	e.adBody(ad)
	return e.buf
}

// EncodeWithHot is Encode with a populated hot header: attributes whose interned
// id is in hot are indexed by a (id, entries-relative offset) pair written before
// the body, so Ad.Lookup finds them in O(1) instead of scanning. The body layout
// is otherwise identical to Encode, so the result decodes with Decode and the hot
// header is a pure read-time accelerator. hot may be nil (equivalent to Encode).
func EncodeWithHot(dst []byte, ad *ast.ClassAd, t *InternTable, hot map[uint32]struct{}) []byte {
	return EncodeWithHotClosure(dst, ad, t, hot, false)
}

// EncodeWithHotClosure is EncodeWithHot that, when closureComplete, marks the ad with
// flagHotClosure -- a promise that hot holds the complete match closure, so a matcher
// can read it via ForEachHot without scanning. Set closureComplete only when hot truly
// contains every attribute the match reads from ad (see collections' astClosure).
func EncodeWithHotClosure(dst []byte, ad *ast.ClassAd, t *InternTable, hot map[uint32]struct{}, closureComplete bool) []byte {
	if ad == nil {
		return Encode(dst, ad, t)
	}
	var extraFlags byte
	if closureComplete {
		extraFlags = flagHotClosure
	}
	// Write the entries (id, node)* into a scratch buffer, recording the
	// entries-relative offset of each hot attribute's node.
	e := encoder{t: t}
	var hots []hotPair
	for _, attr := range ad.Attributes {
		id := t.Intern(attr.Name)
		e.buf = binary.AppendUvarint(e.buf, uint64(id))
		nodeOff := len(e.buf) // offset of the node within the entries region
		e.node(attr.Value)
		if _, ok := hot[id]; ok {
			hots = append(hots, hotPair{id, uint32(nodeOff)})
		}
	}
	out := append(dst, magicByte, formatVer, extraFlags)
	out = binary.AppendUvarint(out, uint64(len(hots)))
	for _, h := range hots {
		out = binary.AppendUvarint(out, uint64(h.id))
		out = binary.AppendUvarint(out, uint64(h.off))
	}
	out = binary.AppendUvarint(out, uint64(len(ad.Attributes))) // attrCount
	out = append(out, e.buf...)                                 // entries region
	return out
}

// EncodeInline encodes ad with inline attribute names (no interning), producing a
// fully self-contained ad that DecodeInline reads with no InternTable. Used by the
// persistent store so on-disk records are recoverable without a shared table.
func EncodeInline(dst []byte, ad *ast.ClassAd) []byte {
	return EncodeInlineWithHot(dst, ad, nil)
}

// EncodeInlineWithHot is EncodeInline with a populated hot header: attributes whose
// (case-folded) name is in hot are indexed by a (nameHash32, entries-relative
// offset-to-entry) pair, so Ad.LookupByName finds them without scanning. hot keys
// must be lower-cased. hot may be nil.
func EncodeInlineWithHot(dst []byte, ad *ast.ClassAd, hot map[string]struct{}) []byte {
	e := encoder{inline: true}
	var hots []hotPair
	if ad != nil {
		for _, attr := range ad.Attributes {
			entryOff := len(e.buf) // offset of the (name, node) entry within the region
			e.putString(attr.Name)
			e.node(attr.Value)
			if _, ok := hot[foldASCII(attr.Name)]; ok {
				hots = append(hots, hotPair{nameHash32(attr.Name), uint32(entryOff)})
			}
		}
	}
	out := append(dst, magicByte, formatVer, flagInlineNames)
	out = binary.AppendUvarint(out, uint64(len(hots)))
	for _, h := range hots {
		out = binary.AppendUvarint(out, uint64(h.id)) // nameHash32
		out = binary.AppendUvarint(out, uint64(h.off))
	}
	n := 0
	if ad != nil {
		n = len(ad.Attributes)
	}
	out = binary.AppendUvarint(out, uint64(n)) // attrCount
	out = append(out, e.buf...)                // entries region
	return out
}

// foldASCII lower-cases ASCII letters in s (attribute names are case-insensitive).
func foldASCII(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if b == nil {
				b = []byte(s)
			}
			b[i] = c + ('a' - 'A')
		}
	}
	if b == nil {
		return s
	}
	return string(b)
}

// EncodeStandalone returns a self-contained encoding that embeds a minimal
// intern table, so it can be decoded with DecodeStandalone alone (e.g. for
// transport out of a collection).
func EncodeStandalone(ad *ast.ClassAd) []byte {
	t := NewInternTable()
	// First pass: encode the body against a fresh table so every referenced
	// name is interned, then prepend the table.
	e := encoder{t: t}
	e.adBody(ad)
	body := e.buf

	names := t.snapshotNames()
	out := make([]byte, 0, len(body)+16*len(names)+8)
	out = append(out, magicByte, formatVer, flagStandalone)
	out = binary.AppendUvarint(out, uint64(len(names)))
	for _, n := range names {
		out = binary.AppendUvarint(out, uint64(len(n)))
		out = append(out, n...)
	}
	out = append(out, body...)
	return out
}

// adBody writes [hotCount][hot entries][attrCount][attr entries]. Hot header is
// empty here; the store populates it at compaction. Used for both the top-level
// ad and nested records.
func (e *encoder) adBody(ad *ast.ClassAd) {
	e.buf = binary.AppendUvarint(e.buf, 0) // hotCount (populated by the store later)
	if ad == nil {
		e.buf = binary.AppendUvarint(e.buf, 0)
		return
	}
	e.buf = binary.AppendUvarint(e.buf, uint64(len(ad.Attributes)))
	for _, attr := range ad.Attributes {
		e.putKey(attr.Name)
		e.node(attr.Value)
	}
}

// node writes a single expression node (pre-order, self-delimiting).
func (e *encoder) node(expr ast.Expr) {
	switch v := expr.(type) {
	case nil:
		e.buf = append(e.buf, nUndefined)
	case *ast.UndefinedLiteral:
		e.buf = append(e.buf, nUndefined)
	case *ast.ErrorLiteral:
		e.buf = append(e.buf, nError)
	case *ast.BooleanLiteral:
		if v.Value {
			e.buf = append(e.buf, nBoolTrue)
		} else {
			e.buf = append(e.buf, nBoolFalse)
		}
	case *ast.IntegerLiteral:
		e.buf = append(e.buf, nInt)
		e.buf = binary.AppendVarint(e.buf, v.Value)
	case *ast.RealLiteral:
		e.buf = append(e.buf, nReal)
		e.buf = binary.LittleEndian.AppendUint64(e.buf, math.Float64bits(v.Value))
	case *ast.StringLiteral:
		e.buf = append(e.buf, nString)
		e.putString(v.Value)
	case *ast.AttributeReference:
		if e.inline {
			e.buf = append(e.buf, nAttrRefStr, byte(v.Scope))
			e.putString(v.Name)
		} else {
			e.buf = append(e.buf, nAttrRef, byte(v.Scope))
			e.buf = binary.AppendUvarint(e.buf, uint64(e.t.Intern(v.Name)))
		}
	case *ast.BinaryOp:
		if id, ok := binOpID[v.Op]; ok {
			e.buf = append(e.buf, nBinOp, id)
		} else {
			e.buf = append(e.buf, nBinOpStr)
			e.putString(v.Op)
		}
		e.node(v.Left)
		e.node(v.Right)
	case *ast.UnaryOp:
		if id, ok := unOpID[v.Op]; ok {
			e.buf = append(e.buf, nUnOp, id)
		} else {
			e.buf = append(e.buf, nUnOpStr)
			e.putString(v.Op)
		}
		e.node(v.Expr)
	case *ast.ListLiteral:
		e.buf = append(e.buf, nList)
		e.buf = binary.AppendUvarint(e.buf, uint64(len(v.Elements)))
		for _, el := range v.Elements {
			e.node(el)
		}
	case *ast.RecordLiteral:
		e.buf = append(e.buf, nRecord)
		e.adBody(v.ClassAd)
	case *ast.FunctionCall:
		if e.inline {
			e.buf = append(e.buf, nFuncStr)
			e.putString(v.Name)
		} else {
			e.buf = append(e.buf, nFunc)
			e.buf = binary.AppendUvarint(e.buf, uint64(e.t.Intern(v.Name)))
		}
		e.buf = binary.AppendUvarint(e.buf, uint64(len(v.Args)))
		for _, a := range v.Args {
			e.node(a)
		}
	case *ast.ConditionalExpr:
		e.buf = append(e.buf, nCond)
		e.node(v.Condition)
		e.node(v.TrueExpr)
		e.node(v.FalseExpr)
	case *ast.ElvisExpr:
		e.buf = append(e.buf, nElvis)
		e.node(v.Left)
		e.node(v.Right)
	case *ast.SelectExpr:
		if e.inline {
			e.buf = append(e.buf, nSelectStr)
			e.node(v.Record)
			e.putString(v.Attr)
		} else {
			e.buf = append(e.buf, nSelect)
			e.node(v.Record)
			e.buf = binary.AppendUvarint(e.buf, uint64(e.t.Intern(v.Attr)))
		}
	case *ast.SubscriptExpr:
		e.buf = append(e.buf, nSubscript)
		e.node(v.Container)
		e.node(v.Index)
	case *ast.ParenExpr:
		e.buf = append(e.buf, nParen)
		e.node(v.Inner)
	default:
		// Unknown node type: encode as undefined rather than panic. The decoder
		// side treats this as an undefined literal. This is a defensive default;
		// every ast.Expr type above is handled.
		e.buf = append(e.buf, nUndefined)
	}
}

func (e *encoder) putString(s string) {
	e.buf = binary.AppendUvarint(e.buf, uint64(len(s)))
	e.buf = append(e.buf, s...)
}
