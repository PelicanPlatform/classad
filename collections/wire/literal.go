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

// AppendBoolNode, AppendIntNode, AppendRealNode, and AppendStringNode append a scalar literal
// wire node to dst and return the extended slice -- the inverse of LiteralValue. They let a
// caller reconstruct a wire node from a typed value (e.g. the per-segment schema decoder
// rebuilding a node from a fixed-layout field), producing bytes LiteralValue reads back
// exactly.
// AppendUndefinedNode appends the UNDEFINED literal node.
func AppendUndefinedNode(dst []byte) []byte { return append(dst, nUndefined) }

func AppendBoolNode(dst []byte, b bool) []byte {
	if b {
		return append(dst, nBoolTrue)
	}
	return append(dst, nBoolFalse)
}

func AppendIntNode(dst []byte, v int64) []byte {
	return binary.AppendVarint(append(dst, nInt), v)
}

func AppendRealNode(dst []byte, f float64) []byte {
	return binary.LittleEndian.AppendUint64(append(dst, nReal), math.Float64bits(f))
}

func AppendStringNode(dst []byte, s string) []byte {
	dst = binary.AppendUvarint(append(dst, nString), uint64(len(s)))
	return append(dst, s...)
}

// LiteralValue decodes node as a scalar literal, returning (lit, true) if node is
// one, or (_, false) if it is a list, record, or computed expression (which must
// be decoded via DecodeNode and evaluated). It is allocation-free except for the
// string case, which copies the string bytes.
// IsLiteralNode reports whether node is a scalar literal, WITHOUT decoding its value.
//
// It answers exactly what `_, ok := LiteralValue(node)` answers -- same tags, same payload
// validity checks -- and for a STRING that is the whole difference: LiteralValue copies the
// string out of the node to return it, so asking "is this a literal?" through LiteralValue
// allocated the string and threw it away. On a grouped scan that is one allocation per record
// for a question whose answer is in the first byte.
//
// Any tag it does not recognise falls through to NumberValue, which is the other half of what
// the scan calls a scalar (a parenthesised or signed number), so the two agree on every input.
func IsLiteralNode(node []byte) bool {
	if len(node) == 0 {
		return false
	}
	switch node[0] {
	case nUndefined, nError, nBoolFalse, nBoolTrue:
		return true
	case nInt:
		_, n := binary.Varint(node[1:])
		return n > 0
	case nReal:
		return len(node) >= 9
	case nString:
		l, n := binary.Uvarint(node[1:])
		return n > 0 && uint64(len(node)-1-n) >= l
	}
	_, ok := NumberValue(node)
	return ok
}

// StringNodeBytes returns a STRING node's bytes WITHOUT converting them to a Go string, so a
// caller that only needs to compare or hash them pays no allocation.
//
// It exists because LiteralValue cannot do this: its Literal carries a string, and building one
// from the node copies. On a grouped scan that copy is per RECORD -- the group key is then
// interned, which keeps one copy per distinct value, so the per-record copy was thrown away
// immediately. A map lookup keyed `m[string(b)]` does not allocate (the compiler elides it), so
// a caller can go straight from these bytes to its own table.
//
// THE SLICE ALIASES node, which for a scan aliases a block's decompressed region. It is valid
// only until that region is reused, so a caller that RETAINS the value must copy it -- which is
// exactly what interning does on insert, and only on insert.
func StringNodeBytes(node []byte) ([]byte, bool) {
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

// NumberValue returns the number a node denotes without evaluating anything: an int or real
// literal under any parentheses and unary minus or plus.
//
// The encoder folds `-<numeric literal>` into a signed literal, so a node written today spells a
// negative number as a literal and LiteralValue alone reads it. This stays because a record is
// read as it was written: a segment sealed before that fold still carries `-5` as unary minus
// over 5, as does any spelling the fold leaves alone (`-(5)`, `+5`), and a reader summarizing
// numeric values -- a zone map -- would otherwise have to treat those as expressions.
//
// The result is what the evaluator computes for the node: minus negates (an int wraps, as int64
// negation does), plus is the identity. Any other operand -- a boolean included, which the
// evaluator's unary minus rejects -- is not a number here.
func NumberValue(node []byte) (Literal, bool) {
	neg := false
	for depth := 0; depth < 64 && len(node) > 0; depth++ {
		switch node[0] {
		case nInt, nReal:
			lit, ok := LiteralValue(node)
			if ok && neg {
				lit.Int, lit.Real = -lit.Int, -lit.Real
			}
			return lit, ok
		case nParen:
			node = node[1:]
		case nUnOp:
			if len(node) < 2 || int(node[1]) >= len(unOps) {
				return Literal{}, false
			}
			switch unOps[node[1]] {
			case "-":
				neg = !neg
			case "+":
			default:
				return Literal{}, false
			}
			node = node[2:]
		default:
			return Literal{}, false
		}
	}
	return Literal{}, false
}
