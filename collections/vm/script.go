package vm

import (
	"errors"
	"fmt"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// A SCRIPT: a sequence of local bindings followed by a result expression.
//
// This is the smallest thing that is a script rather than an expression, and it exists to settle the
// binding model before any syntax or control flow was built on it. It predates the parser, which is
// why it is a builder rather than a parse tree: every question it was written to settle is about
// what a name means and none of them are about how it is spelled. ParseScript lowers source onto
// this same API, so the two cannot disagree about a binding.
//
// WHAT A LOCAL IS. `x = Cpus + 1` binds a name that later statements read, and the scope never sees
// it. A bare reference to a bound name is that local; a reference to anything else is an attribute,
// resolved exactly as it would be in a bare expression. A local therefore neither shadows nor is
// shadowed by an attribute of the same name -- `x = x + 1` reads the ATTRIBUTE x and binds the LOCAL
// x, and a later `x` is the local. That is the Python rule, and it is the reason the decision has to
// be made by the compiler: only the compiler knows what is bound at a given point.
//
// WHERE A LOCAL CANNOT BE READ. A construct the compiler delegates to the tree-walking evaluator --
// a conditional, a list or ad literal, a subscript, a select, a lazy built-in -- is evaluated against
// the ad, which has never heard of a local. Reading a local from inside one is refused at compile
// time rather than silently resolved to the attribute of the same name; see compiler.delegate and
// LocalCaptureError for why the refusal is the right answer and not a stopgap.
//
// WHY ONE PROGRAM. Every statement's expression is compiled into a single Program, through the same
// compile.go emitter an expression uses, and the whole thing is lowered to registers once. Nothing
// is re-lowered: a local that three statements read is computed once and read from a register three
// times, which is the property the reduction law below actually tests.
//
// FUNCTIONS. Func and Return define a function the script can call, and a call is EXPANDED WHERE IT
// IS WRITTEN -- there is no call instruction and no frame, so the program stays one program. A
// body's bare name is a parameter, a name the body binds, or an attribute; the caller's locals are
// not visible to it, which is the one thing a function changes about the binding model above. The
// rest of what a function means, and why recursion is refused rather than bounded, is in
// scriptfunc.go.
//
// WHEN A STATEMENT IS COMPILED. Statements are recorded as they are written and compiled by
// Compile, which is what lets a call be written above the Func that defines it. A caller that wants
// a refusal attributed to the statement that caused it flushes after each one; the parser does.
//
// A LOCAL IS A PLACE. Each one has a single register for the whole script, written by OpStoreLocal
// and read by OpLoadLocal, and every statement leaves the stack as it found it. An earlier model
// gave each binding whatever register its expression happened to land in; that works in a straight
// line and cannot survive a branch, where a binding made on one path has to be the same place a read
// after the merge finds.
//
// THE ORACLE. A script of pure local bindings and a result expression has an obvious reference
// implementation -- substitute the bindings into the result and evaluate THAT with classad.Eval:
//
//	a = <X>
//	b = <Y>
//	result = a + b      must equal      Eval( (<X>) + (<Y>) )
//
// The substituted form is not how the script runs, which is what makes the law worth testing: it
// pins the binding model, the case folding, and the fact that a local is computed once, against an
// engine that has none of those concepts. See FuzzScriptReduction.

// ErrNoResult is returned by Compile when the script has no result expression.
var ErrNoResult = errors.New("vm: script has no result expression")

// Script accumulates statements. The zero value is not usable; call NewScript.
type Script struct {
	c      *compiler
	byName map[string]int32
	hasRes bool
	err    error

	// staged holds the statements not yet emitted, and emitting says whether they are being
	// emitted right now. A statement is recorded rather than compiled on the spot so that a call
	// to a function DEFINED LATER in the script still resolves: Func registers a definition the
	// moment it is called, and no statement has been compiled yet when it does. See flush.
	staged   []func()
	emitting bool

	// funcs holds the function definitions, by folded name, and inlining is the chain of
	// definitions currently being expanded -- the one that makes a cycle findable and nameable.
	funcs    map[string]*funcDef
	inlining []string

	// fn is the state of the innermost inline expansion, nil outside one.
	fn *inlineFrame

	// expansions counts the inline expansions in this script, against maxInlineExpansions.
	expansions int

	// loopDepth is how many While bodies enclose the statement being emitted. An inline expansion
	// inside one can run more than once, which is the only case where a call has to reset its own
	// registers rather than rely on their program-start initialization. See expandCall.
	loopDepth int

	// loops is the stack of While bodies a Break or a Continue may name, innermost last.
	//
	// It is NOT loopDepth with the targets attached, and the difference is the whole of the
	// function-body rule. A function expansion starts with an empty stack and gets the caller's
	// back when it ends (see inline), so a `break` written in a body can only name a loop written
	// in that same body. loopDepth keeps counting across the expansion, because the question it
	// answers -- can this expansion run more than once -- is about the caller.
	loops []*loopFrame
}

// loopFrame is one While whose body is being emitted, and the two addresses a jump out of it or
// round it needs.
//
// The header is known as soon as the loop starts, so a Continue is an ordinary backward jump with
// its target already in hand. The end is not known until the body and the condition's fault site
// have been laid out, so each Break records its jump here and whileNow patches them all at once --
// the same forward-patch If already does for the tail of its arms.
type loopFrame struct {
	header int32
	breaks []int
}

// NewScript returns an empty script.
func NewScript() *Script {
	s := &Script{
		c:      &compiler{p: &Program{}},
		byName: map[string]int32{},
		funcs:  map[string]*funcDef{},
	}
	s.c.inlineCall = s.expandCall
	s.c.isScriptFunc = s.isScriptFunc
	return s
}

// stage records a statement, or runs it if the script is already emitting -- which it is whenever a
// statement comes from inside an If, a While or a function body, since those run their blocks as
// part of being emitted.
func (s *Script) stage(f func()) {
	if s.emitting {
		f()
		return
	}
	s.staged = append(s.staged, f)
}

// flush emits every statement staged so far, in the order they were written.
//
// Calling it is idempotent and it is safe to call at any point: a caller that wants errors
// attributed to the statement that caused them -- the parser does -- flushes after each one.
func (s *Script) flush() {
	if s.emitting {
		return
	}
	s.emitting = true
	for i := 0; i < len(s.staged); i++ {
		s.staged[i]()
	}
	s.staged = nil
	s.emitting = false
}

// foldLocal normalizes a local's name the way ClassAd normalizes an attribute's, so `X` and `x` are
// one local and a reference in either spelling finds it.
func foldLocal(name string) string {
	return ast.NewAttributeReference(name, ast.NoScope).NormalizedName()
}

// Let binds name to expr's value. A name already bound is REBOUND: later references see the new
// value and the old one stays live only for the statements that already read it, which is what
// `x = x + 1` has to mean.
func (s *Script) Let(name string, expr ast.Expr) *Script {
	s.stage(func() { s.letNow(name, expr) })
	return s
}

func (s *Script) letNow(name string, expr ast.Expr) {
	if s.err != nil {
		return
	}
	if s.hasRes {
		s.err = errors.New("vm: Let after Result")
		return
	}
	folded := foldLocal(name)
	if folded == "" {
		s.err = errors.New("vm: empty local name")
		return
	}
	// Emitted BEFORE the name is bound here, so the right-hand side sees whatever the name meant
	// previously: the attribute on a first binding, the previous local on a rebinding. That is the
	// whole of the shadowing rule, and it is why `x = x + 1` is well defined either way.
	s.emitExpr(expr)
	idx, existing := s.byName[folded]
	if !existing {
		display := name
		if s.fn != nil {
			// Inside a function body the register belongs to THIS expansion, not to the script, so
			// its name says which instance it came from. Two calls to the same function have two
			// registers named alike otherwise, and a register dump is then unreadable.
			display = s.fn.prefix + name
		}
		idx = int32(len(s.c.p.locals))
		s.c.p.locals = append(s.c.p.locals, display)
		s.byName[folded] = idx
		if s.fn != nil {
			s.fn.bodyRegs = append(s.fn.bodyRegs, idx)
		}
	}
	s.c.ins(OpStoreLocal, idx)
	s.c.locals = s.byName
}

// If runs then when cond is true and els when it is false. Either block may be nil.
//
// A condition that is NEITHER true nor false -- UNDEFINED, or a type that cannot be one, such as a
// string or a list -- is a FAULT: the script ends with ERROR. It is not a branch not taken. Treating
// "I could not tell" as "no" is how a misspelled attribute silently turns a script into one that
// quietly does nothing, and a script runs once, so there is no second record to notice.
//
// A local bound inside a branch is the same local as everywhere else -- one register -- so a read
// after the If sees whatever path ran. Where no path bound it, it reads UNDEFINED, because the
// lowering can see that no store reaches that read.
func (s *Script) If(cond ast.Expr, then, els func(*Script)) *Script {
	s.stage(func() { s.ifNow(cond, then, els) })
	return s
}

func (s *Script) ifNow(cond ast.Expr, then, els func(*Script)) {
	if s.err != nil {
		return
	}
	if s.hasRes {
		s.err = errors.New("vm: If after Result")
		return
	}
	s.c.p.hasStatements = true
	s.emitExpr(cond)
	s.c.p.conds = append(s.c.p.conds, condTargets{})
	k := int32(len(s.c.p.conds) - 1)
	s.c.ins(OpBranchVoid, k)

	if then != nil {
		then(s)
	}
	jThen := s.c.ins(OpJmpVoid, -1)

	onFalse := len(s.c.p.code)
	if els != nil {
		els(s)
	}
	jElse := s.c.ins(OpJmpVoid, -1)

	onFault := len(s.c.p.code)
	s.c.ins(OpFault, s.c.p.addFaultSite(FaultCondition, "if"))

	end := len(s.c.p.code)
	// UNDEFINED and ERROR share the fault, because the script's answer is the same either way: it
	// could not tell which branch to take.
	s.c.p.conds[k] = condTargets{
		onFalse: int32(onFalse), onUndef: int32(onFault), onError: int32(onFault), end: int32(end),
	}
	s.c.patch(jThen, end)
	s.c.patch(jElse, end)
}

// While runs body for as long as cond is true.
//
// The back edge is the only thing here that the lowering had not seen before, and it works because a
// statement leaves the stack as it found it: the value stack at the loop header is identical on
// every iteration, so there is nothing to reconcile between them. Locals are places, so a value
// carried across iterations is just a register written each time round -- no phi, and no loop-carried
// analysis to get wrong.
//
// A condition that is neither true nor false faults, exactly as in If.
//
// TERMINATION is not the loop's problem to solve but the interpreter's: `while true: x = x` has no
// answer, and nothing the compiler can check would tell it apart from a loop that happens to run a
// long time. The step budget in RunRegistersLimited is what bounds it.
func (s *Script) While(cond ast.Expr, body func(*Script)) *Script {
	s.stage(func() { s.whileNow(cond, body) })
	return s
}

func (s *Script) whileNow(cond ast.Expr, body func(*Script)) {
	if s.err != nil {
		return
	}
	if s.hasRes {
		s.err = errors.New("vm: While after Result")
		return
	}
	header := len(s.c.p.code)
	s.c.p.hasStatements = true
	s.emitExpr(cond)
	s.c.p.conds = append(s.c.p.conds, condTargets{})
	k := int32(len(s.c.p.conds) - 1)
	s.c.ins(OpBranchVoid, k)

	frame := &loopFrame{header: int32(header)}
	if body != nil {
		s.loopDepth++
		s.loops = append(s.loops, frame)
		body(s)
		s.loops = s.loops[:len(s.loops)-1]
		s.loopDepth--
	}
	// The back edge. Every other jump this compiler emits goes forward; this one does not, and the
	// lowering needs no special case for it because targets are resolved after the whole program is
	// laid out.
	s.c.ins(OpJmpVoid, int32(header))

	onFault := len(s.c.p.code)
	s.c.ins(OpFault, s.c.p.addFaultSite(FaultCondition, "while"))

	end := len(s.c.p.code)
	s.c.p.conds[k] = condTargets{
		onFalse: int32(end), onUndef: int32(onFault), onError: int32(onFault), end: int32(end),
		loop: true, header: int32(header),
	}
	// A Break lands exactly where a false condition does. It skips the fault site on the way, which
	// is right: the loop is not ending because the condition could not be read.
	for _, j := range frame.breaks {
		s.c.patch(j, end)
	}
}

// Break leaves the innermost enclosing While and continues after it.
//
// It is a jump to the address a false condition goes to, so a loop that breaks on its first
// iteration terminates for the same reason one whose condition is false terminates: there is no
// back edge on that path.
//
// A BREAK IS BOUND BY A LOOP IN THE SAME FUNCTION BODY. A call is expanded where it is written, so
// a function called from inside a loop sits, instruction for instruction, inside that loop -- and a
// Break in its body would leave the CALLER's loop. That would make a function mean one thing at one
// call site and something else at another, and the same body legal in one place and nonsense in
// another. So a body starts with no enclosing loop, whatever the call site is inside, and a Break
// with nothing to name is refused.
//
// Outside a loop altogether it is refused for the same reason: there is nothing to leave.
func (s *Script) Break() *Script {
	s.stage(func() { s.loopJump(true) })
	return s
}

// Continue starts the innermost enclosing While's next iteration.
//
// It jumps to the loop's HEADER, which is the condition, so the next iteration re-tests it exactly
// as falling off the end of the body does. It is bound by the same rule as Break.
func (s *Script) Continue() *Script {
	s.stage(func() { s.loopJump(false) })
	return s
}

// loopJump emits a Break or a Continue. The two differ only in where they go and in what they are
// called, which is why they share this.
func (s *Script) loopJump(isBreak bool) {
	word := "continue"
	if isBreak {
		word = "break"
	}
	if s.err != nil {
		return
	}
	if s.hasRes {
		s.err = fmt.Errorf("vm: '%s' after Result", word)
		return
	}
	if len(s.loops) == 0 {
		s.err = errors.New("vm: script: " + s.noLoopDetail(word))
		return
	}
	s.c.p.hasLoopJump = true
	frame := s.loops[len(s.loops)-1]
	if isBreak {
		frame.breaks = append(frame.breaks, s.c.ins(OpJmpVoid, -1))
		return
	}
	s.c.ins(OpJmpVoid, frame.header)
}

// noLoopDetail says why a Break or a Continue had nothing to name, which is a different mistake
// inside a function body than outside one and is worth two messages.
func (s *Script) noLoopDetail(word string) string {
	if s.fn != nil {
		return fmt.Sprintf("'%s' in a function body with no loop in that body: a call is expanded "+
			"where it is written, so a '%s' here would leave a loop at the call site and the "+
			"function would mean something different everywhere it is called; put the loop in the "+
			"body, or return a value and let the caller break", word, word)
	}
	return fmt.Sprintf("'%s' outside a loop", word)
}

// Result sets the script's value. It must be called last and exactly once.
func (s *Script) Result(expr ast.Expr) *Script {
	s.stage(func() { s.resultNow(expr) })
	return s
}

func (s *Script) resultNow(expr ast.Expr) {
	if s.err != nil {
		return
	}
	if s.hasRes {
		s.err = errors.New("vm: Result called twice")
		return
	}
	s.emitExpr(expr)
	s.hasRes = true
}

// emitExpr compiles one statement's expression into the shared program.
//
// The depth guard is per statement and matches CompileProgram's: an expression too deep to lower
// natively is delegated whole, so the tree-walker's own recursion limit governs it identically. A
// script does not change that, because a statement's expression is still just an expression.
func (s *Script) emitExpr(expr ast.Expr) {
	if astHeight(expr) >= maxNativeDepth {
		s.c.delegate(expr)
		return
	}
	s.c.emit(expr)
}

// ScriptProgram is a compiled script.
type ScriptProgram struct {
	rp *RegProgram
	p  *Program
}

// Compile lowers the script to a single register program.
func (s *Script) Compile() (*ScriptProgram, error) {
	return s.compile(nil)
}

// compile is Compile, with the locals whose registers must be left alone named by the caller.
//
// preset names locals that are filled from OUTSIDE the program, before it runs: the parameters of a
// function compiled on its own (see scriptmodule.go). Nothing in the program stores to one, so the
// dominance analysis would correctly conclude that its reads are not dominated by a store and emit
// an UNDEFINED initializer -- which would overwrite the value the caller just put there. Suppressing
// the initializer for exactly those locals is what makes the register a parameter rather than a
// local that happens never to be bound.
func (s *Script) compile(preset []int32) (*ScriptProgram, error) {
	s.flush()
	if s.err != nil {
		return nil, s.err
	}
	if s.c.capture != nil {
		return nil, s.c.capture
	}
	if !s.hasRes {
		return nil, ErrNoResult
	}
	s.c.p.readAttrs = s.scriptReads()
	s.c.p.vops = vopPrefix(s.c.p.code)
	s.c.p.refSlots, s.c.p.nColSlots = colSlots(s.c.p.refs)
	needInit := localsNeedingInit(s.c.p)
	for _, l := range preset {
		needInit[l] = false
	}
	return &ScriptProgram{rp: lowerWithInit(s.c.p, needInit), p: s.c.p}, nil
}

// scriptReads is the set of attribute names the compiled script may read, in first-seen order.
//
// It is read off the COMPILED PROGRAM rather than off the statements' syntax, because the compiler
// is the only thing that knows which bare names turned out to be attributes. Two sources, which
// between them are every way a script reaches the ad:
//
//   - p.refs, the unscoped references emitted as OpLoadRef. A bare name that was a local is not
//     among them: it compiled to OpLoadLocal instead.
//   - the unscoped names inside p.nodes, the subtrees handed to the tree-walking evaluator, which
//     resolves every one of them against the ad. A delegated subtree that reads a local is refused
//     before this runs, so none of these is a local either.
//
// COLLECTING THE SYNTAX AND SUBTRACTING THE LOCALS IS A DIFFERENT ANSWER, and wrong in both
// directions. `x = x + 1` reads the ATTRIBUTE x before it binds the local, so subtracting x drops a
// read the script really makes and leaves a planner unaware of a column the script needs. And a
// function's parameters and body locals belong to an expansion whose name map no longer exists by
// the time this runs, so there is nothing left to subtract them by. Asking the program removes both
// problems, because the program already distinguishes a load from the ad from a load from a
// register.
func (s *Script) scriptReads() []string {
	seen := make(map[string]bool, len(s.c.p.refs))
	var out []string
	for i := range s.c.p.refs {
		r := &s.c.p.refs[i]
		if r.scope != ast.NoScope || seen[r.name] {
			continue
		}
		seen[r.name] = true
		out = append(out, r.name)
	}
	for _, n := range s.c.p.nodes {
		out = collectReads(n, seen, out)
	}
	return out
}

// Run evaluates the script against scope and returns its result, under the default instruction
// budget.
func (sp *ScriptProgram) Run(scope *classad.ClassAd) classad.Value {
	return RunRegisters(sp.rp, scope)
}

// RunLimited is Run with an explicit step budget. ok=false means the script did not finish
// within it, which is a different report from a script that finished and computed ERROR.
func (sp *ScriptProgram) RunLimited(scope *classad.ClassAd, budget int) (classad.Value, bool) {
	return RunRegistersLimited(sp.rp, scope, budget)
}

// RunWithFault evaluates the script and reports the fault that stopped it, if one did.
//
// fault is nil when the script completed, whatever value it produced -- including ERROR, which an
// expression can produce on purpose. That distinction is the reason this method exists: the value
// alone cannot tell "the script decided the answer is ERROR" from "an if had no branch to take".
//
// ok=false means the step budget ran out, which is not a fault: the script did not finish, so
// there is no site to blame. See RunRegistersLimited.
func (sp *ScriptProgram) RunWithFault(scope *classad.ClassAd) (result classad.Value, ok bool, fault *Fault) {
	return RunRegistersFault(sp.rp, scope, DefaultStepBudget)
}

// VecEval runs the script over a batch of n records, returning one result element per record.
//
// ok=false when the script uses something the vector executor does not implement. Control flow is
// not in that set: an If runs both arms under a live mask and a While runs its iterations in
// lockstep, letting the live set shrink as records finish. What declines is a delegated subtree,
// which needs a real per-record scope, and a `break` or `continue`, which leaves a loop from a point
// the lockstep walk has no region for.
//
// A DECLINE IS NOT A WRONG ANSWER. The caller falls back to running the script per record, which is
// the same arrangement the expression executor has had since it was written.
func (sp *ScriptProgram) VecEval(src ColumnSource, n int, scratch *VecScratch) (*Vec, bool) {
	if sp == nil || sp.p == nil {
		return nil, false
	}
	if len(sp.p.nodes) != 0 {
		return nil, false // a delegated subtree needs a real per-record scope
	}
	if scratch == nil {
		scratch = &VecScratch{}
	}
	return execVec(sp.p, scratch.evaluator(), src, n, scratch, int64(n)*DefaultStepBudget)
}

// Program returns the underlying stack-form program.
//
// It is NOT runnable by the stack interpreter: a script's local loads are resolved by the register
// lowering and have no stack-form implementation. It is exposed for the planner's read set and for
// tests that inspect the compiled shape.
func (sp *ScriptProgram) Program() *Program { return sp.p }

// RegProgram returns the compiled register program.
func (sp *ScriptProgram) RegProgram() *RegProgram { return sp.rp }

// Locals returns the local names in binding order.
func (sp *ScriptProgram) Locals() []string { return sp.p.locals }

// String renders the script's registers for diagnostics.
func (sp *ScriptProgram) String() string {
	return fmt.Sprintf("script: %d instructions, %d registers, %d locals",
		sp.rp.Len(), sp.rp.NumRegs(), len(sp.p.locals))
}
