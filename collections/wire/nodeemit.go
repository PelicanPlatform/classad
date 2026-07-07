package wire

import (
	"encoding/binary"
	"math"
)

// NodeEmitter builds one wire expression node into a reusable buffer, for a parser
// that emits wire directly from text rather than via an ast.Expr. The wire form is
// prefix (a node's tag precedes its children), but expression parsing is infix, so
// a caller records Mark() before parsing an operand and, once the enclosing
// operator is known, calls a Wrap* method to insert the operator's tag/header
// before that already-emitted operand. Names are interned as they are emitted.
//
// Byte-for-byte identical to encoder.node for the same expression, so a node it
// builds decodes with DecodeNode.
type NodeEmitter struct {
	buf     []byte
	t       *InternTable
	inline  bool
	scratch []byte // reused staging buffer for multi-byte Wrap inserts
}

// NewNodeEmitter returns an emitter interning names into t.
func NewNodeEmitter(t *InternTable) *NodeEmitter { return &NodeEmitter{t: t} }

// NewInlineNodeEmitter returns an emitter writing inline-name node variants (no
// interning), matching the persistent-store encoding.
func NewInlineNodeEmitter() *NodeEmitter { return &NodeEmitter{inline: true} }

// Reset clears the emitter for another node, keeping the buffer.
func (e *NodeEmitter) Reset() { e.buf = e.buf[:0] }

// Bytes returns the emitted node bytes (valid until the next Reset).
func (e *NodeEmitter) Bytes() []byte { return e.buf }

// Mark returns the current buffer position, to pass to a later Wrap* call.
func (e *NodeEmitter) Mark() int { return len(e.buf) }

func (e *NodeEmitter) putString(s string) {
	e.buf = binary.AppendUvarint(e.buf, uint64(len(s)))
	e.buf = append(e.buf, s...)
}

// insertAt inserts ins at position mark, shifting the bytes at/after mark right.
func (e *NodeEmitter) insertAt(mark int, ins []byte) {
	old := len(e.buf)
	e.buf = append(e.buf, ins...) // grow by len(ins); this content is overwritten below
	copy(e.buf[mark+len(ins):], e.buf[mark:old])
	copy(e.buf[mark:], ins)
}

// --- leaf literals (tag then payload, emitted directly) ---

func (e *NodeEmitter) Int(v int64) {
	e.buf = append(e.buf, nInt)
	e.buf = binary.AppendVarint(e.buf, v)
}

func (e *NodeEmitter) Real(v float64) {
	e.buf = append(e.buf, nReal)
	e.buf = binary.LittleEndian.AppendUint64(e.buf, math.Float64bits(v))
}

func (e *NodeEmitter) Str(v string) {
	e.buf = append(e.buf, nString)
	e.putString(v)
}

func (e *NodeEmitter) Bool(v bool) {
	if v {
		e.buf = append(e.buf, nBoolTrue)
	} else {
		e.buf = append(e.buf, nBoolFalse)
	}
}

func (e *NodeEmitter) Undef() { e.buf = append(e.buf, nUndefined) }
func (e *NodeEmitter) Err()   { e.buf = append(e.buf, nError) }

// AttrRef emits an attribute reference with the given scope (ast.AttributeScope).
func (e *NodeEmitter) AttrRef(scope byte, name string) {
	if e.inline {
		e.buf = append(e.buf, nAttrRefStr, scope)
		e.putString(name)
	} else {
		e.buf = append(e.buf, nAttrRef, scope)
		e.buf = binary.AppendUvarint(e.buf, uint64(e.t.Intern(name)))
	}
}

// UnOp emits a unary-operator tag; the operand is emitted after (source-prefix, so
// no insertion needed).
func (e *NodeEmitter) UnOp(op string) {
	if id, ok := unOpID[op]; ok {
		e.buf = append(e.buf, nUnOp, id)
	} else {
		e.buf = append(e.buf, nUnOpStr)
		e.putString(op)
	}
}

// --- Wrap*: insert an operator's tag/header before the operand(s) at mark ---

// WrapBinOp turns the operand at [mark:end] plus the operand appended after it into
// a binary-op node by inserting the op tag at mark.
func (e *NodeEmitter) WrapBinOp(mark int, op string) {
	if id, ok := binOpID[op]; ok {
		e.insertAt(mark, []byte{nBinOp, id})
		return
	}
	e.scratch = append(e.scratch[:0], nBinOpStr)
	e.scratch = binary.AppendUvarint(e.scratch, uint64(len(op)))
	e.scratch = append(e.scratch, op...)
	e.insertAt(mark, e.scratch)
}

// WrapParen wraps the operand at [mark:end] in an explicit-parenthesis node.
func (e *NodeEmitter) WrapParen(mark int) { e.insertAt(mark, []byte{nParen}) }

// WrapCond wraps cond+true+false (cond at mark, true/false appended after) into a
// conditional node.
func (e *NodeEmitter) WrapCond(mark int) { e.insertAt(mark, []byte{nCond}) }

// WrapElvis wraps left+right (left at mark, right appended after) into an elvis node.
func (e *NodeEmitter) WrapElvis(mark int) { e.insertAt(mark, []byte{nElvis}) }

// WrapSubscript wraps container+index (container at mark, index appended after).
func (e *NodeEmitter) WrapSubscript(mark int) { e.insertAt(mark, []byte{nSubscript}) }

// WrapList wraps the count elements at [mark:end] into a list node.
func (e *NodeEmitter) WrapList(mark, count int) {
	e.scratch = append(e.scratch[:0], nList)
	e.scratch = binary.AppendUvarint(e.scratch, uint64(count))
	e.insertAt(mark, e.scratch)
}

// FuncNameID interns a function name and returns its id. A caller emits a call by
// interning the name with FuncNameID *before* parsing/emitting the argument nodes
// (so the name's id is assigned ahead of the args, matching the reference encoder's
// pre-order), then calls WrapFuncID after the args. Returns 0 in inline mode, where
// the name is written verbatim and no interning happens.
func (e *NodeEmitter) FuncNameID(name string) uint32 {
	if e.inline {
		return 0
	}
	return e.t.Intern(name)
}

// WrapFuncID wraps the argc argument nodes at [mark:end] into a function call,
// using the name id pre-interned by FuncNameID (interned mode) or name verbatim
// (inline mode).
func (e *NodeEmitter) WrapFuncID(mark int, name string, id uint32, argc int) {
	e.scratch = e.scratch[:0]
	if e.inline {
		e.scratch = append(e.scratch, nFuncStr)
		e.scratch = binary.AppendUvarint(e.scratch, uint64(len(name)))
		e.scratch = append(e.scratch, name...)
	} else {
		e.scratch = append(e.scratch, nFunc)
		e.scratch = binary.AppendUvarint(e.scratch, uint64(id))
	}
	e.scratch = binary.AppendUvarint(e.scratch, uint64(argc))
	e.insertAt(mark, e.scratch)
}

// WrapSelect wraps the record at [mark:end] into a select of attr: the select tag
// is inserted at mark and the attribute id/name appended after the record (matching
// the nSelect <record> <attr> layout).
func (e *NodeEmitter) WrapSelect(mark int, attr string) {
	if e.inline {
		e.insertAt(mark, []byte{nSelectStr})
		e.putString(attr)
	} else {
		e.insertAt(mark, []byte{nSelect})
		e.buf = binary.AppendUvarint(e.buf, uint64(e.t.Intern(attr)))
	}
}
