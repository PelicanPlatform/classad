package collections

import (
	"math"
	"sort"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// Short-circuit operand reordering. The ClassAd evaluator evaluates a && / || chain
// left to right and stops as soon as one operand decides the result (|| stops on the
// first TRUE, && on the first FALSE). So the ORDER of a chain's operands is a pure
// evaluation-cost lever: put the operand that most cheaply and most often
// short-circuits first, and the expensive operands (an opaque function, a split /
// regexp) are skipped on most candidates.
//
// This is the runtime complement to the index cost gate: the gate decides index vs.
// scan, and reordering makes the scan itself -- and the per-candidate re-verify of the
// indexed path -- cheaper. It changes only evaluation order, never which slots match:
// ClassAd && / || are associative and commutative in three-valued logic, so reordering
// PURE operands is result-preserving (an impure operand like random()/time() could be
// evaluated or skipped differently, so a chain with one keeps its top-level order).

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

// reorderJobRequirements returns job with its Requirements operands reordered for
// cheaper short-circuit evaluation. The input job is never modified: when reordering
// changes the expression a copy carrying the reordered Requirements is returned,
// otherwise job is returned unchanged. Reordering never changes which slots match.
func (c *Collection) reorderJobRequirements(job *classad.ClassAd) *classad.ClassAd {
	reqExpr := jobRequirementsExpr(job)
	if reqExpr == nil {
		return job
	}
	jobVals := jobValues(job)
	changed := false
	reordered := c.reorderShortCircuit(reqExpr, jobVals, &changed)
	if !changed || reordered == nil {
		return job
	}
	copyAd, err := classad.Parse(job.StringWithPrivate())
	if err != nil {
		return job // cannot round-trip: keep the original order
	}
	copyAd.Insert("Requirements", reordered)
	return copyAd
}

// reorderShortCircuit returns e with every associative && / || chain reordered so the
// evaluator short-circuits sooner. It sets *changed when it actually moves an operand,
// so the caller can skip copying the job when nothing reordered.
func (c *Collection) reorderShortCircuit(e ast.Expr, jobVals map[string]classad.Value, changed *bool) ast.Expr {
	switch n := e.(type) {
	case *ast.ParenExpr:
		return &ast.ParenExpr{Inner: c.reorderShortCircuit(n.Inner, jobVals, changed)}
	case *ast.BinaryOp:
		if n.Op == "||" || n.Op == "&&" {
			return c.reorderChain(n, jobVals, changed)
		}
		return &ast.BinaryOp{Op: n.Op,
			Left:  c.reorderShortCircuit(n.Left, jobVals, changed),
			Right: c.reorderShortCircuit(n.Right, jobVals, changed)}
	case *ast.UnaryOp:
		return &ast.UnaryOp{Op: n.Op, Expr: c.reorderShortCircuit(n.Expr, jobVals, changed)}
	case *ast.ConditionalExpr:
		return &ast.ConditionalExpr{
			Condition: c.reorderShortCircuit(n.Condition, jobVals, changed),
			TrueExpr:  c.reorderShortCircuit(n.TrueExpr, jobVals, changed),
			FalseExpr: c.reorderShortCircuit(n.FalseExpr, jobVals, changed)}
	case *ast.ElvisExpr:
		return &ast.ElvisExpr{
			Left:  c.reorderShortCircuit(n.Left, jobVals, changed),
			Right: c.reorderShortCircuit(n.Right, jobVals, changed)}
	default:
		return e
	}
}

// reorderChain flattens one associative && / || chain, recurses into its operands, and
// (when every operand is pure) sorts them by short-circuit rank. The rank favors cheap
// operands that most often decide the result: in an || chain, likely-true operands; in
// an && chain, likely-false ones.
func (c *Collection) reorderChain(root *ast.BinaryOp, jobVals map[string]classad.Value, changed *bool) ast.Expr {
	op := root.Op
	operands := flattenChain(root, op)
	for i := range operands {
		operands[i] = c.reorderShortCircuit(operands[i], jobVals, changed)
	}
	if len(operands) > 1 && allPure(operands) {
		type ranked struct {
			e    ast.Expr
			rank float64
			seq  int
		}
		rs := make([]ranked, len(operands))
		for i, o := range operands {
			favor := 0.5
			if p, ok := c.operandTrueProb(o, jobVals); ok {
				favor = p
			}
			if op == "&&" {
				favor = 1 - favor // && stops on FALSE: prefer likely-false
			}
			rs[i] = ranked{e: o, rank: exprEvalCost(o) / math.Max(favor, shortCircuitEps), seq: i}
		}
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].rank < rs[j].rank })
		for i := range rs {
			if rs[i].seq != i {
				*changed = true
			}
			operands[i] = rs[i].e
		}
	}
	return rebuildChain(op, operands)
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

// rebuildChain rebuilds a left-deep op chain from operands (re-association is sound for
// && / ||).
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

// allPure reports whether every operand is side-effect free, so the chain may be
// reordered without changing its three-valued result.
func allPure(operands []ast.Expr) bool {
	for _, o := range operands {
		if !isPureExpr(o) {
			return false
		}
	}
	return true
}
