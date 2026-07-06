package vm

import (
	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// Native reports whether the query compiled entirely to native instructions,
// with no delegated (OpEvalNode) subtrees. Only native queries can be evaluated
// with EvalResolved, because a delegated subtree needs a real ClassAd scope for
// its function/select/subscript/list semantics.
func (q *Query) Native() bool { return len(q.prog.nodes) == 0 }

// EvalResolved evaluates the query using a custom attribute resolver instead of a
// ClassAd scope, reusing the Matcher's evaluator and stack. resolver(name, scope)
// returns the value of an attribute reference; it lets the query run against an
// alternate backing (e.g. an encoded ad) with no ClassAd materialized.
//
// The query must be Native. The result equals evaluating the same query against a
// ClassAd whose attributes return the same values; a cyclic reference resolves to
// an error value.
func (m *Matcher) EvalResolved(resolver func(name string, scope ast.AttributeScope) classad.Value) (result classad.Value) {
	m.ev.SetScope(nil)
	m.ev.SetResolver(resolver)
	defer m.ev.SetResolver(nil)
	defer classad.RecoverCyclic(&result)
	result, m.stack = exec(m.prog, m.ev, m.stack)
	return result
}
