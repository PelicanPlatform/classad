package db

import (
	"sync"

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

// aliasedCond is a join condition compiled for evaluation against the sides separately.
// It is nil when any term declined, in which case the composite path runs unchanged.
type aliasedCond struct {
	// qs are the compiled terms. A vm.Query (its Program) may be evaluated concurrently;
	// a vm.Matcher may NOT -- it carries the evaluation stack, and the package says to use
	// one per goroutine. So the matchers are POOLED rather than cached here: a parallel
	// walk has several goroutines in holds at once, and sharing one matcher between them
	// does not fail, it silently evaluates the wrong thing -- rows vanish and a LEFT row
	// comes back carrying a member nothing matched.
	qs   []*vm.Query
	pool sync.Pool // holds *matcherSet, one set per goroutine
}

// matcherSet is one goroutine's matchers, one per term. The pool holds the POINTER: a slice
// put into a sync.Pool is boxed into an interface, which allocates on every Put and gives
// back exactly the garbage the pool is there to avoid.
type matcherSet struct {
	ms []*vm.Matcher
}

// compileAliasedCond compiles the ON terms with every alias known. It returns nil when any
// term is not AliasNative -- all or nothing, because a join that evaluated some terms one way
// and some the other would build the composite anyway, which is the cost this avoids.
//
// The alias list is the whole chain, not the two sides visible to one step. A step's terms
// cannot name a later side -- compileJoin refuses that before this is reached -- so the extra
// aliases cost a longer compile-time table and nothing at evaluation.
func compileAliasedCond(srcs, aliases []string) *aliasedCond {
	if len(srcs) == 0 || forceCompositeCond {
		return nil
	}
	qs := make([]*vm.Query, 0, len(srcs))
	for _, src := range srcs {
		q, err := vm.ParseAliases(src, aliases)
		if err != nil {
			// The term already parsed once, into the plan's cond; a second parser
			// disagreeing is not something to paper over, so decline and let the composite
			// path -- which holds the form that DID parse -- run.
			return nil
		}
		if !q.AliasNative() {
			return nil
		}
		qs = append(qs, q)
	}
	a := &aliasedCond{qs: qs}
	a.pool.New = func() any { return a.newMatchers() }
	return a
}

// newMatchers builds one goroutine's set of matchers, one per term.
func (a *aliasedCond) newMatchers() *matcherSet {
	ms := make([]*vm.Matcher, len(a.qs))
	for i, q := range a.qs {
		ms[i] = q.Matcher()
	}
	return &matcherSet{ms: ms}
}

// sideResolver answers (side index, attribute name) out of the row's sides.
//
// A side index past what the row holds, and a side that is absent -- the null-extended half
// of an unmatched LEFT row -- both resolve undefined, which is what an absent member of the
// composite evaluates to.
func sideResolver(rows []*joinRow) func(int, string) classad.Value {
	return func(side int, name string) classad.Value {
		if side < 0 || side >= len(rows) || rows[side] == nil {
			return classad.NewUndefinedValue()
		}
		// The row answers from its bytes when it can, and is decoded when it cannot.
		// Which of the two happened is invisible here and must stay so: the value is the
		// same either way, and a condition that read differently depending on how its row
		// arrived would make the join's answer depend on the read plan.
		return rows[side].eval(name)
	}
}

// unqualifiedUndefined answers every unqualified, MY- or TARGET-scoped reference as
// undefined.
//
// An unqualified reference in an ON term names an attribute of the joined row itself, and the
// joined row's only attributes are the aliases. So it is undefined, which is what it
// evaluates to against the composite -- and MY and TARGET name the row for the same reason,
// which is why nothing here maps them to a side.
func unqualifiedUndefined(string, ast.AttributeScope) classad.Value {
	return classad.NewUndefinedValue()
}

// holds reports whether every term is TRUE for this row. ok is false when any term declined
// at evaluation, and then nothing it returned may be used: the caller must fall back to the
// composite, because a declined term reads undefined rather than failing and the join would
// silently return too few rows.
func (a *aliasedCond) holds(rows []*joinRow) (held, ok bool) {
	resolve := sideResolver(rows)
	set, pooled := a.pool.Get().(*matcherSet)
	if !pooled {
		// Unreachable: the pool's New returns this type. Checked rather than asserted
		// because an assertion here would panic mid-join on a mistake that building a
		// fresh set answers for free.
		set = a.newMatchers()
	}
	defer a.pool.Put(set)
	for _, m := range set.ms {
		v, accepted := m.EvalAliased(resolve, unqualifiedUndefined)
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

// aliasedValue is one equality half compiled to read the row's sides directly. It is the
// value twin of aliasedCond: the same compilation, asked for a value rather than a verdict,
// because an equality half produces the hash key rather than a verdict.
type aliasedValue struct {
	// Pooled for the same reason as aliasedCond: the Query is shareable, the Matcher is not.
	q    *vm.Query
	pool sync.Pool // holds *vm.Matcher
}

// compileAliasedValue compiles one equality half over the accumulated row, or returns nil
// when it is not AliasNative and has to be evaluated against a composite.
func compileAliasedValue(src string, aliases []string) *aliasedValue {
	if forceCompositeCond {
		return nil
	}
	q, err := vm.ParseAliases(src, aliases)
	if err != nil || !q.AliasNative() {
		return nil
	}
	v := &aliasedValue{q: q}
	v.pool.New = func() any { return v.q.Matcher() }
	return v
}

// eval returns the half's value for this row. ok is false when the term declined, and the
// caller must then evaluate against a composite -- a declined term reads undefined, which
// would silently put every row in the no-key bucket and match nothing.
func (a *aliasedValue) eval(rows []*joinRow) (classad.Value, bool) {
	m, ok := a.pool.Get().(*vm.Matcher)
	if !ok {
		m = a.q.Matcher() // unreachable; see aliasedCond.holds
	}
	defer a.pool.Put(m)
	return m.EvalAliased(sideResolver(rows), unqualifiedUndefined)
}
