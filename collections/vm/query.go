package vm

import (
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
func Compile(expr ast.Expr) *Query { return &Query{prog: CompileProgram(expr)} }

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
