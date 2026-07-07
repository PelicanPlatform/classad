package vm

import "github.com/PelicanPlatform/classad/classad"

// Matcher evaluates one compiled Query against many ads while reusing its
// evaluator and value stack, so a table scan pays the per-ad cost of the ClassAd
// evaluation itself but not a fresh evaluator + stack allocation per ad. It is
// semantically identical to calling Query.Eval / Query.Matches for each ad; it
// only removes allocations.
//
// A Matcher holds mutable state (the reused evaluator and stack) and is NOT safe
// for concurrent use. A parallel scan should use one Matcher per goroutine.
type Matcher struct {
	prog  *Program
	ev    *classad.Evaluator
	stack []classad.Value
}

// Matcher returns a reusable Matcher for the query. Create one per scanning
// goroutine.
func (q *Query) Matcher() *Matcher {
	return &Matcher{
		prog:  q.prog,
		ev:    classad.NewEvaluator(nil),
		stack: make([]classad.Value, 0, 16),
	}
}

// Eval evaluates the query against scope and returns the raw Value, reusing the
// Matcher's evaluator and stack. The result equals Query.Eval(scope); a cyclic
// reference resolves to an error value.
func (m *Matcher) Eval(scope *classad.ClassAd) (result classad.Value) {
	m.ev.SetScope(scope)
	defer classad.RecoverCyclic(&result)
	result, m.stack = exec(m.prog, m.ev, m.stack)
	return result
}

// Matches reports whether the query evaluates to boolean true against scope,
// matching Query.Matches (undefined/error/non-boolean are non-matches).
func (m *Matcher) Matches(scope *classad.ClassAd) bool {
	v := m.Eval(scope)
	b, err := v.BoolValue()
	return err == nil && b
}
