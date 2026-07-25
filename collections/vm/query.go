package vm

import (
	"strings"

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
// When the expression references the magic CurrentTime attribute, its constants are folded
// once here (FoldConstants resolves CurrentTime to the current time) so a single "now" is
// baked into both the program and its index probes -- the constraint prunes correctly and
// evaluates consistently, instead of the program re-reading the clock per ad. Expressions
// that do not reference CurrentTime are compiled unchanged.
func Compile(expr ast.Expr) *Query {
	if referencesCurrentTime(expr) {
		expr = classad.FoldConstants(expr)
	}
	return &Query{prog: CompileProgram(expr)}
}

// referencesCurrentTime reports whether expr reads the magic CurrentTime attribute (an
// unscoped reference by that name). It reuses the read-collection walk, so it sees the same
// unscoped references the planner does.
func referencesCurrentTime(expr ast.Expr) bool {
	for _, n := range collectReads(expr, map[string]bool{}, nil) {
		if strings.EqualFold(n, "CurrentTime") {
			return true
		}
	}
	return false
}

// Parse parses a ClassAd expression string and compiles it into a Query.
func Parse(exprStr string) (*Query, error) {
	expr, err := parser.ParseExpr(exprStr)
	if err != nil {
		return nil, err
	}
	return Compile(expr), nil
}

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
