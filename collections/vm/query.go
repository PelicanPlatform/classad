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
}

// Compile compiles an already-parsed expression into a Query.
//
// When the expression references CurrentTime, Compile reads the clock once and pins it: a
// CurrentTime the ad does not define evaluates to that time on every evaluation, so the
// program and its index probes share a single "now" instead of the program re-reading the
// clock per ad. An ad that defines CurrentTime supplies its own value, as it does to the
// evaluator; the expression itself is compiled unchanged.
func Compile(expr ast.Expr) *Query {
	p := CompileProgram(expr)
	if magic, named := currentTimeRefs(expr); named {
		p.now = time.Now().Unix()
		p.foldNow = magic
	}
	return &Query{prog: p}
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
