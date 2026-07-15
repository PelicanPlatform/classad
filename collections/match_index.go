package collections

import (
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// matchIndexPlan derives an index candidate pre-filter (A2) from the job's
// Requirements: it rewrites Requirements into a predicate over the *slot* --
// TARGET.attr becomes the slot's own attribute, and every job reference is baked to
// its constant value -- then extracts that predicate's index-satisfiable conjuncts
// and matches them against the collection's configured indexes. An empty result
// means no usable constraint (Match then scans every slot).
//
// It is sound by construction: the rewrite can only ever yield probes on the slot's
// own attributes compared to job constants, and Match re-verifies every candidate
// with the full bilateral MatchClassAd, so a dropped or over-broad probe costs only
// selectivity, never correctness.
func (c *Collection) matchIndexPlan(job *classad.ClassAd, jobVals map[string]classad.Value) []usableProbe {
	if !c.spec.Load().any() {
		return nil // no indexes configured: nothing to plan against
	}
	return c.planIndex(c.slotProbes(job, jobVals))
}

// slotProbes rewrites the job's Requirements into a slot-side predicate and extracts
// its index-satisfiable probes: the resource attributes the match filters slots on.
// They drive the candidate pre-filter (matchIndexPlan) when covered by an index, and
// -- recorded as demand even when nothing is indexed -- let SuggestIndexes recommend
// indexing exactly those attributes to speed the match.
func (c *Collection) slotProbes(job *classad.ClassAd, jobVals map[string]classad.Value) []vm.Probe {
	reqExpr := jobRequirementsExpr(job)
	if reqExpr == nil {
		return nil
	}
	return vm.Compile(rewriteForSlot(reqExpr, jobVals, nil, false)).Probes()
}

// MatchExplain describes how matchmaking a specific job against this (resource)
// collection would execute: the job's Requirements rewritten over the slot (with the
// job's attributes baked to constants), the index-satisfiable probes that rewrite
// yields, and which of them prune candidates via a configured index.
type MatchExplain struct {
	// HasRequirements is false when the job has no Requirements (it then matches every
	// slot -- a full scan with no pruning).
	HasRequirements bool `json:"hasRequirements"`
	// SlotPredicate is the job's Requirements rewritten over the slot: TARGET.attr
	// becomes the slot's own attribute and every job reference is baked to its value,
	// e.g. `Memory >= 4096 && Arch == "X86_64"`. This is the predicate whose probes
	// drive candidate pruning (the bilateral match still re-verifies every candidate).
	SlotPredicate string `json:"slotPredicate"`
	// Probes are the rewritten predicate's index-satisfiable conjuncts and their index
	// status on the resource collection.
	Probes []ProbeExplain `json:"probes"`
	// IndexUsable is how many probes prune via an index.
	IndexUsable int `json:"indexUsable"`
	// Plan is the access path over the resource slots: "indexed" (visit only candidate
	// slots), "parallel-scan", or "serial-scan" (match every slot).
	Plan        string `json:"plan"`
	Parallelism int    `json:"parallelism"`
	Shards      int    `json:"shards"`
	// TotalResources is the resource (slot) count, the denominator for selectivity.
	TotalResources int `json:"totalResources"`
}

// ExplainMatch reports how matchmaking job against this collection would execute:
// it rewrites the job's Requirements over the slot (baking the job's attribute values
// to constants), extracts the index-satisfiable probes, and reports which are covered
// by a configured index and the resulting access path. No I/O beyond reading the spec.
func (c *Collection) ExplainMatch(job *classad.ClassAd) MatchExplain {
	total := c.Len()
	ex := MatchExplain{Parallelism: c.queryPar, Shards: len(c.shards), TotalResources: total}
	reqExpr := jobRequirementsExpr(job)
	if reqExpr == nil {
		ex.Plan = scanPlanName(c.queryPar) // no Requirements: every slot is a candidate
		return ex
	}
	ex.HasRequirements = true
	jobVals := jobValues(job)
	// Display: honest, baked, de-duplicated (functions kept, not shown as undefined).
	ex.SlotPredicate = slotDisplayExpr(reqExpr, jobVals)
	// Probes: from the probe rewrite (opaque functions -> undefined, so they drop).
	plan := vm.Compile(slotMatchExpr(reqExpr, jobVals)).ProbePlan()
	_, prunable := c.planIndexGroups(plan)
	for _, g := range plan {
		for _, p := range g.Probes {
			pe := ProbeExplain{Attr: p.Attr, Op: p.Op}
			var up usableProbe
			var isUsable bool
			pe.Indexed, pe.Kind, up, isUsable = c.probeIndexKind(p)
			if isUsable {
				ex.IndexUsable++
				if cand, covered := c.estimateCandidates(up); covered {
					pe.HasSelectivity = true
					pe.EstCandidates = int64(cand + 0.5)
					if total > 0 {
						pe.Selectivity = math.Min(1, cand/float64(total))
					}
				}
			}
			ex.Probes = append(ex.Probes, pe)
		}
	}
	if prunable {
		ex.Plan = "indexed"
	} else {
		ex.Plan = scanPlanName(c.queryPar)
	}
	return ex
}

func scanPlanName(queryPar int) string {
	if queryPar > 1 {
		return "parallel-scan"
	}
	return "serial-scan"
}

// rewriteForSlot turns the job's Requirements into an equivalent predicate over a
// slot, for index-probe extraction:
//
//   - TARGET.attr -> the slot's own (self-scoped) attribute, so a probe on it maps to
//     the slot index.
//   - MY.attr / unscoped / PARENT.attr (a job reference) -> the job's constant value,
//     or the undefined literal when it has no scalar value (TARGET-dependent, missing,
//     or non-scalar) -- which makes its conjunct non-probeable and thus dropped.
//   - literals pass through; arithmetic/negation over baked constants is left for the
//     probe extractor's constant folding.
//
// ifThenElse/ternary/elvis are recursed into (their arguments rewritten) so a guard
// that becomes constant after baking can collapse under constant folding -- e.g. a
// modern WithinResourceLimits' disk term guarded by `catalogs is undefined`. When such
// a guard's reference is an UNSCOPED attribute absent from the job, substituting
// undefined is an approximation (unscoped refs fall through to the slot at eval time);
// rewriteForSlot records it in `assumed` (only inside a control-flow construct, where
// it can actually enable a fold) so slotMatchPlan can add a sound `name isnt undefined`
// exception disjunct. Other function calls / lists / records remain undefined (opaque
// to the index).
func rewriteForSlot(e ast.Expr, jobVals map[string]classad.Value, assumed map[string]bool, inCtrl bool) ast.Expr {
	switch n := e.(type) {
	case *ast.AttributeReference:
		if n.Scope == ast.TargetScope {
			return &ast.AttributeReference{Name: n.Name, Scope: ast.NoScope}
		}
		if v, ok := jobVals[strings.ToLower(n.Name)]; ok {
			if lit := valueToLiteral(v); lit != nil {
				return lit
			}
		}
		if n.Scope == ast.NoScope && inCtrl && assumed != nil {
			assumed[n.Name] = true // unscoped + absent inside a guard: assumed undefined
		}
		return &ast.UndefinedLiteral{}
	case *ast.BinaryOp:
		return &ast.BinaryOp{Op: n.Op, Left: rewriteForSlot(n.Left, jobVals, assumed, inCtrl), Right: rewriteForSlot(n.Right, jobVals, assumed, inCtrl)}
	case *ast.UnaryOp:
		return &ast.UnaryOp{Op: n.Op, Expr: rewriteForSlot(n.Expr, jobVals, assumed, inCtrl)}
	case *ast.ParenExpr:
		return &ast.ParenExpr{Inner: rewriteForSlot(n.Inner, jobVals, assumed, inCtrl)}
	case *ast.ConditionalExpr:
		return &ast.ConditionalExpr{
			Condition: rewriteForSlot(n.Condition, jobVals, assumed, true),
			TrueExpr:  rewriteForSlot(n.TrueExpr, jobVals, assumed, true),
			FalseExpr: rewriteForSlot(n.FalseExpr, jobVals, assumed, true),
		}
	case *ast.ElvisExpr:
		return &ast.ElvisExpr{
			Left:  rewriteForSlot(n.Left, jobVals, assumed, true),
			Right: rewriteForSlot(n.Right, jobVals, assumed, true),
		}
	case *ast.FunctionCall:
		if strings.EqualFold(n.Name, "ifThenElse") && len(n.Args) == 3 {
			args := make([]ast.Expr, 3)
			for i := range n.Args {
				args[i] = rewriteForSlot(n.Args[i], jobVals, assumed, true)
			}
			return &ast.FunctionCall{Name: n.Name, Args: args}
		}
		return &ast.UndefinedLiteral{}
	case *ast.IntegerLiteral, *ast.RealLiteral, *ast.StringLiteral, *ast.BooleanLiteral,
		*ast.UndefinedLiteral, *ast.ErrorLiteral:
		return e
	default:
		return &ast.UndefinedLiteral{}
	}
}

// slotMatchExpr rewrites the job's Requirements over the slot and returns the sound
// pushdown predicate: the rewritten predicate OR one `name isnt undefined` disjunct per
// unscoped attribute the rewrite had to assume undefined inside a guard. Including a
// slot where an assumed attribute is actually present keeps the pushdown sound (there
// the approximation may not hold, so the candidate is visited and re-verified); a slot
// where it is absent is covered by the main predicate.
func slotMatchExpr(reqExpr ast.Expr, jobVals map[string]classad.Value) ast.Expr {
	assumed := map[string]bool{}
	pred := rewriteForSlot(reqExpr, jobVals, assumed, false)
	// Deterministic order for a stable plan/explain.
	names := make([]string, 0, len(assumed))
	for name := range assumed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		exc := &ast.BinaryOp{
			Op:    "isnt",
			Left:  &ast.AttributeReference{Name: name, Scope: ast.NoScope},
			Right: &ast.UndefinedLiteral{},
		}
		pred = &ast.BinaryOp{Op: "||", Left: pred, Right: exc}
	}
	return pred
}

