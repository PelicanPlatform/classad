package vm

import (
	"strings"
	"time"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// EVALUATING A JOIN CONDITION WITHOUT A JOINED ROW.
//
// A join condition is written over the joined row -- `j.Owner == h.Owner` -- so evaluating it
// has always meant building that row first: a ClassAd holding each side under its alias. That
// is one composite ad per candidate PAIR, and the pair count is the product, not the sum.
//
// It need not be. `alias.Attr` is a SelectExpr, which the compiler delegates to the
// tree-walking evaluator (see compile.go's default case) because selecting a member out of a
// record needs the record. But when the record half is a bare reference to a name the caller
// has declared to be a join alias, nothing about it is dynamic: the alias is known at compile
// time, and the only question left at evaluation is "what is Attr on side k", which a caller
// holding the two sides separately can answer directly.
//
// So CompileAliases resolves the alias ONCE, at compile time, to an index, and emits
// OpLoadAlias. Evaluation then asks the installed alias resolver for (side index, attribute
// name) and never selects, never materializes, and never compares an alias name.
//
// WHAT MAKES THIS SAFE TO COMPILE UNCONDITIONALLY. The opcode carries the original
// SelectExpr. With no alias resolver installed it evaluates that node against the scope --
// bit-identical to what the reference compiled to before this existed. So an aliased program
// run against a real composite ad gives the same answer as an unaliased one, which is exactly
// what the differential test asserts, and there is no state in which a missing resolver
// produces a wrong answer rather than the slow one.

// aliasRef is a pooled qualified reference targeted by OpLoadAlias.
type aliasRef struct {
	// alias indexes the alias list the program was compiled with.
	alias int
	// name is the attribute's name as written. It is handed to the resolver with its
	// original casing, as SetResolver's name is, because the resolver is what knows how its
	// own backing stores names.
	name string
	// node is the original select expression, evaluated against the scope when no alias
	// resolver is installed. Read-only after compilation, so one program can be evaluated
	// concurrently by several Matchers.
	node ast.Expr
}

// aliasIndex is the compile-time alias table: a case-folded alias name to its index.
// ClassAd attribute names are case-insensitive and an alias is an attribute name of the
// joined row, so `J.Owner` and `j.Owner` must compile to the same load.
type aliasIndex map[string]int

func newAliasIndex(aliases []string) aliasIndex {
	if len(aliases) == 0 {
		return nil
	}
	ix := make(aliasIndex, len(aliases))
	for i, a := range aliases {
		// First spelling wins, so a duplicated alias cannot renumber the earlier one out
		// from under a reference already compiled against it.
		if _, dup := ix[strings.ToLower(a)]; !dup {
			ix[strings.ToLower(a)] = i
		}
	}
	return ix
}

// CompileAliases compiles expr with aliases known to name the sides of a join, so a qualified
// reference `alias.Attr` to one of them compiles to a native load rather than a delegated
// select. The result is evaluated with Matcher.EvalAliased; evaluated any other way it
// behaves exactly as the same expression compiled with Compile.
//
// An alias the expression does not use costs nothing, and a qualified reference naming
// something that is NOT in aliases is left delegated -- it may be a genuine select over a
// nested record, which only the scope can answer.
func CompileAliases(expr ast.Expr, aliases []string) *Query {
	return compileAliasesAt(expr, aliases, 0)
}

// ParseAliases is CompileAliases on expression text.
func ParseAliases(exprStr string, aliases []string) (*Query, error) {
	expr, err := parser.ParseExpr(exprStr)
	if err != nil {
		return nil, err
	}
	return CompileAliases(expr, aliases), nil
}

// AliasNative reports whether the query compiled entirely to native instructions, counting
// OpLoadAlias as native. It is the EvalAliased counterpart of Native: a query with qualified
// references is not Native (EvalResolved has no way to answer them), but it is AliasNative,
// and only an AliasNative query can be evaluated with EvalAliased.
func (q *Query) AliasNative() bool { return q != nil && q.prog != nil && len(q.prog.nodes) == 0 }

// Aliased reports whether the query carries qualified references, i.e. whether EvalAliased
// would resolve anything that Eval would have to select.
func (q *Query) Aliased() bool { return q != nil && q.prog != nil && len(q.prog.aliasRefs) > 0 }

// EvalAliased evaluates the query with resolve answering its qualified references, reusing
// the Matcher's evaluator and stack. resolve(alias, name) is given the alias's INDEX in the
// list the query was compiled with, and returns the attribute's value or undefined when that
// side does not define it -- which is also how an ABSENT side reads, as a LEFT JOIN's
// null-extended row needs.
//
// Unqualified references are resolved through refResolve, or undefined when it is nil; a
// join condition over a joined row has none, but a WHERE conjunct may.
//
// ok is false when the query is not AliasNative, and then NOTHING is evaluated: the caller
// must build the row and evaluate against it instead. That is a return value rather than a
// documented precondition, because the failure is otherwise invisible -- a delegated subtree
// evaluated against the nil scope this installs does not fail, it quietly reads undefined,
// and a condition that reads undefined does not error, it just stops matching. A join would
// silently return too few rows. Every caller must have a fallback, so every caller is made
// to say what it is.
func (m *Matcher) EvalAliased(resolve func(alias int, name string) classad.Value,
	refResolve func(name string, scope ast.AttributeScope) classad.Value) (result classad.Value, ok bool) {
	if len(m.prog.nodes) > 0 {
		return classad.NewUndefinedValue(), false
	}
	m.ev.SetScope(nil)
	m.ev.SetResolver(refResolve)
	m.ev.SetAliasResolver(resolve)
	defer m.ev.SetAliasResolver(nil)
	defer m.ev.SetResolver(nil)
	defer classad.RecoverCyclic(&result)
	result, m.stack, _, _ = exec(m.prog, m.ev, m.stack, DefaultStepBudget)
	return result, true
}

// emitAliasSelect compiles a select over a known alias to OpLoadAlias and reports whether it
// did. It declines anything whose record half is not a bare unscoped reference to a declared
// alias -- a scoped one (MY.x.y), a nested select, a subscript -- because for those the
// record is not the join side the alias names and only the scope can produce it.
func (c *compiler) emitAliasSelect(v *ast.SelectExpr) bool {
	if len(c.aliases) == 0 {
		return false
	}
	ref, ok := v.Record.(*ast.AttributeReference)
	if !ok || ref.Scope != ast.NoScope {
		return false
	}
	idx, ok := c.aliases[strings.ToLower(ref.Name)]
	if !ok {
		return false
	}
	c.ins(OpLoadAlias, c.addAliasRef(idx, v.Attr, v))
	return true
}

func (c *compiler) addAliasRef(alias int, name string, node ast.Expr) int32 {
	c.p.aliasRefs = append(c.p.aliasRefs, aliasRef{alias: alias, name: name, node: node})
	return int32(len(c.p.aliasRefs) - 1)
}

// compileAliasesAt is CompileAliases with the pinned time given (0 reads the clock), mirroring
// compileAt so an aliased query folds CurrentTime on the same terms as an ordinary one.
func compileAliasesAt(expr ast.Expr, aliases []string, now int64) *Query {
	p := compileProgramAliases(expr, newAliasIndex(aliases))
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

// AliasRef is one qualified reference a compiled program makes to a join side: which side,
// by its index in the alias list the program was compiled with, and which attribute on it.
type AliasRef struct {
	Alias int
	Name  string
}

// AliasRefs lists the qualified references the program makes, as written and with
// duplicates, in compilation order.
//
// It is only an ANSWER for an AliasNative program, and a caller must check that first. A
// delegated node holds an arbitrary expression evaluated against the whole scope, which can
// reach a side in ways no list of attribute names describes -- `h` passed to a function, a
// select through a nested record, a reference the compiler never saw because the subtree was
// handed over whole. So a caller deciding what to READ from a side must treat a program that
// is not AliasNative as needing the entire ad, not as making the references it happens to
// list here.
func (q *Query) AliasRefs() []AliasRef {
	if q == nil || q.prog == nil {
		return nil
	}
	out := make([]AliasRef, 0, len(q.prog.aliasRefs))
	for _, r := range q.prog.aliasRefs {
		out = append(out, AliasRef{Alias: r.alias, Name: r.name})
	}
	return out
}
