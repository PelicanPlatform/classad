package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/PelicanPlatform/classad/ast"
)

// ErrMalformed is returned when the input is not a well-formed wire ad.
var ErrMalformed = errors.New("wire: malformed encoding")

// maxDepth bounds recursion so a crafted deeply-nested input cannot overflow the
// goroutine stack during decode.
const maxDepth = 1000

type decoder struct {
	b       []byte
	pos     int
	resolve func(uint32) (string, bool) // id -> name (unused in inline mode)
	inline  bool                        // attribute keys are inline names
	open    Sealer                      // decrypts nEncrypted nodes; nil => they are errors
}

// Decode parses an ad encoded with Encode or EncodeInline. Interned ads resolve
// names via t; inline-names ads (flagInlineNames) are self-contained and t may be
// nil.
func Decode(b []byte, t *InternTable) (*ast.ClassAd, error) {
	d := &decoder{b: b}
	flags, err := d.headerFlags()
	if err != nil {
		return nil, err
	}
	if flags&flagStandalone != 0 {
		return nil, fmt.Errorf("%w: standalone ad; use DecodeStandalone", ErrMalformed)
	}
	if flags&flagInlineNames != 0 {
		d.inline = true
	} else {
		if t == nil {
			return nil, fmt.Errorf("%w: interned ad requires an intern table", ErrMalformed)
		}
		d.resolve = t.Name
	}
	return d.adBody(0)
}

// DecodeInline parses a self-contained inline-names ad (EncodeInline), requiring no
// intern table.
func DecodeInline(b []byte) (*ast.ClassAd, error) {
	return Decode(b, nil)
}

// DecodeStandalone parses a self-contained ad written by EncodeStandalone.
func DecodeStandalone(b []byte) (*ast.ClassAd, error) {
	d := &decoder{b: b}
	flags, err := d.headerFlags()
	if err != nil {
		return nil, err
	}
	if flags&flagStandalone == 0 {
		return nil, fmt.Errorf("%w: not a standalone ad", ErrMalformed)
	}
	n, err := d.uvarint()
	if err != nil {
		return nil, err
	}
	names := make([]string, n)
	for i := range names {
		s, err := d.readString()
		if err != nil {
			return nil, err
		}
		names[i] = s
	}
	d.resolve = func(id uint32) (string, bool) {
		if int(id) >= len(names) {
			return "", false
		}
		return names[id], true
	}
	return d.adBody(0)
}

// headerFlags reads magic/version/flags and returns the flags byte.
func (d *decoder) headerFlags() (byte, error) {
	if len(d.b) < 3 {
		return 0, fmt.Errorf("%w: short header", ErrMalformed)
	}
	if d.b[0] != magicByte {
		return 0, fmt.Errorf("%w: bad magic 0x%02x", ErrMalformed, d.b[0])
	}
	if d.b[1] != formatVer {
		return 0, fmt.Errorf("%w: unsupported version %d", ErrMalformed, d.b[1])
	}
	flags := d.b[2]
	d.pos = 3
	return flags, nil
}

// adBody reads [hotCount][hot entries][attrCount][attr entries].
func (d *decoder) adBody(depth int) (*ast.ClassAd, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("%w: nesting too deep", ErrMalformed)
	}
	hotCount, err := d.uvarint()
	if err != nil {
		return nil, err
	}
	for i := uint64(0); i < hotCount; i++ {
		if _, err := d.uvarint(); err != nil { // internID
			return nil, err
		}
		if _, err := d.uvarint(); err != nil { // offset
			return nil, err
		}
	}
	attrCount, err := d.uvarint()
	if err != nil {
		return nil, err
	}
	attrs := make([]*ast.AttributeAssignment, 0, attrCount)
	for i := uint64(0); i < attrCount; i++ {
		name, err := d.key()
		if err != nil {
			return nil, err
		}
		val, err := d.node(depth + 1)
		if err != nil {
			return nil, err
		}
		attrs = append(attrs, &ast.AttributeAssignment{Name: name, Value: val})
	}
	return &ast.ClassAd{Attributes: attrs}, nil
}

// key reads an attribute key: an inline name (inline mode) or an interned id
// resolved to its name.
func (d *decoder) key() (string, error) {
	if d.inline {
		return d.readString()
	}
	id, err := d.uvarint()
	if err != nil {
		return "", err
	}
	name, ok := d.resolve(uint32(id))
	if !ok {
		return "", fmt.Errorf("%w: unknown intern id %d", ErrMalformed, id)
	}
	return name, nil
}

