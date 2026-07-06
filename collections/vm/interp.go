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
	defer classad.RecoverCyclic(&result)
	result, _ = exec(p, ev, make([]classad.Value, 0, 16))
	return result
}

// exec runs p against ev, using stack as scratch (stack[:0] is taken first). It
// returns the result and the final stack slice, whose backing array the caller
// may retain to amortize allocation across evaluations (see Matcher).
func exec(p *Program, ev *classad.Evaluator, stack []classad.Value) (classad.Value, []classad.Value) {
	stack = stack[:0]
	code := p.code
	for ip := 0; ip < len(code); {
		in := code[ip]
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
			stack = append(stack, ev.ResolveRef(r.name, r.scope))
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
		case OpEvalNode:
			stack = append(stack, ev.Evaluate(p.nodes[in.A]))
			ip++
		default:
			stack = append(stack, classad.NewErrorValue())
			ip++
		}
	}
	if len(stack) == 0 {
		return classad.NewUndefinedValue(), stack
	}
	return stack[len(stack)-1], stack
}
