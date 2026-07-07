package wire

import (
	"math"
	"strconv"

	"github.com/PelicanPlatform/classad/ast"
)

// AppendNodeText appends the canonical ClassAd text of a wire node to dst and
// returns the extended slice. It is byte-for-byte identical to
// DecodeNode(node, t).String(), but renders straight from the wire with no
// ast.Expr and no per-node string: the AST-free query-result path uses it for the
// non-scalar values (lists, records, computed expressions) that LiteralValue
// cannot format directly. Interned name ids resolve via t.
func AppendNodeText(dst, node []byte, t *InternTable) ([]byte, error) {
	d := &decoder{b: node, resolve: t.Name}
	return d.appendNode(dst, 0)
}

// AppendNodeTextInline is AppendNodeText for inline-names nodes (self-contained,
// no intern table) — the form stored by the persistent store.
func AppendNodeTextInline(dst, node []byte) ([]byte, error) {
	d := &decoder{b: node, inline: true}
	return d.appendNode(dst, 0)
}

// appendNode appends one expression node's text to dst, mirroring decoder.node
// (which builds an ast.Expr) and the ast String() methods it would call. Keeping
// the two walks structurally identical is deliberate: any node shape decode.go
// handles, this must render the same way.
func (d *decoder) appendNode(dst []byte, depth int) ([]byte, error) {
	if depth > maxDepth {
		return dst, ErrMalformed
	}
	tag, err := d.byteAt()
	if err != nil {
		return dst, err
	}
	switch tag {
	case nUndefined:
		return append(dst, "undefined"...), nil
	case nError:
		return append(dst, "error"...), nil
	case nBoolFalse:
		return append(dst, "false"...), nil
	case nBoolTrue:
		return append(dst, "true"...), nil
	case nInt:
		v, err := d.varint()
		if err != nil {
			return dst, err
		}
		return strconv.AppendInt(dst, v, 10), nil
	case nReal:
		bits, err := d.uint64()
		if err != nil {
			return dst, err
		}
		return strconv.AppendFloat(dst, math.Float64frombits(bits), 'g', -1, 64), nil
	case nString:
		s, err := d.readString()
		if err != nil {
			return dst, err
		}
		return ast.AppendQuoteString(dst, s), nil
	case nAttrRef, nAttrRefStr:
		scope, err := d.byteAt()
		if err != nil {
			return dst, err
		}
		name, err := d.refName(tag == nAttrRefStr)
		if err != nil {
			return dst, err
		}
		dst = appendScope(dst, scope)
		return ast.AppendQuoteAttributeName(dst, name), nil
	case nBinOp, nBinOpStr:
		op, err := d.opString(tag == nBinOpStr, binOps)
		if err != nil {
			return dst, err
		}
		// ast.BinaryOp.String: "(left op right)".
		dst = append(dst, '(')
		if dst, err = d.appendNode(dst, depth+1); err != nil {
			return dst, err
		}
		dst = append(dst, ' ')
		dst = append(dst, op...)
		dst = append(dst, ' ')
		if dst, err = d.appendNode(dst, depth+1); err != nil {
			return dst, err
		}
		return append(dst, ')'), nil
	case nUnOp, nUnOpStr:
		op, err := d.opString(tag == nUnOpStr, unOps)
		if err != nil {
			return dst, err
		}
		// ast.UnaryOp.String: "(op expr)" with no space between op and expr.
		dst = append(dst, '(')
		dst = append(dst, op...)
		if dst, err = d.appendNode(dst, depth+1); err != nil {
			return dst, err
		}
		return append(dst, ')'), nil
	case nList:
		n, err := d.uvarint()
		if err != nil {
			return dst, err
		}
		if err := d.checkCount(n); err != nil {
			return dst, err
		}
		// ast.ListLiteral.String: "{e0, e1, ...}".
		dst = append(dst, '{')
		for i := uint64(0); i < n; i++ {
			if i > 0 {
				dst = append(dst, ',', ' ')
			}
			if dst, err = d.appendNode(dst, depth+1); err != nil {
				return dst, err
			}
		}
		return append(dst, '}'), nil
	case nRecord:
		return d.appendAdBody(dst, depth+1)
	case nFunc, nFuncStr:
		name, err := d.refName(tag == nFuncStr)
		if err != nil {
			return dst, err
		}
		argc, err := d.uvarint()
		if err != nil {
			return dst, err
		}
		if err := d.checkCount(argc); err != nil {
			return dst, err
		}
		// ast.FunctionCall.String: "name(a0, a1, ...)".
		dst = append(dst, name...)
		dst = append(dst, '(')
		for i := uint64(0); i < argc; i++ {
			if i > 0 {
				dst = append(dst, ',', ' ')
			}
			if dst, err = d.appendNode(dst, depth+1); err != nil {
				return dst, err
			}
		}
		return append(dst, ')'), nil
	case nCond:
		// ast.ConditionalExpr.String: "(cond ? t : f)".
		dst = append(dst, '(')
		if dst, err = d.appendNode(dst, depth+1); err != nil {
			return dst, err
		}
		dst = append(dst, " ? "...)
		if dst, err = d.appendNode(dst, depth+1); err != nil {
			return dst, err
		}
		dst = append(dst, " : "...)
		if dst, err = d.appendNode(dst, depth+1); err != nil {
			return dst, err
		}
		return append(dst, ')'), nil
	case nElvis:
		// ast.ElvisExpr.String: "(left ?: right)".
		dst = append(dst, '(')
		if dst, err = d.appendNode(dst, depth+1); err != nil {
			return dst, err
		}
		dst = append(dst, " ?: "...)
		if dst, err = d.appendNode(dst, depth+1); err != nil {
			return dst, err
		}
		return append(dst, ')'), nil
	case nSelect, nSelectStr:
		// ast.SelectExpr.String: "record.attr", but a bare int/real record needs a
		// space before '.' so "0.A" does not re-lex as a real. The AST switches on
		// the record's concrete type; the wire's record-node tag byte is the same
		// signal, so peek it before rendering.
		recTag := byte(0)
		if d.pos < len(d.b) {
			recTag = d.b[d.pos]
		}
		if dst, err = d.appendNode(dst, depth+1); err != nil {
			return dst, err
		}
		if recTag == nInt || recTag == nReal {
			dst = append(dst, ' ')
		}
		dst = append(dst, '.')
		name, err := d.refName(tag == nSelectStr)
		if err != nil {
			return dst, err
		}
		return ast.AppendQuoteAttributeName(dst, name), nil
	case nSubscript:
		// ast.SubscriptExpr.String: "container[index]".
		if dst, err = d.appendNode(dst, depth+1); err != nil {
			return dst, err
		}
		dst = append(dst, '[')
		if dst, err = d.appendNode(dst, depth+1); err != nil {
			return dst, err
		}
		return append(dst, ']'), nil
	case nParen:
		// ast.ParenExpr.String is transparent (the debug unparser re-parenthesizes
		// operators itself), so render only the inner node.
		return d.appendNode(dst, depth+1)
	default:
		return dst, ErrMalformed
	}
}