// node reads a single expression node.
func (d *decoder) node(depth int) (ast.Expr, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("%w: expression too deep", ErrMalformed)
	}
	tag, err := d.byteAt()
	if err != nil {
		return nil, err
	}
	switch tag {
	case nUndefined:
		return &ast.UndefinedLiteral{}, nil
	case nError:
		return &ast.ErrorLiteral{}, nil
	case nBoolFalse:
		return &ast.BooleanLiteral{Value: false}, nil
	case nBoolTrue:
		return &ast.BooleanLiteral{Value: true}, nil
	case nInt:
		v, err := d.varint()
		if err != nil {
			return nil, err
		}
		return &ast.IntegerLiteral{Value: v}, nil
	case nReal:
		bits, err := d.uint64()
		if err != nil {
			return nil, err
		}
		return &ast.RealLiteral{Value: math.Float64frombits(bits)}, nil
	case nString:
		s, err := d.readString()
		if err != nil {
			return nil, err
		}
		return &ast.StringLiteral{Value: s}, nil
	case nAttrRef:
		scope, err := d.byteAt()
		if err != nil {
			return nil, err
		}
		id, err := d.uvarint()
		if err != nil {
			return nil, err
		}
		name, ok := d.resolve(uint32(id))
		if !ok {
			return nil, fmt.Errorf("%w: unknown attr-ref id %d", ErrMalformed, id)
		}
		return &ast.AttributeReference{Name: name, Scope: ast.AttributeScope(scope)}, nil
	case nAttrRefStr:
		scope, err := d.byteAt()
		if err != nil {
			return nil, err
		}
		name, err := d.readString()
		if err != nil {
			return nil, err
		}
		return &ast.AttributeReference{Name: name, Scope: ast.AttributeScope(scope)}, nil
	case nBinOp, nBinOpStr:
		op, err := d.opString(tag == nBinOpStr, binOps)
		if err != nil {
			return nil, err
		}
		left, err := d.node(depth + 1)
		if err != nil {
			return nil, err
		}
		right, err := d.node(depth + 1)
		if err != nil {
			return nil, err
		}
		return &ast.BinaryOp{Op: op, Left: left, Right: right}, nil
	case nUnOp, nUnOpStr:
		op, err := d.opString(tag == nUnOpStr, unOps)
		if err != nil {
			return nil, err
		}
		operand, err := d.node(depth + 1)
		if err != nil {
			return nil, err
		}
		return &ast.UnaryOp{Op: op, Expr: operand}, nil
	case nList:
		n, err := d.uvarint()
		if err != nil {
			return nil, err
		}
		if err := d.checkCount(n); err != nil {
			return nil, err
		}
		elems := make([]ast.Expr, 0, n)
		for i := uint64(0); i < n; i++ {
			el, err := d.node(depth + 1)
			if err != nil {
				return nil, err
			}
			elems = append(elems, el)
		}
		return &ast.ListLiteral{Elements: elems}, nil
	case nRecord:
		inner, err := d.adBody(depth + 1)
		if err != nil {
			return nil, err
		}
		return &ast.RecordLiteral{ClassAd: inner}, nil
	case nFunc:
		id, err := d.uvarint()
		if err != nil {
			return nil, err
		}
		name, ok := d.resolve(uint32(id))
		if !ok {
			return nil, fmt.Errorf("%w: unknown func id %d", ErrMalformed, id)
		}
		argc, err := d.uvarint()
		if err != nil {
			return nil, err
		}
		if err := d.checkCount(argc); err != nil {
			return nil, err
		}
		args := make([]ast.Expr, 0, argc)
		for i := uint64(0); i < argc; i++ {
			a, err := d.node(depth + 1)
			if err != nil {
				return nil, err
			}
			args = append(args, a)
		}
		return &ast.FunctionCall{Name: name, Args: args}, nil
	case nFuncStr:
		name, err := d.readString()
		if err != nil {
			return nil, err
		}
		argc, err := d.uvarint()
		if err != nil {
			return nil, err
		}
		if err := d.checkCount(argc); err != nil {
			return nil, err
		}
		args := make([]ast.Expr, 0, argc)
		for i := uint64(0); i < argc; i++ {
			a, err := d.node(depth + 1)
			if err != nil {
				return nil, err
			}
			args = append(args, a)
		}
		return &ast.FunctionCall{Name: name, Args: args}, nil
	case nCond:
		cond, err := d.node(depth + 1)
		if err != nil {
			return nil, err
		}
		t, err := d.node(depth + 1)
		if err != nil {
			return nil, err
		}
		f, err := d.node(depth + 1)
		if err != nil {
			return nil, err
		}
		return &ast.ConditionalExpr{Condition: cond, TrueExpr: t, FalseExpr: f}, nil
	case nElvis:
		left, err := d.node(depth + 1)
		if err != nil {
			return nil, err
		}
		right, err := d.node(depth + 1)
		if err != nil {
			return nil, err
		}
		return &ast.ElvisExpr{Left: left, Right: right}, nil
	case nSelect:
		rec, err := d.node(depth + 1)
		if err != nil {
			return nil, err
		}
		id, err := d.uvarint()
		if err != nil {
			return nil, err
		}
		name, ok := d.resolve(uint32(id))
		if !ok {
			return nil, fmt.Errorf("%w: unknown select attr id %d", ErrMalformed, id)
		}
		return &ast.SelectExpr{Record: rec, Attr: name}, nil
	case nSelectStr:
		rec, err := d.node(depth + 1)
		if err != nil {
			return nil, err
		}
		name, err := d.readString()
		if err != nil {
			return nil, err
		}
		return &ast.SelectExpr{Record: rec, Attr: name}, nil
	case nSubscript:
		container, err := d.node(depth + 1)
		if err != nil {
			return nil, err
		}
		index, err := d.node(depth + 1)
		if err != nil {
			return nil, err
		}
		return &ast.SubscriptExpr{Container: container, Index: index}, nil
	case nParen:
		inner, err := d.node(depth + 1)
		if err != nil {
			return nil, err
		}
		return &ast.ParenExpr{Inner: inner}, nil
	case nEncrypted:
		nonce, err := d.readStringBytes()
		if err != nil {
			return nil, err
		}
		ct, err := d.readStringBytes()
		if err != nil {
			return nil, err
		}
		if d.open == nil {
			return nil, fmt.Errorf("%w: encrypted attribute without a decryption key", ErrMalformed)
		}
		pt, err := d.open.Open(nonce, ct)
		if err != nil {
			return nil, fmt.Errorf("%w: decrypting attribute: %v", ErrMalformed, err)
		}
		// Decode the recovered node with a sub-decoder in the same mode (and with the
		// same key, so nested encryption also opens).
		sub := &decoder{b: pt, inline: d.inline, resolve: d.resolve, open: d.open}
		return sub.node(depth + 1)
	default:
		return nil, fmt.Errorf("%w: unknown node tag 0x%02x", ErrMalformed, tag)
	}
}

