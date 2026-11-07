package vm

import (
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// A DELEGATED SUBTREE READS THE SCRIPT'S LOCALS.
//
// Some expressions are handed to the tree-walking evaluator rather than lowered natively: a list
// literal, an ad literal, a select, a lazy built-in (see compiler.delegate). That
// evaluator resolves a bare name against the AD, and a script's locals live in registers it has
// never heard of. The bridge is classad's partial resolver: before running such a subtree the
// interpreter installs one backed by the current register file, so a name the compiler resolved to
// a local is answered from its register and every other name is DECLINED and resolved against the
// ad exactly as it would be with nothing installed.
//
// WHICH NAMES THE RESOLVER ANSWERS IS DECIDED AT COMPILE TIME, not at run time. The compiler walks
// the subtree, finds the bare names that are bound as locals where the subtree sits, and records
// name -> register for just those. Nothing is decided by looking at the register file, which is
// what keeps the binding rules the same inside a delegated subtree as outside one:
//
//   - a bare bound name is the local;
//   - MY.x is the attribute, because the resolver answers only an unscoped reference;
//   - `x = x + 1` reads the attribute and binds the local, because the right-hand side is compiled
//     before the name is bound and so has no binding to find;
//   - a function body sees the function's locals, because the compiler's local map IS the body's
//     while the body is emitted.
//
// WHERE A LOCAL STILL CANNOT BE READ. A resolver lives on one Evaluator, and three constructs
// evaluate in an evaluator of their own, built from a captured scope that carries no resolver:
//
//	{a, x}            a list literal is LAZY -- its elements are evaluated on demand, in an
//	                  evaluator built from the ad the literal was written in, possibly long after
//	                  the delegated evaluation returned (Value.lazyEvaluator)
//	[a = x]           an ad literal is a separate ad whose attributes resolve against itself
//	countMatches(x,L) arg0 is evaluated once per nested ad, in a child evaluator (evalInNested)
//
// A local read in one of those positions is refused at compile time, as every local read inside a
// delegated subtree used to be: LocalCaptureError. Computing it is not an option, because there is
// no moment at which the read could be answered -- a list element may be materialized after the
// script has finished and its registers are gone.

// localBind binds a name a delegated subtree reads to the register that holds it.
//
// A SLICE AND NOT A MAP, matched with EqualFold. The resolver is called once per reference per
// evaluation, and a map keyed by the folded name would fold -- and so allocate -- on every call,
// since the evaluator hands the resolver the original-cased name. A delegated subtree mentions one
// or two locals, so the scan is shorter than the hash would be and allocates nothing.
type localBind struct {
	name string // as the script spelled it at the point of the read
	reg  int32  // index into Program.locals, which is the register index
}

// lookupBind returns the register bound to name, or -1. Names are matched case-insensitively, as
// ClassAd matches every attribute name.
func lookupBind(binds []localBind, name string) int32 {
	for i := range binds {
		if strings.EqualFold(binds[i].name, name) {
			return binds[i].reg
		}
	}
	return -1
}

// evalDelegated evaluates a delegated subtree against ev, with the script locals that subtree reads
// overlaid on the ad from regs. binds names them; it is empty for an expression program, which has
// no locals, and the evaluation is then exactly the bare ev.Evaluate it has always been.
//
// binds' registers index regs, which means the STACK form's local indices for the stack
// interpreter and the lowered registers for the register one (see regBinds). Each interpreter
// passes its own table, so neither has to know about the other's numbering.
//
// Both interpreters call this -- the stack form for OpEvalNode, the register form for REvalNode --
// so the two cannot come to hold different ideas of what a delegated subtree sees.
//
// The resolver is installed per node rather than per run because the binding set is per node: two
// delegated subtrees in one script read different locals, and a function body's subtree must not
// see the caller's. It is removed however the evaluation returns, a cyclic-reference panic
// unwinding through it included -- an Evaluator is reused across records by Matcher, and a resolver
// left behind would answer for the next one.
func evalDelegated(ev *classad.Evaluator, node ast.Expr, binds []localBind, regs []classad.Value) classad.Value {
	if len(binds) == 0 {
		return ev.Evaluate(node)
	}
	ev.SetPartialResolver(func(name string, scope ast.AttributeScope) (classad.Value, bool) {
		// MY.x, TARGET.x and PARENT.x are the ATTRIBUTE, here as everywhere else: a scope
		// names an ad, and a local is not in one.
		if scope != ast.NoScope {
			return classad.Value{}, false
		}
		if reg := lookupBind(binds, name); reg >= 0 {
			return regs[reg], true
		}
		return classad.Value{}, false
	})
	defer ev.SetPartialResolver(nil)
	return ev.Evaluate(node)
}

// scanDelegatedLocals resolves the local reads of a delegated subtree against the bindings in
// force, returning the ones the overlay can answer and the first one it cannot.
//
// reachable tracks whether the position being walked is evaluated by the evaluator the overlay is
// installed on. It goes false under the three re-scoping constructs named at the top of this file,
// and never goes back to true: a ternary nested inside a list literal is still inside the list
// literal.
func scanDelegatedLocals(expr ast.Expr, locals map[string]int32) (binds []localBind, blocked *LocalCaptureError) {
	var walk func(e ast.Expr, reachable bool)
	walk = func(e ast.Expr, reachable bool) {
		switch v := e.(type) {
		case nil:
			return
		case *ast.AttributeReference:
			if v.Scope != ast.NoScope {
				return
			}
			reg, bound := locals[foldLocal(v.Name)]
			if !bound {
				return
			}
			if !reachable {
				if blocked == nil {
					blocked = &LocalCaptureError{Local: v.Name, Expr: expr.String()}
				}
				return
			}
			if lookupBind(binds, v.Name) < 0 {
				binds = append(binds, localBind{name: v.Name, reg: reg})
			}
		case *ast.ParenExpr:
			walk(v.Inner, reachable)
		case *ast.UnaryOp:
			walk(v.Expr, reachable)
		case *ast.BinaryOp:
			walk(v.Left, reachable)
			walk(v.Right, reachable)
		case *ast.ElvisExpr:
			walk(v.Left, reachable)
			walk(v.Right, reachable)
		case *ast.ConditionalExpr:
			walk(v.Condition, reachable)
			walk(v.TrueExpr, reachable)
			walk(v.FalseExpr, reachable)
		case *ast.ListLiteral:
			for _, el := range v.Elements {
				walk(el, false)
			}
		case *ast.RecordLiteral:
			if v.ClassAd != nil {
				for _, attr := range v.ClassAd.Attributes {
					walk(attr.Value, false)
				}
			}
		case *ast.FunctionCall:
			perAd := reScopesFirstArg(v.Name)
			for i, a := range v.Args {
				walk(a, reachable && (!perAd || i != 0))
			}
		case *ast.SelectExpr:
			walk(v.Record, reachable)
		case *ast.SubscriptExpr:
			walk(v.Container, reachable)
			walk(v.Index, reachable)
		}
	}
	walk(expr, true)
	return binds, blocked
}

// reScopesFirstArg reports whether a built-in evaluates its first argument in the scope of some
// other ad rather than in the calling scope. The two that do evaluate it once per nested ad in
// their second argument's list, each in a child evaluator the overlay does not reach.
func reScopesFirstArg(name string) bool {
	switch strings.ToLower(name) {
	case "countmatches", "evalineachcontext":
		return true
	}
	return false
}
