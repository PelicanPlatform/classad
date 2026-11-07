package vm

import "github.com/PelicanPlatform/classad/classad"

// Run executes p against scope and returns the resulting Value. It produces the
// same value that classad evaluation of the source expression would: value
// operations are delegated to the classad evaluator hooks, and a cyclic
// reference resolves to an error value (as at the tree-walker's entry points).
//
// Known limitation: for a self-referential cyclic lazy list (e.g. A = {a} where
// "a" case-folds to "A"), the value both engines produce is a list nested to the
// evaluator's depth limit terminating in an error element. The tree-walker folds
// the source expression's nesting depth into the list's captured depth, whereas
// this flat interpreter does not, so the two bottom out at slightly different
// nesting depths. Only such cyclic lists are affected; every non-cyclic
// expression is bit-identical to the tree-walker (enforced by FuzzDifferential).
func Run(p *Program, scope *classad.ClassAd) (result classad.Value) {
	ev := classad.NewEvaluator(scope)
	ev.PinCurrentTime(p.now)
	defer classad.RecoverCyclic(&result)
	result, _, _, _ = exec(p, ev, make([]classad.Value, 0, 16), DefaultStepBudget)
	return result
}

// RunLimited is Run with an explicit step budget. ok=false means the budget ran out, in which case
// the value is ERROR.
//
// The two are distinguishable because they are different kinds of thing: ERROR is an answer the
// program computed, while exhausting the budget means it did not finish.
func RunLimited(p *Program, scope *classad.ClassAd, budget int) (result classad.Value, ok bool) {
	ev := classad.NewEvaluator(scope)
	ev.PinCurrentTime(p.now)
	ok = true
	defer classad.RecoverCyclic(&result)
	result, _, _, ok = exec(p, ev, make([]classad.Value, 0, 16), budget)
	return result, ok
}

// exec runs p against ev, using stack as scratch (stack[:0] is taken first). It
// returns the result and the final stack slice, whose backing array the caller
// may retain to amortize allocation across evaluations (see Matcher).
func exec(p *Program, ev *classad.Evaluator, stack []classad.Value, budget int) (classad.Value, []classad.Value, int, bool) {
	stack = stack[:0]
	code := p.code
	steps := 0
	// A script's locals. The zero Value is UNDEFINED, which is what an unwritten local reads as,
	// so nothing initializes them. Nil -- and so never indexed -- for an expression program, which
	// has no locals at all.
	var locals []classad.Value
	if n := len(p.locals); n > 0 {
		locals = make([]classad.Value, n)
	}
	for ip := 0; ip < len(code); {
		in := code[ip]
		if isValueOp(in.Op) {
			steps++
			if steps > budget {
				return classad.NewErrorValue(), stack, steps, false
			}
		}
		switch in.Op {
		case OpPushConst:
			stack = append(stack, p.consts[in.A])
			ip++
		case OpPushTrue:
			stack = append(stack, classad.NewBoolValue(true))
			ip++
		case OpPushFalse:
			stack = append(stack, classad.NewBoolValue(false))
			ip++
		case OpPushUndef:
			stack = append(stack, classad.NewUndefinedValue())
			ip++
		case OpPushError:
			stack = append(stack, classad.NewErrorValue())
			ip++
		case OpLoadRef:
			r := p.refs[in.A]
			stack = append(stack, ev.ResolveRefNode(r.node))
			ip++
		case OpBinop:
			n := len(stack)
			right, left := stack[n-1], stack[n-2]
			stack = stack[:n-1]
			stack[n-2] = ev.ApplyBinaryOp(p.ops[in.A], left, right)
			ip++
		case OpUnop:
			n := len(stack)
			stack[n-1] = ev.ApplyUnaryOp(p.ops[in.A], stack[n-1])
			ip++
		case OpShortAnd:
			if res, done := ev.ShortCircuit("&&", stack[len(stack)-1]); done {
				stack[len(stack)-1] = res
				ip = int(in.A)
			} else {
				ip++
			}
		case OpShortOr:
			if res, done := ev.ShortCircuit("||", stack[len(stack)-1]); done {
				stack[len(stack)-1] = res
				ip = int(in.A)
			} else {
				ip++
			}
		case OpCombineAnd:
			n := len(stack)
			right, left := stack[n-1], stack[n-2]
			stack = stack[:n-1]
			stack[n-2] = ev.ApplyBinaryOp("&&", left, right)
			ip++
		case OpCombineOr:
			n := len(stack)
			right, left := stack[n-1], stack[n-2]
			stack = stack[:n-1]
			stack[n-2] = ev.ApplyBinaryOp("||", left, right)
			ip++
		case OpJmpIfNotUndef:
			if !stack[len(stack)-1].IsUndefined() {
				ip = int(in.A) // leave left on the stack as the result
			} else {
				stack = stack[:len(stack)-1] // discard undefined; fall through to fallback
				ip++
			}
		case OpJmp:
			ip = int(in.A)
		case OpBranchCond:
			n := len(stack)
			cond := stack[n-1]
			stack = stack[:n-1]
			t := p.conds[in.A]
			switch classad.ConditionState(cond) {
			case classad.CondTrue:
				ip++
			case classad.CondFalse:
				ip = int(t.onFalse)
			case classad.CondUndef:
				ip = int(t.onUndef)
			default:
				ip = int(t.onError)
			}
		case OpCallFunc:
			f := p.funcs[in.A]
			base := len(stack) - f.argc
			// The argument values are handed over as a slice of the stack itself, which
			// is safe in both directions: no built-in retains or mutates its args (every
			// list a built-in returns is freshly allocated), and the result is already a
			// value by the time the stack is truncated under it.
			res := classad.ApplyFunction(ev, f.name, stack[base:])
			stack = append(stack[:base], res)
			ip++
		case OpEvalNode:
			stack = append(stack, evalDelegated(ev, p.nodes[in.A], p.nodeLocals[in.A], locals))
			ip++
		case OpLoadLocal:
			stack = append(stack, locals[in.A])
			ip++
		case OpStoreLocal:
			locals[in.A] = stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			ip++
		case OpLoadAlias:
			r := p.aliasRefs[in.A]
			if v, ok := ev.ResolveAlias(r.alias, r.name); ok {
				stack = append(stack, v)
			} else {
				// No alias resolver: this is an ordinary evaluation against a real
				// record, so select out of it exactly as the delegated form did.
				stack = append(stack, ev.Evaluate(r.node))
			}
			ip++
		case OpShortSubscript:
			// The container is the result when it is UNDEFINED or ERROR, and it is already on the
			// stack, so the jump path writes nothing. Falling through evaluates the index.
			if v := stack[len(stack)-1]; v.IsUndefined() || v.IsError() {
				ip = int(in.A)
			} else {
				ip++
			}
		case OpSubscript:
			n := len(stack)
			index, container := stack[n-1], stack[n-2]
			stack = stack[:n-1]
			stack[n-2] = ev.ApplySubscript(container, index)
			ip++
		case OpStrListMember:
			stack[len(stack)-1] = p.strLists[in.A].eval(stack[len(stack)-1])
			ip++
		case OpMember:
			stack[len(stack)-1] = p.members[in.A].eval(stack[len(stack)-1])
			ip++
		default:
			stack = append(stack, classad.NewErrorValue())
			ip++
		}
	}
	if len(stack) == 0 {
		return classad.NewUndefinedValue(), stack, steps, true
	}
	return stack[len(stack)-1], stack, steps, true
}
