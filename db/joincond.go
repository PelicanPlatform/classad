package db

import (
	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// EVALUATING THE CONDITION WITHOUT BUILDING THE PAIR.
//
// condHolds used to build a composite ClassAd -- the two sides under their aliases -- for
// every CANDIDATE PAIR, then evaluate the ON terms against it. The pair count is the product
// of the sides, not the sum, so that ad was the join's most multiplied allocation: it was
// built for every pair the hash produced, including every pair the condition then rejected.
//
// vm.CompileAliases removes the need for it. With the two aliases declared, `l.Attr` compiles
// to a load that asks for (side index, attribute name) rather than to a select that needs a
// record, so the terms evaluate against the two sides exactly where they already are.
//
// The fallback is not optional and not rare. A term carrying anything that needs a real
// record -- a non-strict function, a select over a nested record -- is declined by
// EvalAliased, and those terms are evaluated the old way. So this holds a compiled form of
// BOTH, and uses the fast one only when every term accepted it.

// forceCompositeCond disables the aliased path. It exists for the equivalence test, which
// runs the SAME join both ways and compares the rows: an optimization that changes which
// pairs survive is the only way this can be wrong, and comparing the two paths is the only
// thing that can see it.
//
// It is a plain variable rather than a parameter because nothing outside a test may set it,
// and a test that does must not call t.Parallel -- Go runs parallel tests only after the
// serial ones finish, which is what keeps this from needing synchronisation.
var forceCompositeCond bool

// aliasedCond is a join condition compiled for evaluation against the two sides separately.
// It is nil when any term declined, in which case the composite path runs unchanged.
type aliasedCond struct {
	ms []*vm.Matcher
}

// leftSide and rightSide are the alias indexes compileAliasedCond declares, and the ones the
// resolver is handed back at evaluation.
const (
	leftSide  = 0
	rightSide = 1
)

// compileAliasedCond compiles the ON terms with both aliases known. It returns nil when any
// term is not AliasNative -- all or nothing, because a join that evaluated some terms one way
// and some the other would build the composite anyway, which is the cost this avoids.
func compileAliasedCond(srcs []string, leftAlias, rightAlias string) *aliasedCond {
	if len(srcs) == 0 || forceCompositeCond {
		return nil
	}
	ms := make([]*vm.Matcher, 0, len(srcs))
	for _, src := range srcs {
		q, err := vm.ParseAliases(src, []string{leftAlias, rightAlias})
		if err != nil {
			// The term already parsed once, into p.cond; a second parser disagreeing is
			// not something to paper over, so decline and let the composite path -- which
			// holds the form that DID parse -- run.
			return nil
		}
		if !q.AliasNative() {
			return nil
		}
		ms = append(ms, q.Matcher())
	}
	return &aliasedCond{ms: ms}
}

// holds reports whether every term is TRUE for this pair, reading the two sides directly.
// ok is false when any term declined at evaluation, and then nothing it returned may be
// used: the caller must fall back to the composite, because a declined term reads undefined
// rather than failing and the join would silently return too few rows.
//
// A nil right side -- an unmatched LEFT row -- resolves undefined for every attribute of it,
// which is what an absent member of the composite evaluates to.
func (a *aliasedCond) holds(l, r *joinRow) (held, ok bool) {
	resolve := func(side int, name string) classad.Value {
		row := l
		if side == rightSide {
			row = r
		}
		if row == nil {
			return classad.NewUndefinedValue()
		}
		// The row answers from its bytes when it can, and is decoded when it cannot.
		// Which of the two happened is invisible here and must stay so: the value is the
		// same either way, and a condition that read differently depending on how its row
		// arrived would make the join's answer depend on the read plan.
		return row.eval(name)
	}
	// An unqualified reference in an ON term names an attribute of the joined row itself,
	// and the joined row has only the two members. So it is undefined, which is what it
	// evaluates to against the composite.
	unqualified := func(string, ast.AttributeScope) classad.Value {
		return classad.NewUndefinedValue()
	}
	for _, m := range a.ms {
		v, accepted := m.EvalAliased(resolve, unqualified)
		if !accepted {
			// Unreachable by construction: compileAliasedCond admits only AliasNative
			// terms, which is the same condition EvalAliased declines on. Kept because
			// the cost is a branch and the failure it guards is silent -- a declined
			// term reads undefined, so the join would return too few rows rather than
			// fail. TestAliasedCondHoldsDeclines covers it directly, since no join can.
			return false, false
		}
		if !isTrue(v) {
			return false, true
		}
	}
	return true, true
}
