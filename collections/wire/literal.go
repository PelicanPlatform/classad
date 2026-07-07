package wire

import (
	"encoding/binary"
	"math"
)

// LitKind identifies the kind of a scalar literal node.
type LitKind uint8

const (
	LitUndef LitKind = iota
	LitError
	LitBool
	LitInt
	LitReal
	LitString
)

// Literal is a decoded scalar literal value, described without depending on the
// classad package so wire stays decoupled from it. Non-scalar nodes (lists,
// records, and computed expressions) are not literals.
type Literal struct {
	Kind LitKind
	Bool bool
	Int  int64
	Real float64
	Str  string
}

// StringLiteralValue returns the raw value bytes of a string-literal node as a
// subslice of node (no copy), with ok=true, or (nil, false) if node is not a
// string literal. The returned bytes alias node and are valid only while node is.
// It lets a caller quote a string value straight from the wire (via
// ast.AppendQuoteStringBytes) without the string allocation LiteralValue's Str
// field would incur.
func StringLiteralValue(node []byte) ([]byte, bool) {
	if len(node) == 0 || node[0] != nString {
		return nil, false
	}
	l, n := binary.Uvarint(node[1:])
	if n <= 0 || uint64(len(node)-1-n) < l {
		return nil, false
	}
	start := 1 + n
	return node[start : start+int(l)], true
}

// LiteralValue decodes node as a scalar literal, returning (lit, true) if node is
// one, or (_, false) if it is a list, record, or computed expression (which must
// be decoded via DecodeNode and evaluated). It is allocation-free except for the
// string case, which copies the string bytes.
func LiteralValue(node []byte) (Literal, bool) {
	if len(node) == 0 {
		return Literal{}, false
	}
	switch node[0] {
	case nUndefined:
		return Literal{Kind: LitUndef}, true
	case nError:
		return Literal{Kind: LitError}, true
	case nBoolFalse:
		return Literal{Kind: LitBool, Bool: false}, true
	case nBoolTrue:
		return Literal{Kind: LitBool, Bool: true}, true
	case nInt:
		v, n := binary.Varint(node[1:])
		if n <= 0 {
			return Literal{}, false
		}
		return Literal{Kind: LitInt, Int: v}, true
	case nReal:
		if len(node) < 9 {
			return Literal{}, false
		}
		return Literal{Kind: LitReal, Real: math.Float64frombits(binary.LittleEndian.Uint64(node[1:]))}, true
	case nString:
		l, n := binary.Uvarint(node[1:])
		if n <= 0 || uint64(len(node)-1-n) < l {
			return Literal{}, false
		}
		start := 1 + n
		return Literal{Kind: LitString, Str: string(node[start : start+int(l)])}, true
	default:
		return Literal{}, false
	}
}
