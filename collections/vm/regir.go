package vm

import (
	"fmt"

	"github.com/PelicanPlatform/classad/classad"
)

// A REGISTER form of a compiled expression, and the mechanical conversion of the stack form into it.
//
// WHY. The stack discipline works for an expression because an expression's stack depth is its own
// height -- 2, for `Owner == "x" && RequestMemory > 2048`. It stops working the moment there are
// values with lifetimes that are not nested: a local that outlives the statement that defined it, a
// value carried across a loop, a column loaded once and read by three statements. A stack cannot say
// "this value is live across the loop but that one dies here", and which values are simultaneously
// live is the whole question for a vector engine, where a register is a batch and the cost of one is
// measured in hundreds of kilobytes rather than in bytes.
//
// WHAT THIS IS NOT. There is no second expression compiler here, and there must never be. The
// register form does not re-lower an AST: it consumes the Program that compile.go already produced
// and reuses its pools in place -- consts, ops, refs, nodes, funcs, members are read straight out of
// the source Program through the src pointer, not copied. So an expression is still lowered in
// exactly one place (opcode.go's opening comment), and a new operator or built-in reaches this form
// without being taught to it.
//
// WHAT MAKES IT SAFE. The conversion is mechanical: each stack push becomes a definition of a fresh
// virtual register, each pop becomes a read of one. Nothing is reordered, nothing is folded, and
// every value operation is the same call on the same pool entry. That claim is worth exactly as much
// as its test, so RunRegisters is differentially fuzzed against Run over the same Program -- same
// pools, same scope, same expected value. See FuzzRegisterParity.
//
// The two places it is more than a transcription are the jumps, and both are joins:
//
//   - A logical operator (&&, ||) is `short; <right>; combine` in the stack form, where the short
//     instruction may replace the top of the stack and jump past the combine. Both paths have to
//     deliver the operator's value in the SAME register, so the short instruction allocates the
//     destination and the matching combine is told to write it. That is a phi node resolved by
//     construction rather than by analysis -- which is all a register form needs here, and is also
//     exactly the shape an SSA pass would later recognise.
//   - The Elvis operator is `<left>; jmpIfNotUndef; <right>` with no combine to hang a destination
//     on. The jump path's value is the left operand; the fall-through path's value is whatever the
//     fallback left behind. So the fall-through gets an explicit move into the destination, emitted
//     at the join, and the jump lands after that move.

// RegOp identifies a register-form instruction.
//
// Every entry corresponds to exactly one stack Opcode, except RMove (which has no stack counterpart
// because the stack form expresses a join by leaving a value in place) and RNop.
type RegOp uint8

const (
	// RNop does nothing. Emitted only as a jump landing pad at the end of a program.
	RNop RegOp = iota

	// RConst sets Dst to src.consts[Imm]; RTrue/RFalse/RUndef/RError to the corresponding literal.
	RConst
	RTrue
	RFalse
	RUndef
	RError

	// RLoadRef resolves src.refs[Imm] and sets Dst to the result.
	RLoadRef

	// RBinop sets Dst to ApplyBinaryOp(src.ops[Imm], A, B). RUnop sets Dst to
	// ApplyUnaryOp(src.ops[Imm], A).
	RBinop
	RUnop

	// RCombineAnd/RCombineOr set Dst to ApplyBinaryOp("&&"/"||", A, B) for a logical operator whose
	// right operand was evaluated.
	//
	// The operator is in the opcode rather than in a pool index, exactly as it is in the stack form
	// (OpCombineAnd/OpCombineOr carry no argument). That is not only symmetry: a pool index would
	// have to be manufactured here, and appending one to the source Program's operator pool would
	// mutate a structure that program.go documents as read-only after compilation so that one
	// Program can be evaluated concurrently.
	RCombineAnd
	RCombineOr

	// RCallFunc sets Dst to ApplyFunction(src.funcs[Imm], args) where args are the registers listed
	// in RegProgram.args[Args].
	//
	// The arguments are an explicit list rather than a contiguous register window. The window is
	// what a register ALLOCATOR should produce -- callFuncVec's convention depends on it -- but
	// requiring it of the IR before any allocator exists would mean inserting moves here to
	// manufacture adjacency that nothing yet benefits from.
	RCallFunc

	// RMember sets Dst to src.members[Imm].eval(A).
	RMember

	// REvalNode sets Dst to the tree-walking evaluation of src.nodes[Imm].
	REvalNode

	// RMove sets Dst to A. Emitted at an Elvis join, and by nothing else.
	RMove

	// RShortAnd/RShortOr are the short-circuit half of a logical operator: if ShortCircuit settles
	// the operator from A alone, they set Dst to that result and jump to T; otherwise they fall
	// through to the right operand's code, which ends in the matching combine writing the same Dst.
	RShortAnd
	RShortOr

	// RJmpIfDefined is the Elvis test: if A is not UNDEFINED it sets Dst to A and jumps to T;
	// otherwise it falls through to the fallback, which ends in an RMove into the same Dst.
	RJmpIfDefined
)

