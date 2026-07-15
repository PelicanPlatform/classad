package vm

import (
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// Probe is an index-satisfiable constraint extracted from a query: a self-scoped
// attribute Attr related by Op to one or more literal values. Op is one of
// "==","!=","<","<=",">=",">" (a single Val), "in" (a set of Vals, from an
// OR-of-equalities), "is"/"isnt" (=?=/=!= exact identity), or "present"/"absent"
// (is/isnt undefined). A store matches Probes against its configured indexes to
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

// ProbeGroup is a conjunction of index probes -- a candidate matches the group when
// it satisfies ALL of them. An empty Probes means the group is unconstrained (that
// disjunct can match anything), so a plan containing one cannot prune.
type ProbeGroup struct {
	Probes []Probe
}

// ProbePlan describes the query's index-satisfiable structure as a disjunction of
// conjunctive groups (DNF over the top-level `||` spine): a candidate satisfies the
// plan when it satisfies every probe of ANY group. A purely conjunctive query is a
// single group -- identical to Probes(). A disjunctive query like `(A && B) || C`
// yields one group per disjunct, which the planner executes as a union of
// intersections. Because each group is an over-approximation of its disjunct and the
// store re-verifies, the union is a sound candidate superset; a group with no probes
// makes the plan un-prunable (the caller then full-scans).
func (q *Query) ProbePlan() []ProbeGroup {
	if q == nil || q.prog == nil || q.prog.expr == nil {
		return nil
	}
	exprGroups := distributeDNF(classad.FoldConstants(q.prog.expr))
	groups := make([]ProbeGroup, 0, len(exprGroups))
	for _, g := range exprGroups {
		var probes []Probe
		for _, c := range g {
			if p, ok := probeFrom(c); ok {
				probes = append(probes, p)
			}
		}
		groups = append(groups, ProbeGroup{Probes: probes})
	}
	return groups
}

// maxDNFGroups bounds disjunctive-normal-form expansion so a predicate with many
// nested ORs (whose full DNF is a cross-product) cannot explode. Above it, distributeDNF
// falls back to top-level-only splitting (nested ORs then stay unprobed -- sound, just
// less selective).
const maxDNFGroups = 16

// distributeDNF turns a boolean predicate into disjunctive normal form -- a slice of
// conjunctive groups (each an AND-list of leaf expressions) whose union is the
// predicate -- distributing nested ORs (`A && (B || C)` -> `(A && B) || (A && C)`) up
// to maxDNFGroups. Beyond the bound it falls back to splitting only the top-level OR
// spine, leaving nested ORs intact.
func distributeDNF(e ast.Expr) [][]ast.Expr {
	if groups, ok := tryDNF(e); ok {
		return groups
	}
	var out [][]ast.Expr
	for _, d := range flattenOr(e, nil) {
		out = append(out, flattenAnd(d, nil))
	}
	return out
}

// tryDNF recursively builds the bounded DNF: OR concatenates disjunct groups, AND takes
// the cross-product of its operands' groups. ok is false if the group count would
// exceed maxDNFGroups.
func tryDNF(e ast.Expr) ([][]ast.Expr, bool) {
	e = unparen(e)
	if b, ok := e.(*ast.BinaryOp); ok {
		switch b.Op {
		case "||":
			// An OR-of-equalities on one attribute is a single `in` probe -- keep it as
			// a leaf rather than splitting it into a group per value.
			if _, ok := orEqProbe(b); ok {
				return [][]ast.Expr{{e}}, true
			}
			l, ok1 := tryDNF(b.Left)
			r, ok2 := tryDNF(b.Right)
			if !ok1 || !ok2 || len(l)+len(r) > maxDNFGroups {
				return nil, false
			}
			return append(l, r...), true
		case "&&":
			l, ok1 := tryDNF(b.Left)
			r, ok2 := tryDNF(b.Right)
			if !ok1 || !ok2 || len(l)*len(r) > maxDNFGroups {
				return nil, false
			}
			out := make([][]ast.Expr, 0, len(l)*len(r))
			for _, ga := range l {
				for _, gb := range r {
					g := make([]ast.Expr, 0, len(ga)+len(gb))
					g = append(g, ga...)
					g = append(g, gb...)
					out = append(out, g)
				}
			}
			return out, true
		}
	}
	return [][]ast.Expr{{e}}, true
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
	"is": "is", "isnt": "isnt", // =?= / =!= are symmetric
}

// ProbeOf returns the index probe a single (already slot-rewritten) expression yields,
// or ok=false if it is not a recognizable Attr-OP-literal / presence / OR-of-equalities.
// Callers use it to tell an already-probeable leaf from an opaque one (e.g. before
// finite-domain materialization).
func ProbeOf(e ast.Expr) (Probe, bool) { return probeFrom(e) }

// probeFrom classifies a single conjunct into a Probe.
func probeFrom(c ast.Expr) (Probe, bool) {
	c = unparen(c)
	// !X -> handle the negatable forms: a flipped comparison, a negated presence
	// test (!(attr is undefined) is presence), or !isUndefined(attr) (presence).
	if u, ok := c.(*ast.UnaryOp); ok && u.Op == "!" {
		inner := unparen(u.Expr)
		if fc, ok := inner.(*ast.FunctionCall); ok {
			return undefinedFuncProbe(fc, true)
		}
		if b, ok := inner.(*ast.BinaryOp); ok {
			if fl, ok := negFlip[b.Op]; ok {
				return cmpProbe(fl, b.Left, b.Right)
			}
			if b.Op == "is" || b.Op == "isnt" {
				if name, ok := refVsUndefined(b.Left, b.Right); ok {
					if b.Op == "is" { // !(attr is undefined) -> present
						return Probe{Attr: name, Op: "present"}, true
					}
					return Probe{Attr: name, Op: "absent"}, true // !(attr isnt undefined) -> absent
				}
			}
		}
		return Probe{}, false
	}
	// isUndefined(attr) is a presence probe (matches when attr is absent/undefined).
	if fc, ok := c.(*ast.FunctionCall); ok {
		return undefinedFuncProbe(fc, false)
	}
	b, ok := c.(*ast.BinaryOp)
	if !ok {
		return Probe{}, false
	}
	switch b.Op {
	case "==", "!=", "<", "<=", ">", ">=":
		return cmpProbe(b.Op, b.Left, b.Right)
	case "is":
		// `attr =?= undefined` is a presence probe (matches when attr is absent /
		// evaluates undefined); the index answers it from its posted set.
		if name, ok := refVsUndefined(b.Left, b.Right); ok {
			return Probe{Attr: name, Op: "absent"}, true
		}
		// `attr =?= literal` is exact (case-sensitive) identity: the "is" op reads the
		// index's exact-case postings (categorical) or the value posting (numeric, a
		// superset the store re-verifies).
		return cmpProbe("is", b.Left, b.Right)
	case "isnt":
		// `attr =!= undefined` is a presence probe (matches when attr is defined).
		if name, ok := refVsUndefined(b.Left, b.Right); ok {
			return Probe{Attr: name, Op: "present"}, true
		}
		// `attr =!= literal` = everything but the exact-case matches. Indexable for
		// categoricals via the exact-case postings (all-but-exact); for values it is
		// dropped in valUsable (int/real type-strictness makes the folded != path drop
		// records =!= should keep), falling back to a scan.
		return cmpProbe("isnt", b.Left, b.Right)
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

// refVsUndefined recognizes `attr is/isnt undefined` in either operand order,
// returning the self-scoped attribute name.
func refVsUndefined(left, right ast.Expr) (string, bool) {
	if isUndefinedLit(right) {
		return indexableRef(left)
	}
	if isUndefinedLit(left) {
		return indexableRef(right)
	}
	return "", false
}

func isUndefinedLit(e ast.Expr) bool {
	_, ok := unparen(e).(*ast.UndefinedLiteral)
	return ok
}

// undefinedFuncProbe classifies isUndefined(attr) as a presence probe: bare it is
// "absent" (true when attr is undefined); negated (!isUndefined(attr)) it is
// "present". Any other function is not an index probe.
func undefinedFuncProbe(fc *ast.FunctionCall, negated bool) (Probe, bool) {
	if !strings.EqualFold(fc.Name, "isUndefined") || len(fc.Args) != 1 {
		return Probe{}, false
	}
	name, ok := indexableRef(fc.Args[0])
	if !ok {
		return Probe{}, false
	}
	op := "absent"
	if negated {
		op = "present"
	}
	return Probe{Attr: name, Op: op}, true
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
