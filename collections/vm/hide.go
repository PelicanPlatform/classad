package vm

import (
	"strings"

	"github.com/PelicanPlatform/classad/ast"
)

// Hiding returns q with every reference to an attribute hide reports replaced by the literal
// undefined, compiled with q's pinned time, or q itself when it references none.
//
// It is how a store serves a reader that may not see some attributes WHATEVER form their values are
// stored in. Such a reader evaluates a sealed value as undefined, because it holds no key; a value of
// the same attribute written in the clear, before the attribute was sealed, has to read the same way,
// and the planner must not be able to tell the two apart either. Replacing the reference before
// planning does both at once: zone maps, indexes and column tiers never see the attribute, so none of
// them can prune, match or count on a value the reader is not entitled to, and the per-record
// evaluation reads undefined exactly where a sealed value would have.
//
// A reference and the literal agree in every context: an attribute whose value is undefined and an
// absent one both evaluate to undefined, under every operator and function, and `is`/`isnt` compare
// that value. Every scope is replaced, TARGET included -- a collection ad has no target, so a TARGET
// reference is undefined already. A reference inside a record literal that defines the same name
// refers to the record's own attribute and is left alone. A name computed at run time (eval of a
// non-constant) is not visible here; the store resolves those through the same rule at evaluation.
func (q *Query) Hiding(hide func(name string) bool) *Query {
	if q == nil || q.prog == nil || q.prog.expr == nil || hide == nil {
		return q
	}
	h := hider{hide: hide}
	e := h.expr(q.prog.expr)
	if !h.changed {
		return q
	}
	return compileAt(e, q.prog.now)
}

type hider struct {
	hide    func(string) bool
	shadow  []map[string]struct{} // names defined by the enclosing record literals, innermost last
	changed bool
}

func (h *hider) shadowed(name string) bool {
	if len(h.shadow) == 0 {
		return false
	}
	folded := strings.ToLower(name)
	for _, s := range h.shadow {
		if _, ok := s[folded]; ok {
			return true
		}
	}
	return false
}

func (h *hider) expr(e ast.Expr) ast.Expr {
	switch v := e.(type) {
	case nil:
		return nil
	case *ast.AttributeReference:
		if h.hide(v.Name) && !h.shadowed(v.Name) {
			h.changed = true
			return &ast.UndefinedLiteral{}
		}
		return v
	case *ast.ParenExpr:
		return &ast.ParenExpr{Inner: h.expr(v.Inner)}
	case *ast.UnaryOp:
		return &ast.UnaryOp{Op: v.Op, Expr: h.expr(v.Expr)}
	case *ast.BinaryOp:
		return &ast.BinaryOp{Op: v.Op, Left: h.expr(v.Left), Right: h.expr(v.Right)}
	case *ast.ElvisExpr:
		return &ast.ElvisExpr{Left: h.expr(v.Left), Right: h.expr(v.Right)}
	case *ast.ConditionalExpr:
		return &ast.ConditionalExpr{Condition: h.expr(v.Condition), TrueExpr: h.expr(v.TrueExpr), FalseExpr: h.expr(v.FalseExpr)}
	case *ast.ListLiteral:
		els := make([]ast.Expr, len(v.Elements))
		for i, el := range v.Elements {
			els[i] = h.expr(el)
		}
		return &ast.ListLiteral{Elements: els}
	case *ast.FunctionCall:
		args := make([]ast.Expr, len(v.Args))
		for i, a := range v.Args {
			args[i] = h.expr(a)
		}
		return &ast.FunctionCall{Name: v.Name, Args: args}
	case *ast.SelectExpr:
		// The selected name belongs to whatever the record evaluates to, not to the ad; only the
		// record expression can reach one of the ad's attributes.
		return &ast.SelectExpr{Record: h.expr(v.Record), Attr: v.Attr}
	case *ast.SubscriptExpr:
		return &ast.SubscriptExpr{Container: h.expr(v.Container), Index: h.expr(v.Index)}
	case *ast.RecordLiteral:
		if v.ClassAd == nil {
			return v
		}
		return &ast.RecordLiteral{ClassAd: h.record(v.ClassAd)}
	case *ast.ClassAd:
		return h.record(v)
	default:
		// Literals and anything without sub-expressions.
		return e
	}
}

// record rewrites a nested ad's values, with the names it defines shadowing the outer ad's.
func (h *hider) record(ad *ast.ClassAd) *ast.ClassAd {
	defined := make(map[string]struct{}, len(ad.Attributes))
	for _, a := range ad.Attributes {
		defined[strings.ToLower(a.Name)] = struct{}{}
	}
	h.shadow = append(h.shadow, defined)
	attrs := make([]*ast.AttributeAssignment, len(ad.Attributes))
	for i, a := range ad.Attributes {
		attrs[i] = &ast.AttributeAssignment{Name: a.Name, Value: h.expr(a.Value)}
	}
	h.shadow = h.shadow[:len(h.shadow)-1]
	return &ast.ClassAd{Attributes: attrs}
}