// opString returns the operator: either an inline string (fallback tag) or the
// table entry for a byte id.
func (d *decoder) opString(inline bool, table []string) (string, error) {
	if inline {
		return d.readString()
	}
	id, err := d.byteAt()
	if err != nil {
		return "", err
	}
	if int(id) >= len(table) {
		return "", fmt.Errorf("%w: bad operator id %d", ErrMalformed, id)
	}
	return table[id], nil
}

// --- primitive readers ---

func (d *decoder) byteAt() (byte, error) {
	if d.pos >= len(d.b) {
		return 0, fmt.Errorf("%w: unexpected end of input", ErrMalformed)
	}
	c := d.b[d.pos]
	d.pos++
	return c, nil
}

func (d *decoder) uvarint() (uint64, error) {
	v, n := binary.Uvarint(d.b[d.pos:])
	if n <= 0 {
		return 0, fmt.Errorf("%w: bad uvarint", ErrMalformed)
	}
	d.pos += n
	return v, nil
}

func (d *decoder) varint() (int64, error) {
	v, n := binary.Varint(d.b[d.pos:])
	if n <= 0 {
		return 0, fmt.Errorf("%w: bad varint", ErrMalformed)
	}
	d.pos += n
	return v, nil
}

func (d *decoder) uint64() (uint64, error) {
	if d.pos+8 > len(d.b) {
		return 0, fmt.Errorf("%w: short float64", ErrMalformed)
	}
	v := binary.LittleEndian.Uint64(d.b[d.pos:])
	d.pos += 8
	return v, nil
}

func (d *decoder) readString() (string, error) {
	b, err := d.readStringBytes()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// readStringBytes reads a uvarint-length-prefixed string as a subslice of the
// decoder's buffer (no allocation). The bytes alias d.b, so a caller that needs to
// retain them past the next mutation must copy. Used by the append-based renderer,
// which consumes each string immediately.
func (d *decoder) readStringBytes() ([]byte, error) {
	n, err := d.uvarint()
	if err != nil {
		return nil, err
	}
	if n > uint64(len(d.b)-d.pos) {
		return nil, fmt.Errorf("%w: string length %d exceeds remaining input", ErrMalformed, n)
	}
	b := d.b[d.pos : d.pos+int(n)]
	d.pos += int(n)
	return b, nil
}

// checkCount rejects element/argument counts larger than the remaining bytes,
// so a crafted huge count cannot trigger a giant make() before we run out of
// input (each element needs at least one byte).
func (d *decoder) checkCount(n uint64) error {
	if n > uint64(len(d.b)-d.pos) {
		return fmt.Errorf("%w: count %d exceeds remaining input", ErrMalformed, n)
	}
	return nil
}
