package collections

import (
	"iter"
	"strconv"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// RawAd is a query result rendered as old-ClassAd wire parts -- the "Name = Value"
// expression byte slices plus MyType/TargetType -- decoded straight from the stored
// form with no ast/classad ClassAd. A collector can hand Exprs to
// message.PutClassAdRawBytes to stream a result set without ever building an AST.
//
// Exprs (and their backing bytes) alias a buffer reused across the iteration, so a
// consumer must finish with one RawAd -- e.g. write it to the wire -- before
// advancing the iterator to the next.
type RawAd struct {
	Exprs      [][]byte
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
		q := queryPlan{}
		emit := c.yieldRaw(yield)
		for _, sh := range c.shards {
			if !c.scanShard(sh, q, emit) {
				return
			}
		}
	}
}

// yieldRaw is the scan emit callback for the raw path: render the decompressed
// wire bytes straight to old-ClassAd text and yield them. The buf/offs/exprs
// scratch is reused across the whole scan (single-threaded), so no per-ad
// expression bytes are allocated.
func (c *Collection) yieldRaw(yield func(RawAd) bool) func(w []byte) bool {
	var buf []byte
	var offs []int
	var exprs [][]byte
	return func(w []byte) bool {
		var mt, tt string
		var ok bool
		buf, offs, mt, tt, ok = c.appendWireAd(w, buf, offs)
		if !ok {
			return true // undecodable record: skip, keep scanning
		}
		exprs = exprs[:0]
		for i := 0; i+1 < len(offs); i++ {
			exprs = append(exprs, buf[offs[i]:offs[i+1]])
		}
		return yield(RawAd{Exprs: exprs, MyType: mt, TargetType: tt})
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
	buf, offs, mt, tt, ok := c.appendWireAd(wireBytes, nil, nil)
	if !ok {
		return nil, "", "", false
	}
	exprs = make([]string, 0, len(offs)-1)
	for i := 0; i+1 < len(offs); i++ {
		exprs = append(exprs, string(buf[offs[i]:offs[i+1]]))
	}
	return exprs, mt, tt, true
}

// appendWireAd renders already-decompressed wire bytes to old-ClassAd expression
// text with no AST: each "Name = Value" is appended into buf (reset to [:0] and
// grown as needed), and offs records the boundaries so expression i is
// buf[offs[i]:offs[i+1]]. MyType/TargetType are returned separately (they are sent
// as trailing fields, not numbered expressions). Passing the previous ad's buf/offs
// reuses their backing, so a streaming scan allocates no per-ad expression strings.
// Returns ok=false for inline-name ads (no intern table).
func (c *Collection) appendWireAd(wireBytes []byte, buf []byte, offs []int) (outBuf []byte, outOffs []int, myType, targetType string, ok bool) {
	buf = buf[:0]
	offs = append(offs[:0], 0)
	// ForEachNamed handles both encodings: an inline (persistent) ad yields its
	// stored names and the intern table is ignored; an interned ad resolves ids
	// through it. Values render with the matching decoder (inline nodes carry
	// inline names too).
	good := true
	wire.Ad(wireBytes).ForEachNamed(c.intern, func(name string, node []byte) bool {
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
		buf = append(buf, name...)
		buf = append(buf, ' ', '=', ' ')
		var aerr error
		if c.inline {
			buf, aerr = wire.AppendNodeTextInline(buf, node)
		} else {
			buf, aerr = appendWireValue(buf, node, c.intern)
		}
		if aerr != nil {
			good = false
			return false
		}
		offs = append(offs, len(buf))
		return true
	})
	if !good {
		return buf, offs, "", "", false
	}
	return buf, offs, myType, targetType, true
}

// appendWireValue appends a wire node's canonical ClassAd value text to dst,
// matching ast.Expr.String(). Literals are appended directly (no intermediate
// string allocation); computed expressions decode to an ast.Expr and render.
func appendWireValue(dst, node []byte, table *wire.InternTable) ([]byte, error) {
	// String literals are the most common non-numeric attribute value; quote their
	// bytes straight from the wire (no string copy) before the general literal path.
	if s, ok := wire.StringLiteralValue(node); ok {
		return ast.AppendQuoteStringBytes(dst, s), nil
	}
	if lit, ok := wire.LiteralValue(node); ok {
		switch lit.Kind {
		case wire.LitInt:
			return strconv.AppendInt(dst, lit.Int, 10), nil
		case wire.LitReal:
			return strconv.AppendFloat(dst, lit.Real, 'g', -1, 64), nil
		case wire.LitString:
			return ast.AppendQuoteString(dst, lit.Str), nil
		case wire.LitBool:
			if lit.Bool {
				return append(dst, "true"...), nil
			}
			return append(dst, "false"...), nil
		case wire.LitUndef:
			return append(dst, "undefined"...), nil
		case wire.LitError:
			return append(dst, "error"...), nil
		}
	}
	// Non-scalar: a list, record, or computed expression. Render it straight from
	// the wire (AST-free) instead of decoding an ast.Expr and calling String().
	return wire.AppendNodeText(dst, node, table)
}
