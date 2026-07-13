package collections

import (
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
	reqExpr := jobRequirementsExpr(job)
	if reqExpr == nil {
		return nil
	}
	slotExpr := rewriteForSlot(reqExpr, jobVals)
	return c.planIndex(vm.Compile(slotExpr).Probes())
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
// Every other node (function call, list, record, ternary, elvis, select, subscript,
// or anything unrecognized) is replaced by the undefined literal: it cannot form an
// index probe, and collapsing it guarantees no job reference leaks through as a bare
// self-scoped reference (which the extractor would wrongly read as a slot attribute).
func rewriteForSlot(e ast.Expr, jobVals map[string]classad.Value) ast.Expr {
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
		return &ast.BinaryOp{Op: n.Op, Left: rewriteForSlot(n.Left, jobVals), Right: rewriteForSlot(n.Right, jobVals)}
	case *ast.UnaryOp:
		return &ast.UnaryOp{Op: n.Op, Expr: rewriteForSlot(n.Expr, jobVals)}
	case *ast.ParenExpr:
		return &ast.ParenExpr{Inner: rewriteForSlot(n.Inner, jobVals)}
	case *ast.IntegerLiteral, *ast.RealLiteral, *ast.StringLiteral, *ast.BooleanLiteral,
		*ast.UndefinedLiteral, *ast.ErrorLiteral:
		return e
	default:
		return &ast.UndefinedLiteral{}
	}
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
func (c *Collection) indexedMatches(job *classad.ClassAd, usable []usableProbe, jp *jobPlan, deferMat bool) []rankedMatch {
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
			c.scanShardCandidates(sh, usable, func(w []byte) bool {
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
				c.scanShardCandidates(shards[idx], usable, func(w []byte) bool {
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