// RegInstr is one register-form instruction. Dst, A and B are register numbers; Imm is an index into
// one of the source Program's pools; Args indexes RegProgram.args; T is an absolute instruction
// index. Which fields are meaningful depends on Op.
type RegInstr struct {
	Op        RegOp
	Dst, A, B int32
	Imm       int32
	Args      int32
	T         int32
}

// RegProgram is a compiled expression in register form.
//
// It owns no pools. src is the stack-form Program every pool index refers to, which is what keeps
// the two forms from being able to disagree about what a constant or an operator is.
type RegProgram struct {
	code  []RegInstr
	args  [][]int32 // RCallFunc argument register lists
	nregs int
	res   int32 // the register holding the expression's value, or -1 for an empty program
	src   *Program
}

// Len returns the instruction count (for tests and benchmarks).
func (rp *RegProgram) Len() int { return len(rp.code) }

// NumRegs returns how many virtual registers the program defines.
//
// Each definition gets a fresh register: there is no reuse, so this is also the number of definitions
// and, for now, the peak live count. Making it smaller is the register allocator's job and is
// deliberately not done here -- an unoptimized form that is obviously correct is what the optimizer
// gets validated against.
func (rp *RegProgram) NumRegs() int { return rp.nregs }

// Source returns the stack-form Program this was lowered from.
func (rp *RegProgram) Source() *Program { return rp.src }

// LowerToRegisters converts a compiled expression from stack form to register form.
//
// It accepts every Program compile.go can produce, including one delegated wholesale to the
// evaluator, and it never fails on valid input: the stack form is well-formed by construction (the
// compiler emits balanced pushes and pops, and jumps only within the two patterns above), so there is
// no expression for which this can get stuck.
//
// It panics rather than degrades on input that cannot occur -- a stack underflow, a combine with no
// matching short-circuit, or an opcode it has not been taught. Each is a compiler bug, and each would
// otherwise produce a register program that quietly disagrees with the stack one. The last is the
// one that will actually happen: adding an opcode without extending this conversion.
func LowerToRegisters(p *Program) *RegProgram {
	l := &lowerer{
		src:     p,
		rp:      &RegProgram{src: p, res: -1},
		newIdx:  make([]int32, len(p.code)+1),
		pending: map[int][]elvisJoin{},
	}
	l.lower()
	return l.rp
}

// lowerer holds the state of one conversion.
type lowerer struct {
	src *Program
	rp  *RegProgram

	// vstack maps the stack form's operand stack to the registers currently holding those operands.
	vstack []int32

	// shortDst is the destination register allocated by each pending RShort, popped by the matching
	// RCombine. A stack because logical operators nest.
	shortDst []int32

	// pending maps a stack-form jump target to the Elvis joins that land there, innermost last.
	pending map[int][]elvisJoin

	// newIdx[oldIp] is the register-form index that oldIp's code begins at, used to retarget jumps
	// once all of it is emitted. It has one extra entry for a jump to the end of the program.
	newIdx []int32

	// jumps are the register-form indices of instructions whose T is still a stack-form index.
	jumps []int
}