// appendAdBody renders a record body ([hotCount][hot entries][attrCount][attrs])
// to "[k0 = v0; k1 = v1; ...]", matching ast.ClassAd.String via
// ast.AttributeAssignment.String.
func (d *decoder) appendAdBody(dst []byte, depth int) ([]byte, error) {
	if depth > maxDepth {
		return dst, ErrMalformed
	}
	hotCount, err := d.uvarint()
	if err != nil {
		return dst, err
	}
	for i := uint64(0); i < hotCount; i++ {
		if _, err := d.uvarint(); err != nil { // internID / nameHash
			return dst, err
		}
		if _, err := d.uvarint(); err != nil { // offset
			return dst, err
		}
	}
	attrCount, err := d.uvarint()
	if err != nil {
		return dst, err
	}
	dst = append(dst, '[')
	for i := uint64(0); i < attrCount; i++ {
		if i > 0 {
			dst = append(dst, ';', ' ')
		}
		name, err := d.key()
		if err != nil {
			return dst, err
		}
		dst = ast.AppendQuoteAttributeName(dst, name)
		dst = append(dst, " = "...)
		if dst, err = d.appendNode(dst, depth+1); err != nil {
			return dst, err
		}
	}
	return append(dst, ']'), nil
}

// refName reads a name that is either an inline string (str variant) or an
// interned id resolved via d.resolve — the name-source split shared by the
// attr-ref / func / select node pairs.
func (d *decoder) refName(str bool) (string, error) {
	if str {
		return d.readString()
	}
	id, err := d.uvarint()
	if err != nil {
		return "", err
	}
	name, ok := d.resolve(uint32(id))
	if !ok {
		return "", ErrMalformed
	}
	return name, nil
}

// appendScope prepends the scope qualifier for an attribute reference, matching
// ast.AttributeReference.String.
func appendScope(dst []byte, scope byte) []byte {
	switch ast.AttributeScope(scope) {
	case ast.MyScope:
		return append(dst, "MY."...)
	case ast.TargetScope:
		return append(dst, "TARGET."...)
	case ast.ParentScope:
		return append(dst, "PARENT."...)
	default:
		return dst
	}
}
