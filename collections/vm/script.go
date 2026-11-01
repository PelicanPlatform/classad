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
// binding model before any syntax or control flow is built on it. There is no parser: a script is
// constructed through this API, because every question at this stage is about what a name means and
// none of them are about how it is spelled.
//
// WHAT A LOCAL IS. `x = Cpus + 1` binds a name that later statements read, and the scope never sees
// it. A bare reference to a bound name is that local; a reference to anything else is an attribute,
// resolved exactly as it would be in a bare expression. A local therefore neither shadows nor is
// shadowed by an attribute of the same name -- `x = x + 1` reads the ATTRIBUTE x and binds the LOCAL
// x, and a later `x` is the local. That is the Python rule, and it is the reason the decision has to
// be made by the compiler: only the compiler knows what is bound at a given point.
//
// WHY ONE PROGRAM. Every statement's expression is compiled into a single Program, through the same
// compile.go emitter an expression uses, and the whole thing is lowered to registers once. Nothing
// is inlined and nothing is re-lowered: a local that three statements read is computed once and read
// from a register three times, which is the property the reduction law below actually tests.
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
}

// NewScript returns an empty script.
func NewScript() *Script {
	return &Script{
		c:      &compiler{p: &Program{}, seen: map[string]bool{}},
		byName: map[string]int32{},
	}
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
	if s.err != nil {
		return s
	}
	if s.hasRes {
		s.err = errors.New("vm: Let after Result")
		return s
	}
	folded := foldLocal(name)
	if folded == "" {
		s.err = errors.New("vm: empty local name")
		return s
	}
	// Emitted BEFORE the name is bound here, so the right-hand side sees whatever the name meant
	// previously: the attribute on a first binding, the previous local on a rebinding. That is the
	// whole of the shadowing rule, and it is why `x = x + 1` is well defined either way.
	s.emitExpr(expr)
	idx, existing := s.byName[folded]
	if !existing {
		idx = int32(len(s.c.p.locals))
		s.c.p.locals = append(s.c.p.locals, name)
		s.byName[folded] = idx
	}
	s.c.ins(OpStoreLocal, idx)
	s.c.locals = s.byName
	return s
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
	if s.err != nil {
		return s
	}
	if s.hasRes {
		s.err = errors.New("vm: If after Result")
		return s
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
	return s
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
	if s.err != nil {
		return s
	}
	if s.hasRes {
		s.err = errors.New("vm: While after Result")
		return s
	}
	header := len(s.c.p.code)
	s.c.p.hasStatements = true
	s.emitExpr(cond)
	s.c.p.conds = append(s.c.p.conds, condTargets{})
	k := int32(len(s.c.p.conds) - 1)
	s.c.ins(OpBranchVoid, k)

	if body != nil {
		body(s)
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
	}
	return s
}

// Result sets the script's value. It must be called last and exactly once.
func (s *Script) Result(expr ast.Expr) *Script {
	if s.err != nil {
		return s
	}
	if s.hasRes {
		s.err = errors.New("vm: Result called twice")
		return s
	}
	s.emitExpr(expr)
	s.hasRes = true
	return s
}

// emitExpr compiles one statement's expression into the shared program.
//
// The depth guard is per statement and matches CompileProgram's: an expression too deep to lower
// natively is delegated whole, so the tree-walker's own recursion limit governs it identically. A
// script does not change that, because a statement's expression is still just an expression.
func (s *Script) emitExpr(expr ast.Expr) {
	// Accumulated across statements, as CompileProgram accumulates it for one expression. It
	// over-approximates by including names that turn out to be locals; Compile removes those.
	s.c.p.readAttrs = collectReads(expr, s.c.seen, s.c.p.readAttrs)
	if astHeight(expr) >= maxNativeDepth {
		s.c.ins(OpEvalNode, s.c.addNode(expr))
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
	if s.err != nil {
		return nil, s.err
	}
	if !s.hasRes {
		return nil, ErrNoResult
	}
	// readAttrs is the union over the statements MINUS the locals: a local is not an attribute, and
	// a planner told otherwise would look for a column that does not exist. collectReads has already
	// accumulated every unscoped name, including the ones that turned out to be locals, because it
	// walks the AST and the AST cannot tell them apart.
	kept := s.c.p.readAttrs[:0]
	for _, name := range s.c.p.readAttrs {
		if _, isLocal := s.byName[foldLocal(name)]; !isLocal {
			kept = append(kept, name)
		}
	}
	s.c.p.readAttrs = kept
	s.c.p.vops = vopPrefix(s.c.p.code)
	return &ScriptProgram{rp: LowerToRegisters(s.c.p), p: s.c.p}, nil
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
// ok=false when the script uses something the vector executor does not implement, which today means
// anything with control flow: a branch picks per record, and until the masked store is built the
// executor has no way to run a statement for some records and not others. A script of bindings and a
// result expression is exactly the part that needs none of that, and it is the part a constraint is
// made of.
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
