package collections

import (
	"math"
	"sort"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// Short-circuit conjunct ordering. The ClassAd evaluator evaluates a && chain left to
// right and stops at the first operand that is not TRUE, so the order of the job
// Requirements' top-level conjuncts is an evaluation-cost lever: put the cheap conjunct
// that most often rejects first, and an expensive one (an opaque function, a split /
// regexp) is skipped on most candidates.
//
// ClassAd && and || are NOT commutative. They are left-biased with respect to ERROR and
// non-boolean operands: ERROR || TRUE is ERROR but TRUE || ERROR is TRUE, and
// FALSE && ERROR is FALSE but ERROR && FALSE is ERROR. Reordering an operand chain can
// therefore change its value. What it cannot change is whether a && chain of pure
// operands is TRUE: that holds exactly when every operand is TRUE, in any order. The
// match verdict on the job side is only that question (is Requirements TRUE?), so the
// reordered conjunction is used solely to decide it -- by the wire-native pre-filter and
// in place of evaluating the job's Requirements on a decoded candidate. The job ad
// itself is never rewritten: anything else that reads its Requirements (a slot's
// TARGET.Requirements, the job's own attributes) sees the original expression. Nested
// chains, || chains, and chains with an impure operand keep their written order.

// shortCircuitEps floors an operand's short-circuit probability so a probe estimated to
// never fire (fraction ~0) does not blow the rank up to infinity and pin an operand.
const shortCircuitEps = 0.02

// heavyFuncs are functions materially more expensive than a comparison -- they allocate
// and scan (lists, strings, regexps) -- so an operand calling one should sort late in a
// chain unless it short-circuits very often.
var heavyFuncs = map[string]bool{
	"split": true, "splitusername": true, "splitslotname": true,
	"stringlistmember": true, "stringlistimember": true, "stringlistsize": true,
	"stringlistsum": true, "stringlistavg": true, "stringlistmin": true, "stringlistmax": true,
	"regexp": true, "regexps": true, "replace": true, "substr": true,
	"strcat": true, "member": true, "anycompare": true, "allcompare": true,
}

// exprEvalCost is a static estimate of the work to evaluate e once. The scale is
// arbitrary (a bare reference or literal is 1); only ratios between operands matter, so
// the ordering is robust to the exact weights.
func exprEvalCost(e ast.Expr) float64 {
	switch n := e.(type) {
	case nil:
		return 0
	case *ast.AttributeReference, *ast.IntegerLiteral, *ast.RealLiteral,
		*ast.StringLiteral, *ast.BooleanLiteral, *ast.UndefinedLiteral, *ast.ErrorLiteral:
		return 1
	case *ast.ParenExpr:
		return exprEvalCost(n.Inner)
	case *ast.UnaryOp:
		return 1 + exprEvalCost(n.Expr)
	case *ast.BinaryOp:
		return 1 + exprEvalCost(n.Left) + exprEvalCost(n.Right)
	case *ast.SubscriptExpr:
		return 4 + exprEvalCost(n.Container) + exprEvalCost(n.Index)
	case *ast.SelectExpr:
		return 2 + exprEvalCost(n.Record)
	case *ast.ConditionalExpr:
		return 1 + exprEvalCost(n.Condition) + exprEvalCost(n.TrueExpr) + exprEvalCost(n.FalseExpr)
	case *ast.ElvisExpr:
		return 1 + exprEvalCost(n.Left) + exprEvalCost(n.Right)
	case *ast.ListLiteral:
		cost := 1.0
		for _, el := range n.Elements {
			cost += exprEvalCost(el)
		}
		return cost
	case *ast.RecordLiteral:
		return 5
	case *ast.FunctionCall:
		base := 8.0
		if heavyFuncs[lowerASCII(n.Name)] {
			base = 20
		}
		for _, a := range n.Args {
			base += exprEvalCost(a)
		}
		return base
	default:
		return 3
	}
}

// lowerASCII lowercases an ASCII function name without allocating for the common
// already-lowercase case.
func lowerASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'A' && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// operandTrueProb estimates the probability that a job-side Requirements operand
// evaluates true over the slots, from the resource index selectivity of the operand's
// slot rewrite. ok is false when nothing indexable backs the operand (the caller then
// uses a neutral prior). Best-effort: a superset probe over-estimates the true fraction,
// which only softens the ordering and never changes the match result.
func (c *Collection) operandTrueProb(operand ast.Expr, jobVals map[string]classad.Value) (float64, bool) {
	total := float64(c.Len())
	if total <= 0 {
		return 0, false
	}
	// Same rewrite the probe planner uses: TARGET.attr -> slot self-ref, job refs baked,
	// opaque pure functions of a low-cardinality categorical folded to a membership probe.
	pred := c.materializeFinite(rewriteForSlot(operand, jobVals, nil, false))
	ups := c.planIndex(vm.Compile(pred).Probes())
	if len(ups) == 0 {
		// A bare boolean conjunct (`HasFileTransfer`, `!HasFileTransfer`) is a truthiness
		// test, not a comparison, so probeFrom yields nothing -- but the attribute may be
		// indexed. Synthesize `attr == true/false` to read its selectivity, so an indexed
		// but near-always-true capability flag is ranked by its real ~99% rather than the
		// neutral prior (which, being cheap, would otherwise float it to the chain front).
		if p, ok := synthBoolProbe(pred); ok {
			ups = c.planIndex([]vm.Probe{p})
		}
	}
	if len(ups) == 0 {
		return 0, false
	}
	// An operand that is itself a small conjunction yields several probes; treat them as
	// independent (product of fractions) -- a rough but monotone estimate.
	frac := 1.0
	any := false
	for _, up := range ups {
		cand, covered := c.estimateCandidates(up)
		if !covered {
			continue
		}
		any = true
		frac *= math.Min(1, cand/total)
	}
	if !any {
		return 0, false
	}
	return frac, true
}

// conjunctOrder returns the evaluation order for the top-level && conjuncts of reqExpr
// (as flattened by topConjuncts): a permutation of their indexes, cheapest and most
// often rejecting first. It returns nil when reqExpr is not a && chain, when any
// conjunct is impure, or when the order would not change. Only the TRUE-ness of the
// reordered conjunction matches that of reqExpr, so callers must use it only for the
// job-side verdict (see the comment at the top of this file).
func (c *Collection) conjunctOrder(reqExpr ast.Expr, jobVals map[string]classad.Value) []int {
	ops := topConjuncts(reqExpr)
	if len(ops) < 2 || !allPure(ops) {
		return nil
	}
	type ranked struct {
		rank float64
		seq  int
	}
	rs := make([]ranked, len(ops))
	for i, o := range ops {
		falseProb := 0.5
		if p, ok := c.operandTrueProb(o, jobVals); ok {
			falseProb = 1 - p // && stops on a non-TRUE operand: prefer likely-false
		}
		rs[i] = ranked{rank: exprEvalCost(o) / math.Max(falseProb, shortCircuitEps), seq: i}
	}
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].rank < rs[j].rank })
	perm := make([]int, len(rs))
	moved := false
	for i := range rs {
		perm[i] = rs[i].seq
		if rs[i].seq != i {
			moved = true
		}
	}
	if !moved {
		return nil
	}
	return perm
}

