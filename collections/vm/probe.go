package vm

import (
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// Probe is an index-satisfiable constraint extracted from a query: a self-scoped
// attribute Attr related by Op to one or more literal values. Op is one of
// "==","!=","<","<=",">=",">" (a single Val) or "in" (a set of Vals, from an
// OR-of-equalities); `=?=` (identity) is normalized to "==" since the index yields
// a superset that the store's re-verify narrows. A store matches Probes against its
// configured indexes to
// build a candidate set; because the store still re-verifies the full query, any
// Probe the planner omits only costs selectivity, never correctness.
type Probe struct {
	Attr string
	Op   string
	Vals []classad.Value
}

// Probes extracts the query's index-satisfiable conjuncts. It constant-folds the
// source expression (so `Memory > 2048*1024` normalizes), then flattens the
// top-level && spine and classifies each conjunct, pushing `!` into comparisons
// where that is identity under ClassAd three-valued logic. Conjuncts that are not
// a recognizable `Attr OP literal` (or OR-of-equalities on one attr) are omitted.
func (q *Query) Probes() []Probe {
	if q == nil || q.prog == nil || q.prog.expr == nil {
		return nil
	}
	conj := flattenAnd(classad.FoldConstants(q.prog.expr), nil)
	var out []Probe
	for _, c := range conj {
		if p, ok := probeFrom(c); ok {
			out = append(out, p)
		}
	}
	return out
}

// flattenAnd collects the top-level `&&` conjuncts of e (unwrapping parentheses).
func flattenAnd(e ast.Expr, acc []ast.Expr) []ast.Expr {
	e = unparen(e)
	if b, ok := e.(*ast.BinaryOp); ok && b.Op == "&&" {
		acc = flattenAnd(b.Left, acc)
		acc = flattenAnd(b.Right, acc)
		return acc
	}
	return append(acc, e)
}

// flattenOr collects the top-level `||` disjuncts of e (unwrapping parentheses).
func flattenOr(e ast.Expr, acc []ast.Expr) []ast.Expr {
	e = unparen(e)
	if b, ok := e.(*ast.BinaryOp); ok && b.Op == "||" {
		acc = flattenOr(b.Left, acc)
		acc = flattenOr(b.Right, acc)
		return acc
	}
	return append(acc, e)
}

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok || p.Inner == nil {
			return e
		}
		e = p.Inner
	}
}

// negFlip maps a comparison operator to its negation, valid under ClassAd
// three-valued logic (both forms yield undefined/error in the same cases).
var negFlip = map[string]string{
	"==": "!=", "!=": "==", "<": ">=", "<=": ">", ">": "<=", ">=": "<",
}

// operandFlip maps a comparison to the operator that holds when its operands are
// swapped (e.g. `5 < Memory` is `Memory > 5`).
var operandFlip = map[string]string{
	"==": "==", "!=": "!=", "<": ">", "<=": ">=", ">": "<", ">=": "<=",
}

// probeFrom classifies a single conjunct into a Probe.
func probeFrom(c ast.Expr) (Probe, bool) {
	c = unparen(c)
	// !(comparison) -> flipped comparison (only a single comparison; no De Morgan).
	if u, ok := c.(*ast.UnaryOp); ok && u.Op == "!" {
		if b, ok := unparen(u.Expr).(*ast.BinaryOp); ok {
			if fl, ok := negFlip[b.Op]; ok {
				return cmpProbe(fl, b.Left, b.Right)
			}
		}
		return Probe{}, false
	}
	b, ok := c.(*ast.BinaryOp)
	if !ok {
		return Probe{}, false
	}
	switch b.Op {
	case "==", "!=", "<", "<=", ">", ">=":
		return cmpProbe(b.Op, b.Left, b.Right)
	case "is":
		// =?= (identity) is index-satisfiable exactly like ==, so plan it as ==.
		// The index yields a superset of =?='s matches — categorical postings are
		// case-folded and value postings fold int/real, both admitting more than
		// =?='s strict, case-sensitive, same-type identity — and the store
		// re-verifies every candidate against the real expression, narrowing back to
		// =?=. Absent attributes are never posted and `attr =?= literal` is false for
		// them, so they are correctly excluded. (=!=/"isnt" is deliberately NOT added:
		// it must match absent and case/type-differing records, which the case-folded
		// != posting path would wrongly drop.)
		return cmpProbe("==", b.Left, b.Right)
	case "||":
		return orEqProbe(b)
	}
	return Probe{}, false
}

// cmpProbe builds a probe from `AttrRef OP literal` or `literal OP AttrRef`.
func cmpProbe(op string, left, right ast.Expr) (Probe, bool) {
	if name, ok := indexableRef(left); ok {
		if v, ok := literalVal(right); ok {
			return Probe{Attr: name, Op: op, Vals: []classad.Value{v}}, true
		}
	}
	if name, ok := indexableRef(right); ok {
		if v, ok := literalVal(left); ok {
			return Probe{Attr: name, Op: operandFlip[op], Vals: []classad.Value{v}}, true
		}
	}
	return Probe{}, false
}

// orEqProbe builds a set-membership probe from a `||`-chain in which every
// disjunct is `SameAttr == literal`.
func orEqProbe(b *ast.BinaryOp) (Probe, bool) {
	disj := flattenOr(b, nil)
	attr := ""
	vals := make([]classad.Value, 0, len(disj))
	for _, d := range disj {
		p, ok := probeFrom(d)
		if !ok || p.Op != "==" || len(p.Vals) != 1 {
			return Probe{}, false
		}
		if attr == "" {
			attr = p.Attr
		} else if !strings.EqualFold(attr, p.Attr) {
			return Probe{}, false
		}
		vals = append(vals, p.Vals[0])
	}
	if attr == "" {
		return Probe{}, false
	}
	return Probe{Attr: attr, Op: "in", Vals: vals}, true
}

// indexableRef returns the name of a self-scoped (unscoped or MY) attribute
// reference — the only references an index over the current ad can satisfy.
func indexableRef(e ast.Expr) (string, bool) {
	if r, ok := unparen(e).(*ast.AttributeReference); ok {
		if r.Scope == ast.NoScope || r.Scope == ast.MyScope {
			return r.Name, true
		}
	}
	return "", false
}

// literalVal converts a literal AST node to a classad.Value. Undefined/error
// literals are intentionally not index values.
func literalVal(e ast.Expr) (classad.Value, bool) {
	switch v := unparen(e).(type) {
	case *ast.IntegerLiteral:
		return classad.NewIntValue(v.Value), true
	case *ast.RealLiteral:
		return classad.NewRealValue(v.Value), true
	case *ast.StringLiteral:
		return classad.NewStringValue(v.Value), true
	case *ast.BooleanLiteral:
		return classad.NewBoolValue(v.Value), true
	}
	return classad.Value{}, false
}
