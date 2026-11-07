package vm

import (
	"errors"
	"fmt"
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// USER-DEFINED FUNCTIONS IN A SCRIPT, and the one decision everything else follows from: a call is
// EXPANDED IN PLACE. There is no call instruction, no frame and no return address. The script is
// still one program, lowered to registers once, and a function is a piece of it written down
// somewhere else.
//
// WHAT A CALL COMPILES TO. For `over_cap(MY.RequestMemory, 8192)`:
//
//	<MY.RequestMemory>   ; each argument is evaluated IN THE CALLER, left to right,
//	store  requested     ; exactly once, into that parameter's register
//	<8192>
//	store  cap
//	<the body's statements, with the body's own name map>
//	...
//	load   over_cap#1.return
//
// CALL BY VALUE, AND EVERY ARGUMENT IS EVALUATED. A parameter the body never reads still has its
// argument computed, because the argument is computed before the body exists. That is observable:
// an argument can fault, and a faulting argument faults the script whether or not the parameter is
// used.
//
// THE BODY IS FUNCTION-SCOPED. While the body is emitted the compiler's local map is replaced with
// one holding just the parameters, so a bare name in the body is a parameter, a name the body
// itself binds, or an ATTRIBUTE OF THE SUBJECT AD. The caller's locals are not visible, which is
// the only part of the binding model a function changes and the reason it is worth having: a
// function written once can be called from anywhere in the script without its names colliding with
// the names where it is called.
//
// A FUNCTION IS NOT PURE. A bare name the body does not bind is an attribute, so
// `def over(): return RequestMemory > 4096` is a legal function that reads the ad. It is a function
// of its arguments AND of the ad, exactly as every other expression in a script is.
//
// RECURSION IS REFUSED, and the reason is the inlining rather than a missing keyword. Expanding a
// call in place means expanding whatever that call expands to, and a cycle has no end; there is no
// depth limit that would make it work, because the expansion happens at compile time and the depth
// is not known then. A call that reaches itself, directly or through other functions, is a compile
// error naming the cycle.
//
// ONE RECURSIVE CALL NEEDS NO EXPANSION: `return f(...)` inside f, where the call's value IS the
// function's value and nothing is left to do with it. Such a call needs no second copy of the body
// and no frame to come back to, because a local here is a PLACE -- one register for the life of the
// program -- so the next activation wants the registers this one is using:
//
//	<the new arguments, each into a temporary>
//	store  f#1.tail1.n            ; all of them, BEFORE any parameter is written
//	...
//	load   f#1.tail1.n            ; then copy the temporaries into the parameters
//	store  f#1.n
//	jmp    f#1.reentry            ; clear the activation and go back to the body's first instruction
//
// That is the back edge a `while` already emits, pointed at a function's entry rather than a loop's
// header, which is why it costs the rest of the compiler nothing: no frame-relative addressing on
// the interpreter's hot path, no second call mechanism, and no barrier that liveness and CSE have to
// stop at. The temporaries are not an optimization to remove -- see tailCall. What bounds a
// tail-recursive function is what bounds a `while`: the step budget, with no compile-time depth to
// know and nothing else to check.
//
// NOTHING ELSE BECOMES LEGAL. Mutual recursion still has nowhere to jump to, since the jump goes to
// an entry this expansion owns and f's expansion is not g's. A self-call whose value feeds anything
// -- `return f(x) + 1`, `return g(f(x))`, a call in a binding or a condition -- still has to produce
// a value where it stands, and the only way to produce one is the expansion that has no end. Both
// keep the refusal above.
//
// REGISTERS ARE ALLOCATED ONCE FOR THE WHOLE PROGRAM, so each expansion gets its own: two calls to
// the same function are two independent sets of parameter, body-local and result registers. They
// are named for the call -- `over_cap#2.cap` -- so a register dump says which call a value belongs
// to.

// funcDef is one definition. The body is a closure rather than a statement list because the builder
// is the only API the parser lowers onto, and a closure is what If and While already take.
type funcDef struct {
	name   string // as written, for diagnostics
	params []string
	body   func(*Script)
	active bool // currently being expanded, which is what makes a cycle findable
}

// inlineFrame is the state of one expansion in progress.
type inlineFrame struct {
	prefix   string // "over_cap#1.", prepended to every register this expansion allocates
	retReg   int32
	retJumps []int   // the `return` jumps waiting for the epilogue's address
	bodyRegs []int32 // registers the body bound, for the per-instance reset inside a loop

	// fd is the definition being expanded. A `return` compares its call's name against it, and
	// against nothing else: that comparison is the whole of what makes a tail call findable.
	fd *funcDef

	// paramRegs are the parameter registers in declaration order, which is where a tail call puts
	// the next activation's arguments.
	paramRegs []int32

	// entry is the body's first instruction, where a tail call resumes.
	entry int32

	// firstLocal is the first program local allocated while this body was being emitted. Everything
	// from there to the end of the body belongs to this activation -- the body's own locals, the
	// registers of calls nested inside it, a `for`'s counters -- which is what the re-entry block
	// clears. It is a RANGE rather than a list because the list would have to be collected by every
	// construct that allocates a register, and one that forgot would leak a value into the next
	// activation silently.
	firstLocal int32

	// tailJumps are the self-tail-calls' jumps, waiting for the re-entry block's address, and tails
	// counts them so each one's argument temporaries have a name of their own.
	tailJumps []int
	tails     int
}

// maxInlineExpansions bounds how many calls one script may expand.
//
// A bound is needed because nesting is MULTIPLICATIVE: a function that calls another twice, which
// calls a third twice, expands the third four times, and a chain of n such functions expands 2^n
// copies of the innermost one. Nothing in the source looks large when that happens, so the failure
// to avoid is a script of twenty readable lines that compiles for a minute and produces a program
// too big to run.
//
// 256 is chosen against what a script IS rather than against a memory budget: scripts here are
// policy expressions a person reads in one screen, and no such script has 256 calls in it. A script
// that does is either generated or recursive-by-accident, and both are better stopped than
// compiled. The expanded program stays in the low tens of thousands of instructions at the bound,
// which the register lowering and the step budget both handle comfortably.
const maxInlineExpansions = 256

// Func defines a function callable from anywhere in the script, including from statements written
// BEFORE this call: a definition is recorded as soon as Func runs, while statements are not
// compiled until Compile.
//
// The body is a block, exactly as If's and While's are, and it ends its value with Return.
func (s *Script) Func(name string, params []string, body func(*Script)) *Script {
	return s.defineFunc(name, params, body, true)
}

// defineFunc is Func, with the refusal of a name the process registry already holds made optional.
//
// A script keeps it (refuseRegistered true): a script function that shadowed a registered one would
// make a call inside the script mean something other than the same call outside it.
//
// A MODULE's definitions do not, and the reason is what a module is for. Its functions become
// registrations, so the one that is being registered IS the name in the registry, and its siblings
// would stop compiling the moment it got there -- registering one function of a module would break
// the rest. Whether a module's name may be taken is a question about the registry, answered where
// registration happens, by classad.RegisterFunction returning ErrFunctionExists. A built-in is still
// refused, by the module parser, because that answer never changes while the process runs.
func (s *Script) defineFunc(name string, params []string, body func(*Script), refuseRegistered bool) *Script {
	if s.err != nil {
		return s
	}
	folded := strings.ToLower(name)
	switch {
	case !isBareIdent(name):
		s.err = fmt.Errorf("vm: script: %q is not a name a call can spell", name)
		return s
	case refuseRegistered && classad.KnownFunctionName(name):
		// The same refusal classad.RegisterFunction makes, for the same reason: a script function
		// that shadowed one would make `strcat(a, b)` mean something different inside this script
		// than in every expression outside it, and a script's expressions are ClassAd expressions.
		s.err = fmt.Errorf("vm: script: %q is already a built-in or a registered function and "+
			"a script function may not shadow one", name)
		return s
	case s.funcs[folded] != nil:
		s.err = fmt.Errorf("vm: script: %q is defined twice", name)
		return s
	case body == nil:
		s.err = fmt.Errorf("vm: script: %q has no body", name)
		return s
	}
	seen := make(map[string]bool, len(params))
	for _, p := range params {
		if !isBareIdent(p) {
			s.err = fmt.Errorf("vm: script: %q has a parameter that is not a name: %q", name, p)
			return s
		}
		f := foldLocal(p)
		if seen[f] {
			s.err = fmt.Errorf("vm: script: %q has two parameters named %q", name, p)
			return s
		}
		seen[f] = true
	}
	s.funcs[folded] = &funcDef{name: name, params: append([]string(nil), params...), body: body}
	return s
}

// Return ends the enclosing function body with expr's value. A nil expr returns UNDEFINED, which is
// also what falling off the end of a body does.
//
// A return of a call to the function being expanded is a TAIL CALL: the next activation's arguments
// are computed, the parameters are overwritten and control goes back to the body's first
// instruction. It is the only recursive call a script may make, and the only one that needs neither
// a second expansion nor a frame. See tailSelfCall for exactly which returns qualify.
func (s *Script) Return(expr ast.Expr) *Script {
	s.stage(func() { s.returnNow(expr) })
	return s
}

func (s *Script) returnNow(expr ast.Expr) {
	if s.err != nil {
		return
	}
	if s.fn == nil {
		s.err = errors.New("vm: script: 'return' outside a function body")
		return
	}
	// Recognised before anything is emitted, because a tail call emits something else entirely: it
	// leaves no value for the store below and jumps backwards rather than forwards.
	if call := s.tailSelfCall(expr); call != nil {
		s.tailCall(call)
		return
	}
	// A BARE RETURN STORES NOTHING ON PURPOSE. The result register is read once, at the epilogue,
	// and the lowering initializes a local to UNDEFINED exactly when no store reaches a read of it
	// (see localsNeedingInit). A path that returns no value is such a path, so leaving the store
	// out is not an omission -- it is how the value gets to be UNDEFINED, with no instruction and
	// no special case.
	if expr != nil {
		s.emitExpr(expr)
		s.c.ins(OpStoreLocal, s.fn.retReg)
	}
	s.fn.retJumps = append(s.fn.retJumps, s.c.ins(OpJmpVoid, -1))
}

// isScriptFunc reports whether name is a function this script defines.
func (s *Script) isScriptFunc(name string) bool {
	return s.funcs[strings.ToLower(name)] != nil
}

// expandCall is the compiler's hook back into the script: it expands v in place and reports whether
// the name was one of the script's functions.
//
// Once it returns true the call has been compiled, which means it has pushed EXACTLY ONE value --
// including on every error path, where it pushes ERROR. The call sits in the middle of an enclosing
// expression's operands, and a refusal that pushed nothing would leave that expression's operand
// stack short, turning a diagnosable compile error into a panic in the register lowering.
func (s *Script) expandCall(v *ast.FunctionCall) bool {
	fd := s.funcs[strings.ToLower(v.Name)]
	if fd == nil {
		return false
	}
	if err := s.inlineCheck(fd, v); err != nil {
		s.fail(err)
		s.c.ins(OpPushError, 0)
		return true
	}
	s.inline(fd, v.Args)
	return true
}

// inlineCheck is everything that can refuse a call before any of it is emitted.
func (s *Script) inlineCheck(fd *funcDef, v *ast.FunctionCall) error {
	if len(v.Args) != len(fd.params) {
		return fmt.Errorf("vm: script: %s takes %d argument(s), but this call passes %d",
			fd.name, len(fd.params), len(v.Args))
	}
	if fd.active {
		return fmt.Errorf("vm: script: %s is recursive (%s); ClassAd Script expands a call where it "+
			"is written, and an expansion that contains itself has no end, so recursion cannot be "+
			"compiled at any depth", fd.name, strings.Join(append(append([]string(nil), s.inlining...), fd.name), " -> "))
	}
	if s.expansions >= maxInlineExpansions {
		return fmt.Errorf("vm: script: more than %d function calls expanded; every call is expanded "+
			"where it is written, so calls nested inside calls multiply", maxInlineExpansions)
	}
	return nil
}

// inline emits one expansion and returns the parameter locals it allocated, in declaration order.
//
// args are the caller's argument expressions, one per parameter. They are NIL when the expansion is
// not a call written in a script but a whole function compiled on its own, whose parameters are
// filled from values before it runs rather than by code inside it; see scriptmodule.go. The locals
// are returned for that caller, which needs to know where to put those values, and ignored by
// every other.
func (s *Script) inline(fd *funcDef, args []ast.Expr) []int32 {
	s.expansions++
	frame := &inlineFrame{
		prefix: fmt.Sprintf("%s#%d.", fd.name, s.expansions),
		retReg: s.newFuncReg(fmt.Sprintf("%s#%d.return", fd.name, s.expansions)),
		fd:     fd,
	}

	// The arguments, in the CALLER's scope: left to right, each evaluated once into its parameter's
	// register. Emitted before the scope is swapped, which is the whole of call-by-value here.
	params := make(map[string]int32, len(fd.params))
	paramLocals := make([]int32, len(fd.params))
	for i, p := range fd.params {
		reg := s.newFuncReg(frame.prefix + p)
		if args != nil {
			s.c.emit(args[i])
			s.c.ins(OpStoreLocal, reg)
		}
		params[foldLocal(p)] = reg
		paramLocals[i] = reg
	}
	frame.paramRegs = paramLocals

	// An expansion inside a loop runs more than once, and its registers are the program's, written
	// once and never cleared. The result register of a call that returns nothing this time round
	// would then hold what it returned last time, which is a value the function did not produce.
	// The reset runs before the body and is written after it, because the body's own locals are not
	// known until it has been emitted -- hence the jump out and back.
	reset := s.loopDepth > 0
	var jToReset int
	if reset {
		jToReset = s.c.ins(OpJmpVoid, -1)
	}
	// The body's first instruction, and the first register it will allocate. A tail call needs both:
	// one to go back to, the other to say what belongs to the activation it is leaving.
	frame.entry = int32(len(s.c.p.code))
	frame.firstLocal = int32(len(s.c.p.locals))

	// The loop stack is cleared for the body, not just the name map: a `break` in the body is bound
	// by a loop in the BODY, never by the loop this call happens to sit inside. See Script.Break.
	outerByName, outerFrame, outerLoops := s.byName, s.fn, s.loops
	s.byName, s.fn, s.loops = params, frame, nil
	s.c.locals = s.byName
	fd.active = true
	s.inlining = append(s.inlining, fd.name)

	fd.body(s)

	s.inlining = s.inlining[:len(s.inlining)-1]
	fd.active = false
	s.byName, s.fn, s.loops = outerByName, outerFrame, outerLoops
	s.c.locals = s.byName

	// Written after the body for the same reason the per-call reset below is: what it clears is not
	// known until the body has been emitted.
	if len(frame.tailJumps) > 0 {
		s.emitReentry(frame)
	}

	jumps := frame.retJumps
	if reset {
		jEnd := s.c.ins(OpJmpVoid, -1)
		s.c.patch(jToReset, len(s.c.p.code))
		for _, reg := range append([]int32{frame.retReg}, frame.bodyRegs...) {
			s.c.ins(OpPushUndef, 0)
			s.c.ins(OpStoreLocal, reg)
		}
		s.c.ins(OpJmpVoid, frame.entry)
		jumps = append(jumps, jEnd)
	} else {
		// A body whose last statement is a return ends in a jump to the instruction that follows
		// it. Dropping it keeps a straight-line function free of control flow, which is what lets a
		// script that calls one still run over a batch: the vector executor serves a jump only as
		// part of an If it recognises.
		for len(jumps) > 0 && jumps[len(jumps)-1] == len(s.c.p.code)-1 {
			s.c.p.code = s.c.p.code[:len(s.c.p.code)-1]
			jumps = jumps[:len(jumps)-1]
		}
	}
	epilogue := len(s.c.p.code)
	for _, j := range jumps {
		s.c.patch(j, epilogue)
	}
	s.c.ins(OpLoadLocal, frame.retReg)
	return paramLocals
}

// newFuncReg allocates a program local for an expansion. It is deliberately NOT entered in any name
// map: a parameter is reachable only through the body's scope and a result register only through
// the epilogue this function emits.
func (s *Script) newFuncReg(display string) int32 {
	s.c.p.locals = append(s.c.p.locals, display)
	return int32(len(s.c.p.locals) - 1)
}

// tailSelfCall reports the call a `return` returns when that call is the function currently being
// expanded and its value is the function's value -- the one recursive position that needs no
// expansion. It returns nil for every other `return`.
//
// THE TEST IS DELIBERATELY NARROW: the WHOLE of the returned expression is a call, and the call
// resolves to the innermost definition being expanded. Resolution is the same map lookup an ordinary
// call does, so a tail call folds case exactly as every other call does.
//
// `return f(x) + 1`, `return g(f(x))`, `return f(x) ? a : b` and a call in a binding or a condition
// are not among them, and the line between is not a matter of degree: each of those needs the call's
// value AS A VALUE in the middle of something else, and the only way to have one is the expansion
// that has no end. A parenthesised call, `return (f(x))`, is not accepted either. It would be sound
// to accept, and it is left out so that the rule stays one a reader checks by looking at the line
// rather than at a parse tree -- the cost of being wrong here is a recursion that quietly computes
// something other than what it says, and the cost of being too strict is a pair of brackets.
//
// AN ARITY MISMATCH IS NOT A TAIL CALL. Letting it fall through to the ordinary expansion is what
// gets it the arity refusal, which names both counts, rather than one about recursion.
func (s *Script) tailSelfCall(expr ast.Expr) *ast.FunctionCall {
	if s.fn == nil || s.fn.fd == nil {
		return nil
	}
	call, ok := expr.(*ast.FunctionCall)
	if !ok || s.funcs[strings.ToLower(call.Name)] != s.fn.fd {
		return nil
	}
	if len(call.Args) != len(s.fn.fd.params) {
		return nil
	}
	return call
}

// tailCall compiles `return f(...)` where f is the function being expanded: the next activation's
// arguments are computed, the parameters are overwritten with them, and control goes back to the
// body's first instruction by way of the re-entry block.
//
// IT PUSHES NOTHING, which is what makes it a tail call rather than a call. An ordinary expansion
// leaves one value for the `return` to store; here the store and the epilogue are both unreachable
// from this point, so there is nothing to leave. The statement is still stack-neutral -- each
// argument is pushed and immediately stored -- so the value stack at the jump is the one the entry
// was reached with, which is the same invariant the back edge of a `while` rests on.
//
// THE ARGUMENTS GO THROUGH TEMPORARIES, and that is the one part of this that is not obvious. An
// argument expression usually reads a parameter, and the parameters are the registers being written.
// Writing them one at a time would let an earlier write be read by a later argument: `return f(b, a)`
// would store the new first argument into b's register and then read THAT register for the second,
// so a function that swaps its arguments would duplicate one instead. Evaluating every argument
// before any parameter is written is the only order that is right for every argument list, and it is
// not worth a special case for the lists where it does not matter -- the lowering's own liveness
// sees straight through a copy that nothing else reads.
func (s *Script) tailCall(call *ast.FunctionCall) {
	frame := s.fn
	frame.tails++
	tag := fmt.Sprintf("%stail%d.", frame.prefix, frame.tails)

	tmps := make([]int32, len(call.Args))
	for i, arg := range call.Args {
		tmps[i] = s.newFuncReg(tag + frame.fd.params[i])
		s.emitExpr(arg)
		s.c.ins(OpStoreLocal, tmps[i])
	}
	for i, tmp := range tmps {
		s.c.ins(OpLoadLocal, tmp)
		s.c.ins(OpStoreLocal, frame.paramRegs[i])
	}
	s.c.p.hasStatements = true
	s.c.p.hasTailCall = true
	frame.tailJumps = append(frame.tailJumps, s.c.ins(OpJmpVoid, -1))
}

// emitReentry lays out the block a self-tail-call jumps to: the activation's registers are cleared
// and control returns to the body's first instruction.
//
// THE PARAMETERS ARE NOT CLEARED. The tail call has just written the next activation's arguments
// into them, and they are the only thing that crosses the back edge -- which is the whole of what a
// frame would otherwise have carried.
//
// EVERYTHING ELSE IS, because the next trip is a fresh activation of the body rather than another
// turn of a loop the author wrote. A local the body binds on one path and reads on another has to
// read UNDEFINED again, as it would in a second call; so does the result register of a call NESTED
// in the body, which a bare `return` inside that call leaves untouched and which nothing the author
// can write could put right. The range covers both, and the argument temporaries besides -- cleared
// after they have been copied into the parameters, which costs an instruction and keeps the rule
// "everything this activation allocated" true with no exceptions to maintain.
//
// THE CLEAR IS ALSO WHAT CHARGES THE BACK EDGE. OpPushUndef is a value operation, so a trip costs at
// least one charged step even for a function that takes no arguments and computes nothing: that is
// what makes `def f(): return f()` end at the step budget instead of running forever, and it is the
// same guarantee a `while` gets from having to evaluate a condition.
func (s *Script) emitReentry(frame *inlineFrame) {
	// The body falls through to whatever comes after it, so the block needs jumping over rather
	// than appending.
	jOver := s.c.ins(OpJmpVoid, -1)
	block := len(s.c.p.code)
	end := int32(len(s.c.p.locals))
	s.c.ins(OpPushUndef, 0)
	s.c.ins(OpStoreLocal, frame.retReg)
	for reg := frame.firstLocal; reg < end; reg++ {
		s.c.ins(OpPushUndef, 0)
		s.c.ins(OpStoreLocal, reg)
	}
	s.c.ins(OpJmpVoid, frame.entry)
	s.c.patch(jOver, len(s.c.p.code))
	for _, j := range frame.tailJumps {
		s.c.patch(j, block)
	}
}

// fail records the first error. Later ones are almost always consequences of it.
func (s *Script) fail(err error) {
	if s.err == nil {
		s.err = err
	}
}

// ScriptCallCaptureError reports a call to a script function from inside a construct the compiler
// evaluates against the ad, where the function does not exist. The sibling of LocalCaptureError,
// and refused for the same reason: the tree-walking evaluator resolves the name against the
// built-ins and the UDF registry, finds nothing, and returns ERROR for a call the script can see
// perfectly well.
type ScriptCallCaptureError struct {
	Func string // the function the subtree calls, as the script spelled it
	Expr string // the delegated subtree, unparsed
}

func (e *ScriptCallCaptureError) Error() string { return "vm: script: " + e.detail() }

func (e *ScriptCallCaptureError) detail() string {
	return fmt.Sprintf("%s calls %s, but this construct is evaluated against the ad, where a script "+
		"function does not exist; call it from a statement and use the local instead", e.Expr, e.Func)
}

// callsScriptFunc reports the first script function a subtree calls, if any.
func callsScriptFunc(expr ast.Expr, known func(string) bool) string {
	var found string
	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		if e == nil || found != "" {
			return
		}
		switch v := e.(type) {
		case *ast.ParenExpr:
			walk(v.Inner)
		case *ast.UnaryOp:
			walk(v.Expr)
		case *ast.BinaryOp:
			walk(v.Left)
			walk(v.Right)
		case *ast.ElvisExpr:
			walk(v.Left)
			walk(v.Right)
		case *ast.ConditionalExpr:
			walk(v.Condition)
			walk(v.TrueExpr)
			walk(v.FalseExpr)
		case *ast.ListLiteral:
			for _, el := range v.Elements {
				walk(el)
			}
		case *ast.FunctionCall:
			if known(v.Name) {
				found = v.Name
				return
			}
			for _, a := range v.Args {
				walk(a)
			}
		case *ast.SelectExpr:
			walk(v.Record)
		case *ast.SubscriptExpr:
			walk(v.Container)
			walk(v.Index)
		case *ast.RecordLiteral:
			if v.ClassAd != nil {
				for _, attr := range v.ClassAd.Attributes {
					walk(attr.Value)
				}
			}
		}
	}
	walk(expr)
	return found
}