// topConjuncts returns the operands of reqExpr's top-level && chain, or nil when reqExpr
// is not a && chain.
func topConjuncts(reqExpr ast.Expr) []ast.Expr {
	if b, ok := unparenExpr(reqExpr).(*ast.BinaryOp); !ok || b.Op != "&&" {
		return nil
	}
	return flattenChain(reqExpr, "&&")
}

// orderedConjunction rebuilds reqExpr's top-level && chain with its conjuncts in perm
// order (from conjunctOrder). It returns nil when perm is nil or does not fit reqExpr.
func orderedConjunction(reqExpr ast.Expr, perm []int) ast.Expr {
	if perm == nil {
		return nil
	}
	ops := topConjuncts(reqExpr)
	if len(ops) != len(perm) {
		return nil
	}
	ordered := make([]ast.Expr, len(perm))
	for i, j := range perm {
		ordered[i] = ops[j]
	}
	return rebuildChain("&&", ordered)
}

// flattenChain collects the operands of an associative op chain, descending through
// parentheses only when they wrap the SAME operator (so `a && (b || c)` keeps `(b || c)`
// as a single && operand rather than splicing its disjuncts in).
func flattenChain(e ast.Expr, op string) []ast.Expr {
	var out []ast.Expr
	var walk func(ast.Expr)
	walk = func(x ast.Expr) {
		switch b := x.(type) {
		case *ast.ParenExpr:
			if inner, ok := b.Inner.(*ast.BinaryOp); ok && inner.Op == op {
				walk(inner)
				return
			}
		case *ast.BinaryOp:
			if b.Op == op {
				walk(b.Left)
				walk(b.Right)
				return
			}
		}
		out = append(out, x)
	}
	walk(e)
	return out
}

// rebuildChain rebuilds a left-deep op chain from operands.
func rebuildChain(op string, operands []ast.Expr) ast.Expr {
	if len(operands) == 0 {
		return nil
	}
	acc := operands[0]
	for _, o := range operands[1:] {
		acc = &ast.BinaryOp{Op: op, Left: acc, Right: o}
	}
	return acc
}

// synthBoolProbe turns a bare boolean truthiness conjunct into the equivalent equality
// probe: `attr` -> `attr == true`, `!attr` -> `attr == false`. It lets operandTrueProb
// read the selectivity of an indexed boolean flag that probeFrom (which only classifies
// comparisons) does not recognize. Estimation only -- never affects match correctness.
func synthBoolProbe(e ast.Expr) (vm.Probe, bool) {
	e = unparenExpr(e)
	neg := false
	if u, ok := e.(*ast.UnaryOp); ok && u.Op == "!" {
		neg = true
		e = unparenExpr(u.Expr)
	}
	if r, ok := e.(*ast.AttributeReference); ok && (r.Scope == ast.NoScope || r.Scope == ast.MyScope) {
		return vm.Probe{Attr: r.Name, Op: "==", Vals: []classad.Value{classad.NewBoolValue(!neg)}}, true
	}
	return vm.Probe{}, false
}

func unparenExpr(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.Inner
	}
}

// allPure reports whether every operand is side-effect free, so evaluating the operands
// in another order (or skipping some) cannot change any operand's value.
func allPure(operands []ast.Expr) bool {
	for _, o := range operands {
		if !isPureExpr(o) {
			return false
		}
	}
	return true
}