// elvisJoin is one Elvis whose fall-through path has to move its value into dst at a join, and whose
// jump instruction is at jumpIdx in the register form.
//
// The jump index is kept because each Elvis at a shared join needs its OWN landing point: it has
// already written dst itself, so it must skip its own move -- but it must still run the moves of any
// Elvis it is nested INSIDE, whose destination has not been written on this path. One landing point
// per target cannot express that, which is what makes `A ?: (B ?: 7)` the shape that catches it.
type elvisJoin struct {
	dst     int32
	jumpIdx int
}

func (l *lowerer) reg() int32 {
	r := int32(l.rp.nregs)
	l.rp.nregs++
	return r
}

func (l *lowerer) emit(in RegInstr) int {
	l.rp.code = append(l.rp.code, in)
	return len(l.rp.code) - 1
}

func (l *lowerer) push(r int32) { l.vstack = append(l.vstack, r) }

func (l *lowerer) pop() int32 {
	n := len(l.vstack)
	if n == 0 {
		panic("vm: register lowering stack underflow; the stack-form Program is malformed")
	}
	r := l.vstack[n-1]
	l.vstack = l.vstack[:n-1]
	return r
}

func (l *lowerer) lower() {
	for ip := 0; ip < len(l.src.code); ip++ {
		// newIdx is recorded BEFORE the joins, so a jump that is not itself one of these joins --
		// a logical short-circuit sharing this target -- lands ahead of them and still runs them.
		// Its value is what the fall-through produced, so the destinations here must receive it.
		l.newIdx[ip] = int32(len(l.rp.code))
		l.emitJoins(ip)
		l.one(ip)
	}
	// The end of the program is a jump target too -- every Elvis and every logical operator whose
	// right operand ends the expression targets it.
	l.newIdx[len(l.src.code)] = int32(len(l.rp.code))
	l.emitJoins(len(l.src.code))
	if len(l.vstack) > 0 {
		l.rp.res = l.vstack[len(l.vstack)-1]
	}
	// A jump to the end needs something to land on.
	if l.needsEndPad() {
		l.emit(RegInstr{Op: RNop})
	}
	l.retarget()
}

// emitJoins applies the Elvis joins registered for stack-form index ip, innermost first, and gives
// each one's jump its own landing point immediately after its own move.
//
// Innermost first is forced by the data flow: the inner Elvis's move produces the value the outer
// one's move consumes. Landing after its own move is forced by the control flow: on its jump path the
// destination is already written, so repeating the move would overwrite it with an operand that was
// never evaluated -- while the outer moves still have to run.
func (l *lowerer) emitJoins(ip int) {
	joins := l.pending[ip]
	for i := len(joins) - 1; i >= 0; i-- {
		src := l.pop()
		l.emit(RegInstr{Op: RMove, Dst: joins[i].dst, A: src})
		l.push(joins[i].dst)
		l.rp.code[joins[i].jumpIdx].T = int32(len(l.rp.code))
	}
	delete(l.pending, ip)
}

// needsEndPad reports whether any jump lands one past the last instruction, which needs a landing pad
// because there is nothing there to execute.
//
// Two kinds have to be checked separately: an Elvis jump's target is already a register-form index by
// now (emitJoins resolved it), while a logical short-circuit still carries its stack-form target.
func (l *lowerer) needsEndPad() bool {
	end := len(l.rp.code)
	for _, j := range l.jumps {
		if int(l.rp.code[j].T) == len(l.src.code) {
			return true
		}
	}
	for i := range l.rp.code {
		if l.rp.code[i].Op == RJmpIfDefined && int(l.rp.code[i].T) == end {
			return true
		}
	}
	return false
}

// retarget rewrites stack-form jump targets into register-form indices.
func (l *lowerer) retarget() {
	for _, j := range l.jumps {
		l.rp.code[j].T = l.newIdx[l.rp.code[j].T]
	}
}

