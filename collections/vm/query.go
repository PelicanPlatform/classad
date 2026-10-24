package vm

import (
	"sync"
	"time"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// Query is a compiled boolean constraint over ads (a compiled Program plus the
// convenience of a match predicate). The store uses ReadAttrs for planning.
type Query struct {
	prog *Program
	// probesOnce/probes memoize Probes. A query's folded expression is fixed at
	// Compile (CurrentTime is pinned there), so its probes are too, and the scan
	// paths ask for them once per scan -- a paged read, once per page. The slice is
	// shared by every caller and must not be modified.
	probesOnce sync.Once
	probes     []Probe
	// fold is set when the query reads an unscoped or MY CurrentTime; it holds the folded
	// form (see Folded), compiled on first use.
	fold *foldedQuery
}

// foldedQuery is a Query's CurrentTime-folded form, compiled once. q stays nil when the
// fold leaves a CurrentTime reference behind (one inside a list or record literal).
type foldedQuery struct {
	once sync.Once
	q    *Query
}

// Compile compiles an already-parsed expression into a Query.
//
// When the expression references CurrentTime, Compile reads the clock once and pins it: a
// CurrentTime the ad does not define evaluates to that time on every evaluation, so the
// program and its index probes share a single "now" instead of the program re-reading the
// clock per ad. An ad that defines CurrentTime supplies its own value, as it does to the
// evaluator; the expression itself is compiled unchanged.
func Compile(expr ast.Expr) *Query {
	return compileAt(expr, 0)
}

// compileAt is Compile with the pinned time given (0 reads the clock).
func compileAt(expr ast.Expr, now int64) *Query {
	p := CompileProgram(expr)
	q := &Query{prog: p}
	if magic, named := currentTimeRefs(expr); named {
		if now == 0 {
			now = time.Now().Unix()
		}
		p.now = now
		p.foldNow = magic
		if magic {
			q.fold = &foldedQuery{}
		}
	}
	return q
}

// And returns the query `(q) && (e)`, compiled with q's pinned time, so a CurrentTime both
// read means the same instant. A caller that refines a query it has already run -- a top-K
// pass that adds the cutoff it computed -- needs that: recompiling from text would pin a
// later time and could select a different set.
func (q *Query) And(e ast.Expr) *Query {
	return compileAt(&ast.BinaryOp{Op: "&&", Left: &ast.ParenExpr{Inner: q.prog.expr}, Right: &ast.ParenExpr{Inner: e}}, q.prog.now)
}

// Folded returns the query with its unscoped and MY CurrentTime references folded to the
// pinned time and its constants folded (classad.FoldConstantsAt), compiled as a query of
// its own. It is nil when the query does not read CurrentTime that way, or when the fold
// leaves such a reference behind.
//
// The folded query evaluates exactly as q does on an ad that does not define CurrentTime
// (neither it nor a chained parent), at the same pinned time: that is FoldConstantsAt's
// contract. On an ad that defines CurrentTime it may not. So a store may evaluate it, and
// answer from its probes (ExactProbes), only over records it has proven lack the attribute;
// everywhere else it must evaluate q. Any CurrentTime reference the fold keeps (TARGET,
// PARENT, a select) is pinned to the same time as in q.
func (q *Query) Folded() *Query {
	if q == nil || q.fold == nil {
		return nil
	}
	q.fold.once.Do(func() {
		e := classad.FoldConstantsAt(q.prog.expr, q.prog.now)
		if magic, _ := currentTimeRefs(e); magic {
			return
		}
		p := CompileProgram(e)
		p.now = q.prog.now
		q.fold.q = &Query{prog: p}
	})
	return q.fold.q
}

// currentTimeRefs reports whether expr reads CurrentTime unscoped or as MY.CurrentTime
// (magic), which FoldConstantsAt folds, and whether it reads CurrentTime in any form
// (named): those, TARGET.CurrentTime, PARENT.CurrentTime and a select of CurrentTime from
// a record.
func currentTimeRefs(expr ast.Expr) (magic, named bool) {
	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		if magic || e == nil {
			return
		}
		switch v := e.(type) {
		case *ast.AttributeReference:
			if classad.IsCurrentTimeName(v.Name) {
				named = true
				magic = classad.IsCurrentTimeRef(v)
			}
		case *ast.ParenExpr:
			walk(v.Inner)
		case *ast.UnaryOp:
			walk(v.Expr)
		case *ast.BinaryOp:
			walk(v.Left)
			walk(v.Right)
		case *ast.ElvisExpr:
			walk(v.Left)
			walk(v.Right)
		case *ast.ConditionalExpr:
			walk(v.Condition)
			walk(v.TrueExpr)
			walk(v.FalseExpr)
		case *ast.ListLiteral:
			for _, el := range v.Elements {
				walk(el)
			}
		case *ast.FunctionCall:
			for _, a := range v.Args {
				walk(a)
			}
		case *ast.SelectExpr:
			if classad.IsCurrentTimeName(v.Attr) {
				named = true
			}
			walk(v.Record)
		case *ast.SubscriptExpr:
			walk(v.Container)
			walk(v.Index)
		case *ast.RecordLiteral:
			if v.ClassAd != nil {
				for _, attr := range v.ClassAd.Attributes {
					walk(attr.Value)
				}
			}
		}
	}
	walk(expr)
	return magic, named
}

// foldedExpr is the source expression with its constants folded for the planner. A pinned
// CurrentTime folds to the pinned time, which is exact for an ad that does not define
// CurrentTime (see Probe for what the planner may assume about that).
func (q *Query) foldedExpr() ast.Expr {
	if q.prog.foldNow {
		return classad.FoldConstantsAt(q.prog.expr, q.prog.now)
	}
	return classad.FoldConstants(q.prog.expr)
}

// Parse parses a ClassAd expression string and compiles it into a Query.
func Parse(exprStr string) (*Query, error) {
	expr, err := parser.ParseExpr(exprStr)
	if err != nil {
		return nil, err
	}
	return Compile(expr), nil
}

// ReadsCurrentTime reports whether the query reads the magic CurrentTime attribute
// unscoped or as MY.CurrentTime, so its probes assume the ads do not define CurrentTime
// (see Probe). TARGET.CurrentTime and a select of CurrentTime are pinned but not folded.
func (q *Query) ReadsCurrentTime() bool { return q != nil && q.prog != nil && q.prog.foldNow }

// Program returns the underlying compiled program.
func (q *Query) Program() *Program { return q.prog }

// Expr returns the source expression the query was compiled from (nil if empty).
func (q *Query) Expr() ast.Expr {
	if q == nil || q.prog == nil {
		return nil
	}
	return q.prog.expr
}

// ReadAttrs returns the distinct unscoped attribute names the query may read.
func (q *Query) ReadAttrs() []string { return q.prog.readAttrs }

// Eval evaluates the query against scope and returns the raw Value.
func (q *Query) Eval(scope *classad.ClassAd) classad.Value { return Run(q.prog, scope) }

// Matches reports whether the query evaluates to boolean true against scope.
// Undefined, error, and non-boolean results are treated as non-matches, matching
// how a ClassAd requirement/constraint is applied.
func (q *Query) Matches(scope *classad.ClassAd) bool {
	v := Run(q.prog, scope)
	b, err := v.BoolValue()
	return err == nil && b
}
