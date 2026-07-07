package collections

import (
	"iter"
	"strconv"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// RawAd is a query result rendered as old-ClassAd wire parts -- the "Name = Value"
// expression strings plus MyType/TargetType -- decoded straight from the stored
// form with no ast/classad ClassAd. A collector can hand these to
// message.PutClassAdRaw to stream a result set without ever building an AST. The
// Exprs slice is freshly allocated per ad; the strings do not alias scan buffers.
type RawAd struct {
	Exprs      []string
	MyType     string
	TargetType string
}

// QueryRaw is Query, but yields each matching ad as RawAd (AST-free) instead of a
// *classad.ClassAd. It uses the same index/scan machinery -- an indexed lookup
// still visits only candidate ads -- so it does not regress selective queries.
// Inline-name (persistent) collections have no intern table, so QueryRaw yields
// nothing for them; callers that must support those should use Query.
func (c *Collection) QueryRaw(q *vm.Query) iter.Seq[RawAd] {
	return func(yield func(RawAd) bool) {
		if c.inline {
			return
		}
		plan := q.ReadPlan()
		ws := &wireScope{ctx: c}
		qp := queryPlan{
			q:        q,
			plan:     plan,
			m:        q.Matcher(),
			wireOK:   q.Native() && plan.PartialSafe,
			ws:       ws,
			resolver: ws.resolve,
		}
		probes := q.Probes()
		c.demand.record(probes)
		usable := c.planIndex(probes)
		emit := c.yieldRaw(yield)
		for _, sh := range c.shards {
			var cont bool
			if len(usable) > 0 {
				cont = c.scanShardIndexed(sh, usable, qp, emit)
			} else {
				cont = c.scanShard(sh, qp, emit)
			}
			if !cont {
				return
			}
		}
	}
}

// ScanRaw is Scan, yielding every ad as RawAd (AST-free).
func (c *Collection) ScanRaw() iter.Seq[RawAd] {
	return func(yield func(RawAd) bool) {
		if c.inline {
			return
		}
		q := queryPlan{}
		emit := c.yieldRaw(yield)
		for _, sh := range c.shards {
			if !c.scanShard(sh, q, emit) {
				return
			}
		}
	}
}

// yieldRaw is the scan emit callback for the raw path: format the decompressed
// wire bytes straight to old-ClassAd text and yield them.
func (c *Collection) yieldRaw(yield func(RawAd) bool) func(w []byte) bool {
	return func(w []byte) bool {
		exprs, myType, targetType, ok := c.formatWireAd(w)
		if !ok {
			return true // undecodable record: skip, keep scanning
		}
		return yield(RawAd{Exprs: exprs, MyType: myType, TargetType: targetType})
	}
}

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
	return c.formatWireAd(wireBytes)
}

// formatWireAd renders already-decompressed wire bytes to old-ClassAd expression
// strings + MyType/TargetType, with no AST (see decodeAdRaw). It is the emit-side
// worker shared by decodeAdRaw and the QueryRaw/ScanRaw scan path.
func (c *Collection) formatWireAd(wireBytes []byte) (exprs []string, myType, targetType string, ok bool) {
	if c.inline {
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