// one lowers the single stack-form instruction at ip.
func (l *lowerer) one(ip int) {
	in := l.src.code[ip]
	switch in.Op {
	case OpPushConst:
		d := l.reg()
		l.emit(RegInstr{Op: RConst, Dst: d, Imm: in.A})
		l.push(d)
	case OpPushTrue:
		d := l.reg()
		l.emit(RegInstr{Op: RTrue, Dst: d})
		l.push(d)
	case OpPushFalse:
		d := l.reg()
		l.emit(RegInstr{Op: RFalse, Dst: d})
		l.push(d)
	case OpPushUndef:
		d := l.reg()
		l.emit(RegInstr{Op: RUndef, Dst: d})
		l.push(d)
	case OpPushError:
		d := l.reg()
		l.emit(RegInstr{Op: RError, Dst: d})
		l.push(d)
	case OpLoadRef:
		d := l.reg()
		l.emit(RegInstr{Op: RLoadRef, Dst: d, Imm: in.A})
		l.push(d)
	case OpBinop:
		// The stack form pops right then left, so left was pushed first.
		b := l.pop()
		a := l.pop()
		d := l.reg()
		l.emit(RegInstr{Op: RBinop, Dst: d, A: a, B: b, Imm: in.A})
		l.push(d)
	case OpUnop:
		a := l.pop()
		d := l.reg()
		l.emit(RegInstr{Op: RUnop, Dst: d, A: a, Imm: in.A})
		l.push(d)
	case OpMember:
		a := l.pop()
		d := l.reg()
		l.emit(RegInstr{Op: RMember, Dst: d, A: a, Imm: in.A})
		l.push(d)
	case OpCallFunc:
		f := l.src.funcs[in.A]
		args := make([]int32, f.argc)
		for i := f.argc - 1; i >= 0; i-- {
			args[i] = l.pop()
		}
		l.rp.args = append(l.rp.args, args)
		d := l.reg()
		l.emit(RegInstr{Op: RCallFunc, Dst: d, Imm: in.A, Args: int32(len(l.rp.args) - 1)})
		l.push(d)
	case OpEvalNode:
		d := l.reg()
		l.emit(RegInstr{Op: REvalNode, Dst: d, Imm: in.A})
		l.push(d)
	case OpShortAnd, OpShortOr:
		// The left operand stays on the stack for the combine; the destination is allocated here so
		// that both the short-circuit path and the combine write the same register.
		if len(l.vstack) == 0 {
			panic("vm: register lowering found a short-circuit with no left operand")
		}
		a := l.vstack[len(l.vstack)-1]
		d := l.reg()
		op := RShortAnd
		if in.Op == OpShortOr {
			op = RShortOr
		}
		j := l.emit(RegInstr{Op: op, Dst: d, A: a, T: in.A})
		l.jumps = append(l.jumps, j)
		l.shortDst = append(l.shortDst, d)
	case OpCombineAnd, OpCombineOr:
		b := l.pop()
		a := l.pop()
		n := len(l.shortDst)
		if n == 0 {
			panic("vm: register lowering found a combine with no matching short-circuit")
		}
		d := l.shortDst[n-1]
		l.shortDst = l.shortDst[:n-1]
		op := RCombineAnd
		if in.Op == OpCombineOr {
			op = RCombineOr
		}
		l.emit(RegInstr{Op: op, Dst: d, A: a, B: b})
		l.push(d)
	case OpJmpIfNotUndef:
		// The stack form pops the left operand on the fall-through path and leaves it as the result
		// on the jump path, so the register form pops it here and the jump carries it into Dst.
		a := l.pop()
		d := l.reg()
		// Not added to l.jumps: emitJoins resolves this jump's target directly, because it lands
		// after its own move rather than at the target's newIdx.
		j := l.emit(RegInstr{Op: RJmpIfDefined, Dst: d, A: a, T: -1})
		l.pending[int(in.A)] = append(l.pending[int(in.A)], elvisJoin{dst: d, jumpIdx: j})
	default:
		// An opcode this conversion has not been taught. It must fail LOUDLY rather than produce
		// something: the stack interpreter pushes ERROR for an opcode it does not know, and emitting
		// ERROR here would match that -- but only for an opcode NEITHER form implements. The real
		// case is an opcode the stack form implements and this one has not been taught yet, where
		// emitting ERROR is a silent wrong answer in a form that looks like it agreed. Every Opcode
		// is handled above today, so reaching this is a programming error: a new opcode was added
		// without extending the lowering.
		panic(fmt.Sprintf("vm: register lowering has no case for opcode %d; a new opcode must be taught to both forms", in.Op))
	}
}