// slotDisplayExpr rewrites the job's Requirements over the slot for a HUMAN-READABLE
// explanation: like rewriteForSlot it maps TARGET.attr to the slot's own attribute and
// bakes the job's constant values, but it PRESERVES functions and control-flow (their
// arguments baked) instead of collapsing them to undefined. So an opaque conjunct reads
// as `HasFileTransfer && stringListIMember("osdf", HasFileTransferPluginMethods)` -- what
// it really is -- rather than `HasFileTransfer && undefined`. It is display-only; probe
// extraction uses slotMatchExpr. Top-level conjuncts are de-duplicated (a Requirements
// that repeats `TARGET.HasSingularity` shows it once).
func slotDisplayExpr(reqExpr ast.Expr, jobVals map[string]classad.Value) string {
	folded := classad.FoldConstants(displayRewrite(reqExpr, jobVals))
	conj := dedupConjuncts(folded)
	parts := make([]string, len(conj))
	for i, c := range conj {
		parts[i] = c.String()
	}
	return strings.Join(parts, " && ")
}

func displayRewrite(e ast.Expr, jobVals map[string]classad.Value) ast.Expr {
	switch n := e.(type) {
	case *ast.AttributeReference:
		if n.Scope == ast.TargetScope {
			return &ast.AttributeReference{Name: n.Name, Scope: ast.NoScope}
		}
		if v, ok := jobVals[strings.ToLower(n.Name)]; ok {
			if lit := valueToLiteral(v); lit != nil {
				return lit
			}
		}
		return &ast.UndefinedLiteral{}
	case *ast.BinaryOp:
		return &ast.BinaryOp{Op: n.Op, Left: displayRewrite(n.Left, jobVals), Right: displayRewrite(n.Right, jobVals)}
	case *ast.UnaryOp:
		return &ast.UnaryOp{Op: n.Op, Expr: displayRewrite(n.Expr, jobVals)}
	case *ast.ParenExpr:
		return &ast.ParenExpr{Inner: displayRewrite(n.Inner, jobVals)}
	case *ast.ConditionalExpr:
		return &ast.ConditionalExpr{Condition: displayRewrite(n.Condition, jobVals), TrueExpr: displayRewrite(n.TrueExpr, jobVals), FalseExpr: displayRewrite(n.FalseExpr, jobVals)}
	case *ast.ElvisExpr:
		return &ast.ElvisExpr{Left: displayRewrite(n.Left, jobVals), Right: displayRewrite(n.Right, jobVals)}
	case *ast.FunctionCall:
		args := make([]ast.Expr, len(n.Args))
		for i := range n.Args {
			args[i] = displayRewrite(n.Args[i], jobVals)
		}
		return &ast.FunctionCall{Name: n.Name, Args: args}
	case *ast.SubscriptExpr:
		return &ast.SubscriptExpr{Container: displayRewrite(n.Container, jobVals), Index: displayRewrite(n.Index, jobVals)}
	case *ast.SelectExpr:
		return &ast.SelectExpr{Record: displayRewrite(n.Record, jobVals), Attr: n.Attr}
	default:
		return e
	}
}

