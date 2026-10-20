package vm

import (
	"time"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// Query is a compiled boolean constraint over ads (a compiled Program plus the
// convenience of a match predicate). The store uses ReadAttrs for planning.
type Query struct {
	prog *Program
}

// Compile compiles an already-parsed expression into a Query.
//
// When the expression references the magic CurrentTime attribute, Compile reads the clock
// once and pins it: an ad that does not define CurrentTime evaluates it to that time on
// every evaluation, so the program and its index probes share a single "now" instead of
// the program re-reading the clock per ad. An ad that defines CurrentTime supplies its
// own value, as it does to the evaluator; the expression itself is compiled unchanged.
func Compile(expr ast.Expr) *Query {
	p := CompileProgram(expr)
	if referencesCurrentTime(expr) {
		p.now = time.Now().Unix()
	}
	return &Query{prog: p}
}

// referencesCurrentTime reports whether expr reads the magic CurrentTime attribute (an
// unscoped or MY reference by that name), anywhere in the expression.
func referencesCurrentTime(expr ast.Expr) bool {
	found := false
	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		if found || e == nil {
			return
		}
		switch v := e.(type) {
		case *ast.AttributeReference:
			found = classad.IsCurrentTimeRef(v)
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
	return found
}

// foldedExpr is the source expression with its constants folded for the planner. A pinned
// CurrentTime folds to the pinned time, which is exact for an ad that does not define
// CurrentTime (see Probe for what the planner may assume about that).
func (q *Query) foldedExpr() ast.Expr {
	if q.prog.now != 0 {
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

// ReadsCurrentTime reports whether the query reads the magic CurrentTime attribute, so
// its probes assume the ads do not define CurrentTime (see Probe).
func (q *Query) ReadsCurrentTime() bool { return q != nil && q.prog != nil && q.prog.now != 0 }

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