// RunRegisters executes rp against scope and returns the resulting Value.
//
// It produces the same value Run produces for the Program rp was lowered from, which is the property
// FuzzRegisterParity exists to enforce. Every value operation goes through the same classad hooks the
// stack interpreter uses, so neither form has its own copy of any semantics.
func RunRegisters(rp *RegProgram, scope *classad.ClassAd) (result classad.Value) {
	ev := classad.NewEvaluator(scope)
	defer classad.RecoverCyclic(&result)
	return execReg(rp, ev, make([]classad.Value, rp.nregs))
}

// execReg runs rp against ev, using regs as the register file. regs must have at least rp.nregs
// entries; the caller may retain its backing array to amortize allocation across evaluations.
func execReg(rp *RegProgram, ev *classad.Evaluator, regs []classad.Value) classad.Value {
	p := rp.src
	code := rp.code
	for ip := 0; ip < len(code); {
		in := code[ip]
		switch in.Op {
		case RNop:
			ip++
		case RConst:
			regs[in.Dst] = p.consts[in.Imm]
			ip++
		case RTrue:
			regs[in.Dst] = classad.NewBoolValue(true)
			ip++
		case RFalse:
			regs[in.Dst] = classad.NewBoolValue(false)
			ip++
		case RUndef:
			regs[in.Dst] = classad.NewUndefinedValue()
			ip++
		case RError:
			regs[in.Dst] = classad.NewErrorValue()
			ip++
		case RLoadRef:
			regs[in.Dst] = ev.ResolveRefNode(p.refs[in.Imm].node)
			ip++
		case RBinop:
			regs[in.Dst] = ev.ApplyBinaryOp(p.ops[in.Imm], regs[in.A], regs[in.B])
			ip++
		case RCombineAnd:
			regs[in.Dst] = ev.ApplyBinaryOp("&&", regs[in.A], regs[in.B])
			ip++
		case RCombineOr:
			regs[in.Dst] = ev.ApplyBinaryOp("||", regs[in.A], regs[in.B])
			ip++
		case RUnop:
			regs[in.Dst] = ev.ApplyUnaryOp(p.ops[in.Imm], regs[in.A])
			ip++
		case RMember:
			regs[in.Dst] = p.members[in.Imm].eval(regs[in.A])
			ip++
		case RCallFunc:
			f := p.funcs[in.Imm]
			srcs := rp.args[in.Args]
			args := make([]classad.Value, len(srcs))
			for i, r := range srcs {
				args[i] = regs[r]
			}
			regs[in.Dst] = classad.ApplyFunction(ev, f.name, args)
			ip++
		case REvalNode:
			regs[in.Dst] = ev.Evaluate(p.nodes[in.Imm])
			ip++
		case RMove:
			regs[in.Dst] = regs[in.A]
			ip++
		case RShortAnd:
			if res, done := ev.ShortCircuit("&&", regs[in.A]); done {
				regs[in.Dst] = res
				ip = int(in.T)
			} else {
				ip++
			}
		case RShortOr:
			if res, done := ev.ShortCircuit("||", regs[in.A]); done {
				regs[in.Dst] = res
				ip = int(in.T)
			} else {
				ip++
			}
		case RJmpIfDefined:
			if !regs[in.A].IsUndefined() {
				regs[in.Dst] = regs[in.A]
				ip = int(in.T)
			} else {
				ip++
			}
		default:
			regs[in.Dst] = classad.NewErrorValue()
			ip++
		}
	}
	if rp.res < 0 {
		return classad.NewUndefinedValue()
	}
	return regs[rp.res]
}