// dedupConjuncts flattens the top-level && spine and drops later conjuncts equal (by
// unparsed text) to an earlier one, preserving first-seen order.
func dedupConjuncts(e ast.Expr) []ast.Expr {
	var out []ast.Expr
	seen := map[string]bool{}
	var walk func(ast.Expr)
	walk = func(x ast.Expr) {
		if p, ok := x.(*ast.ParenExpr); ok {
			walk(p.Inner)
			return
		}
		if b, ok := x.(*ast.BinaryOp); ok && b.Op == "&&" {
			walk(b.Left)
			walk(b.Right)
			return
		}
		if s := x.String(); !seen[s] {
			seen[s] = true
			out = append(out, x)
		}
	}
	walk(e)
	return out
}

// slotMatchPlan builds the DNF index plan for matching job against this collection: the
// slot predicate (with undefined-guard exceptions) compiled to a probe plan and matched
// to the configured indexes. prunable is false when some disjunct is unconstrained (the
// caller then full-scans / re-verifies every slot).
func (c *Collection) slotMatchPlan(job *classad.ClassAd, jobVals map[string]classad.Value) (groups [][]usableProbe, prunable bool) {
	reqExpr := jobRequirementsExpr(job)
	if reqExpr == nil {
		return nil, false
	}
	plan := vm.Compile(slotMatchExpr(reqExpr, jobVals)).ProbePlan()
	return c.planIndexGroups(plan)
}

