package vm

import (
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// Probe is an index-satisfiable constraint extracted from a query: a self-scoped
// attribute Attr related by Op to one or more literal values. Op is one of
// "==","!=","<","<=",">=",">" (a single Val), "!<","!<=","!>","!>=" (a NEGATED ordering
// comparison, which is not the flipped one -- see negOrdering), "in" (a set of Vals: the
// attribute is == to one of them, from an OR-of-equalities or a member() call over a
// constant list), "is"/"isnt" (=?=/=!= exact identity), or "present"/"absent"
// (is/isnt undefined). A store matches Probes against its configured indexes to
// build a candidate set; because the store still re-verifies the full query, any
// Probe the planner omits only costs selectivity, never correctness.
//
// A query that reads CurrentTime has its probes computed with CurrentTime folded to the
// query's pinned time (see Compile), so `CompletionDate > CurrentTime - 86400` yields a
// range probe. Those probes describe only the ads that do not define CurrentTime; HTCondor
// does not store it in job or history ads. ExactProbes reports such a query as inexact,
// since a count computed from the probes alone would be wrong for an ad that does.
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
// The result is memoized and SHARED: callers read it, none may modify it.
func (q *Query) Probes() []Probe {
	if q == nil || q.prog == nil || q.prog.expr == nil {
		return nil
	}
	q.probesOnce.Do(func() { q.probes = q.computeProbes() })
	return q.probes
}

func (q *Query) computeProbes() []Probe {
	conj := flattenAnd(q.foldedExpr(), nil)
	var out []Probe
	for _, c := range conj {
		if p, ok := probeFrom(c); ok {
			out = append(out, p)
		}
	}
	return out
}

// ExactProbes returns the query's probes together with whether they are EXACTLY equivalent to the
// query: the top-level expression is a conjunction and EVERY conjunct was recognized as a probe.
//
// Probes and ProbePlan are deliberate OVER-APPROXIMATIONS, built for index pruning where the store
// re-verifies every candidate, so an omitted conjunct only costs selectivity. A consumer that answers
// from the probes ALONE -- a columnar COUNT/MIN/MAX that never re-verifies -- needs the opposite
// guarantee, and using Probes for that silently over-counts: `ProcId >= 5 && ClusterId != ProcId`
// compiles natively and yields ONE probe, because an attribute-to-attribute comparison is not
// `Attr OP literal`, so counting from that probe alone answers `ProcId >= 5` instead of the query.
//
// exact=false means "do not answer from these probes"; they remain a sound candidate filter.
func (q *Query) ExactProbes() (probes []Probe, exact bool) {
	if q == nil || q.prog == nil || q.prog.expr == nil {
		return nil, false
	}
	conj := flattenAnd(q.foldedExpr(), nil)
	out := make([]Probe, 0, len(conj))
	allExact := true
	for _, c := range conj {
		p, ok := probeFrom(c)
		if !ok {
			return out, false // an unrecognized conjunct: the probe set does not cover the query
		}
		out = append(out, p)
		allExact = allExact && probeIsExact(c, p)
	}
	if !allExact {
		return out, false // every conjunct is probed, but some probe is only a superset of it
	}
	// A pinned CurrentTime was folded into the probes, which is exact only for an ad that
	// does not define CurrentTime.
	return out, len(out) > 0 && !q.prog.foldNow
}

// probeIsExact reports whether probe p, which probeFrom built from conjunct c, holds for exactly
// the records c is TRUE for -- rather than for a superset of them, which is all a candidate filter
// needs. Two recognized shapes are only supersets:
//
//   - identicalMember(attr, {...}) plans as "in", the attribute == one of the values. =?= implies ==,
//     but not conversely: 1.0 == 1 while 1.0 =?= 1 is FALSE, and "A" == "a" while "A" =?= "a" is FALSE.
//   - an OR-of-equalities whose literals mix strings with numbers. `x == 1 || x == "a"` is ERROR, not
//     TRUE, for x = "a", because the first disjunct is ERROR (a string compared with a number) and
//     ERROR || TRUE is ERROR. "in" reads as the union of the equalities, which counts x = "a". With
//     one kind of literal no disjunct can be ERROR where another is TRUE, so the union is exact.
//
// member(attr, {...}) itself is exact: it is TRUE precisely when some element compares == TRUE,
// because the reference skips an element whose comparison is ERROR or UNDEFINED rather than
// propagating it.
func probeIsExact(c ast.Expr, p Probe) bool {
	switch v := unparen(c).(type) {
	case *ast.FunctionCall:
		identical, isMember := isMemberFunc(v.Name)
		return !isMember || !identical
	case *ast.BinaryOp:
		if v.Op != "||" {
			return true
		}
		var str, num bool
		for _, val := range p.Vals {
			if val.IsString() {
				str = true
			} else {
				num = true
			}
		}
		return !str || !num
	}
	return true
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
	exprGroups := distributeDNF(q.foldedExpr())
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

// negFlip maps a comparison operator to the operator its NEGATION is equivalent to -- for the
// equality pair only. `!(x == c)` is `x != c` and `!(x != c)` is `x == c` for every value of x:
// the two forms are UNDEFINED or ERROR together (an absent value, a string against a number), and
// wherever both are defined they are exact complements -- a NaN is != c and is not == c, so even
// there negation and flip agree. That last part is where the ordering operators part company.
//
// THE FOUR ORDERING OPERATORS ARE DELIBERATELY ABSENT. `!(x <= c)` is NOT `x > c`. Every
// comparison with a NaN is false, so a stored NaN makes `x <= c` FALSE and `!(x <= c)` TRUE while
// `x > c` is FALSE. Flipping the operator loses exactly the NaN records: an undercount wherever a
// consumer answers from the probe, and -- because zone pruning gates the row scan -- a whole
// segment silently dropped from the scan itself. They get their own operator spelling instead
// (negOrdering), which every consumer that cannot honour the difference refuses by default.
var negFlip = map[string]string{"==": "!=", "!=": "=="}

// negOrdering spells the negation of an ordering comparison as a probe operator of its own,
// rather than as the flipped comparison it is not (see negFlip).
//
// A consumer may act on one of these only if it either evaluates the comparison on the stored
// value and negates the RESULT -- which gets NaN right for free and costs nothing -- or has
// PROVEN that the values it reasons about hold no NaN. Everything else (every index posting
// lookup, every string-dictionary prune, every estimate) must decline it, which the op-string
// switches do by default. The negated form is still a necessary condition for its conjunct, so
// declining only costs selectivity.
var negOrdering = map[string]string{"<": "!<", "<=": "!<=", ">": "!>", ">=": "!>="}

// operandFlip maps a comparison to the operator that holds when its operands are
// swapped (e.g. `5 < Memory` is `Memory > 5`).
var operandFlip = map[string]string{
	"==": "==", "!=": "!=", "<": ">", "<=": ">=", ">": "<", ">=": "<=",
	"is": "is", "isnt": "isnt", // =?= / =!= are symmetric
	// The negated orderings swap the same way: `!(5 < Memory)` is `!(Memory > 5)`.
	"!<": "!>", "!<=": "!>=", "!>": "!<", "!>=": "!<=",
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
			// A negated ordering comparison keeps the negation in its operator rather than
			// flipping it away: the two are not the same predicate over a NaN (see negOrdering).
			if nf, ok := negOrdering[b.Op]; ok {
				return cmpProbe(nf, b.Left, b.Right)
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
		// !attr -> a falsy truthiness test on a bare attribute.
		if name, ok := indexableRef(inner); ok {
			return Probe{Attr: name, Op: "untruthy"}, true
		}
		return Probe{}, false
	}
	// isUndefined(attr) is a presence probe (matches when attr is absent/undefined);
	// member(attr, {constants}) is set membership.
	if fc, ok := c.(*ast.FunctionCall); ok {
		if p, ok := memberProbe(fc); ok {
			return p, true
		}
		return undefinedFuncProbe(fc, false)
	}
	// A bare attribute reference (`HAS_CVMFS`) is a truthiness test. Only a categorical
	// index can answer it soundly (its exception set catches non-boolean values, so the
	// candidate set stays a superset the store re-verifies); catUsable maps it to == true.
	if name, ok := indexableRef(c); ok {
		return Probe{Attr: name, Op: "truthy"}, true
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

// memberProbe builds a set-membership probe from member(attr, {c1, c2, ...}) or
// identicalMember(attr, {...}) over a self-scoped attribute and a list of constants.
//
// member is TRUE exactly when attr == some element is TRUE, which is what "in" means. An
// UNDEFINED or ERROR element is never == to anything, so it is dropped without changing the
// set. identicalMember is TRUE only where member is (=?= implies ==), so "in" is a sound
// superset for it -- except that =?= does find an UNDEFINED or ERROR element, which "in" cannot
// say (identicalMember(attr, {undefined}) holds where attr is absent), so such a list is not
// probed at all. ExactProbes knows the identicalMember probe is only a superset (probeIsExact).
//
// A list with no element left (`member(attr, {})`) can never be TRUE, but it is not probed:
// an empty "in" is not a shape the probe consumers are written for, and declining costs only
// selectivity.
func memberProbe(fc *ast.FunctionCall) (Probe, bool) {
	identical, ok := isMemberFunc(fc.Name)
	if !ok || len(fc.Args) != 2 {
		return Probe{}, false
	}
	name, ok := indexableRef(fc.Args[0])
	if !ok {
		return Probe{}, false
	}
	elems, ok := constListOf(fc.Args[1])
	if !ok {
		return Probe{}, false
	}
	vals := make([]classad.Value, 0, len(elems))
	for _, v := range elems {
		if v.IsUndefined() || v.IsError() {
			if identical {
				return Probe{}, false
			}
			continue
		}
		vals = append(vals, v)
	}
	if len(vals) == 0 {
		return Probe{}, false
	}
	return Probe{Attr: name, Op: "in", Vals: vals}, true
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
