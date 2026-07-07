package collections

import (
	"strconv"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// decodeAdRaw decodes a stored ad straight to old-ClassAd expression strings
// ("Name = Value"), plus its MyType/TargetType values, WITHOUT building an
// ast.ClassAd or classad.ClassAd. Scalar literals -- the vast majority of a
// collector's attributes -- are formatted directly from the wire node; only
// computed expressions fall back to decoding an ast.Expr. This is the send-side
// analogue of the AST-free ingest path (StreamEncoder), for streaming query
// results to the wire.
//
// It returns ok=false for inline-name (persistent) ads, whose names are not in
// the collection's intern table; the caller should decode those via decodeAd.
func (c *Collection) decodeAdRaw(stored []byte, codec Codec, dst []byte) (exprs []string, myType, targetType string, ok bool) {
	if c.inline {
		return nil, "", "", false
	}
	wireBytes, err := codec.Decompress(dst, stored)
	if err != nil {
		return nil, "", "", false
	}
	ad := wire.Ad(wireBytes)
	good := true
	ad.ForEach(func(id uint32, node []byte) bool {
		name, nok := c.intern.Name(id)
		if !nok {
			good = false
			return false
		}
		// MyType/TargetType are sent as the two trailing string fields, not as
		// numbered expressions -- carry their raw string value out separately.
		if name == "MyType" || name == "TargetType" {
			if lit, lok := wire.LiteralValue(node); lok && lit.Kind == wire.LitString {
				if name == "MyType" {
					myType = lit.Str
				} else {
					targetType = lit.Str
				}
				return true
			}
		}
		val, verr := formatWireValue(node, c.intern)
		if verr != nil {
			good = false
			return false
		}
		exprs = append(exprs, name+" = "+val)
		return true
	})
	if !good {
		return nil, "", "", false
	}
	return exprs, myType, targetType, true
}

// formatWireValue renders a wire node to canonical ClassAd value text, matching
// ast.Expr.String(). Literals are formatted directly (allocation-light); computed
// expressions are decoded to an ast.Expr and rendered.
func formatWireValue(node []byte, table *wire.InternTable) (string, error) {
	if lit, ok := wire.LiteralValue(node); ok {
		switch lit.Kind {
		case wire.LitInt:
			return strconv.FormatInt(lit.Int, 10), nil
		case wire.LitReal:
			return strconv.FormatFloat(lit.Real, 'g', -1, 64), nil
		case wire.LitString:
			return ast.QuoteString(lit.Str), nil
		case wire.LitBool:
			if lit.Bool {
				return "true", nil
			}
			return "false", nil
		case wire.LitUndef:
			return "undefined", nil
		case wire.LitError:
			return "error", nil
		}
	}
	e, err := wire.DecodeNode(node, table)
	if err != nil {
		return "", err
	}
	return e.String(), nil
}