// valueToLiteral converts a scalar classad.Value to its literal AST node, or nil for
// undefined/error/list/nested-ad values (which cannot serve as an index constant).
func valueToLiteral(v classad.Value) ast.Expr {
	switch {
	case v.IsBool():
		b, _ := v.BoolValue()
		return &ast.BooleanLiteral{Value: b}
	case v.IsInteger():
		i, _ := v.IntValue()
		return &ast.IntegerLiteral{Value: i}
	case v.IsReal():
		f, _ := v.RealValue()
		return &ast.RealLiteral{Value: f}
	case v.IsString():
		s, _ := v.StringValue()
		return &ast.StringLiteral{Value: s}
	}
	return nil
}

// indexedMatches matches only the candidate slots the pre-filter selected, fanning
// out across shards when the worker budget allows. Each candidate is fully matched
// with MatchClassAd (plus the wire-native reject), so the index only narrows which
// slots are visited -- correctness is unchanged from a full scan.
func (c *Collection) indexedMatches(job *classad.ClassAd, groups [][]usableProbe, jp *jobPlan, deferMat bool) []rankedMatch {
	shards := c.shards

	W := 0
	jobText := job.StringWithPrivate()
	if _, err := classad.Parse(jobText); err == nil && c.qsem != nil && len(shards) >= 2 {
		want := c.queryPar
		if want > len(shards) {
			want = len(shards)
		}
		W = tryAcquire(c.qsem, want)
	}
	if W < 2 {
		for i := 0; i < W; i++ {
			<-c.qsem
		}
		orig := job.GetTarget()
		defer job.SetTarget(orig)
		mw := newMatchWorker(job, c, jp, deferMat)
		var out []rankedMatch
		for _, sh := range shards {
			c.scanShardCandidatesGroups(sh, groups, func(w []byte) bool {
				c.matchCandidate(w, mw, &out)
				return true
			})
		}
		return out
	}
	defer func() {
		for i := 0; i < W; i++ {
			<-c.qsem
		}
	}()

	perWorker := make([][]rankedMatch, W)
	var next int64
	var wg sync.WaitGroup
	for i := 0; i < W; i++ {
		wg.Add(1)
		go func(wi int) {
			defer wg.Done()
			jobCopy, _ := classad.Parse(jobText) // validated above
			mw := newMatchWorker(jobCopy, c, jp, deferMat)
			var local []rankedMatch
			for {
				idx := int(atomic.AddInt64(&next, 1)) - 1
				if idx >= len(shards) {
					break
				}
				c.scanShardCandidatesGroups(shards[idx], groups, func(w []byte) bool {
					c.matchCandidate(w, mw, &local)
					return true
				})
			}
			perWorker[wi] = local
		}(i)
	}
	wg.Wait()
	var out []rankedMatch
	for _, lw := range perWorker {
		out = append(out, lw...)
	}
	return out
}
