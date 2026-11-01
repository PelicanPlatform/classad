package vm

// CHARGED STEPS: what one execution is billed for.
//
// A step is a VALUE OPERATION -- an instruction that produces a value: a literal, an attribute load,
// a binary or unary operator, a logical combine, a member test, a call, a delegated subtree. It is
// not a jump, a short-circuit test, a move, a local load or store, or a nop.
//
// WHY THAT LINE AND NOT "ONE INSTRUCTION". The count has two jobs, and only this definition does
// both.
//
// It is the BUDGET, which exists because a script can loop and `while true: x = x` has no answer. A
// bound counted in instructions rather than wall clock is the only reproducible kind: a time limit
// makes the same script succeed or fail depending on what else the machine is doing, and a fault
// nobody can reproduce cannot be debugged.
//
// It is also the one quantity the EXECUTORS CAN BE COMPARED ON, and there are three of them over two
// instruction sets -- the stack interpreter and the vector executor share one, the register executor
// has its own. Counting every instruction makes the two instruction sets disagree by construction:
// the register form expresses a join with a move and a local read with a move, and the stack form
// expresses them by leaving a value in place. Neither is doing more work than the other. Counting
// only value operations makes the lowering 1:1, so a record's charge is a property of the EXPRESSION
// rather than of the form it was lowered to -- which is what lets a divergence between executors be
// attributed rather than merely noticed. Verified over every corpus in this package, per record,
// rather than argued: see TestChargeIsTheSameInBothForms.
//
// WHAT THIS DELIBERATELY DOES NOT MEASURE is real work. A local read is a move in the register form
// and a vector copy in the vector executor; the second is thousands of times the first and both are
// charged zero. The budget's job is termination, and a benchmark's job is cost. Conflating them is
// what would make the number useless for both.
//
// TERMINATION STILL HOLDS, because every back edge the compiler emits passes through a condition
// expression, and the smallest expression is a literal, which is a value operation. So an iteration
// cannot be free. TestEveryLoopIterationIsCharged pins that against the compiler rather than
// assuming it of the interpreter.

// valueOps and regValueOps are bitmasks over the two opcode sets. A mask rather than a range test
// because the stack opcodes are not contiguous and, more to the point, a range test silently
// reclassifies every opcode after the one somebody inserts in the middle.
const valueOps = 1<<OpPushConst | 1<<OpPushTrue | 1<<OpPushFalse | 1<<OpPushUndef | 1<<OpPushError |
	1<<OpLoadRef | 1<<OpBinop | 1<<OpUnop | 1<<OpCombineAnd | 1<<OpCombineOr |
	1<<OpMember | 1<<OpStrListMember | 1<<OpCallFunc | 1<<OpEvalNode

const regValueOps = 1<<RConst | 1<<RTrue | 1<<RFalse | 1<<RUndef | 1<<RError |
	1<<RLoadRef | 1<<RBinop | 1<<RUnop | 1<<RCombineAnd | 1<<RCombineOr |
	1<<RMember | 1<<RStrListMember | 1<<RCallFunc | 1<<REvalNode

// nonValueOps and regNonValueOps are the complements, written out rather than derived. They exist so
// that an opcode added later is in NEITHER mask, which TestEveryOpcodeIsClassified turns into a
// failure -- where a single mask would silently leave it uncharged, and an uncharged instruction in a
// loop body is an execution that does not terminate.
const nonValueOps = 1<<OpShortAnd | 1<<OpShortOr | 1<<OpJmpIfNotUndef | 1<<OpJmp | 1<<OpBranchCond |
	1<<OpLoadLocal | 1<<OpStoreLocal | 1<<OpBranchVoid | 1<<OpJmpVoid | 1<<OpFault

const regNonValueOps = 1<<RNop | 1<<RMove | 1<<RShortAnd | 1<<RShortOr | 1<<RJump | 1<<RFault |
	1<<RBranchCond | 1<<RJmpIfDefined

// isValueOp reports whether a stack-form instruction is charged.
func isValueOp(op Opcode) bool { return op < 64 && uint64(valueOps)>>op&1 != 0 }

// isRegValueOp reports whether a register-form instruction is charged.
func isRegValueOp(op RegOp) bool { return op < 64 && uint64(regValueOps)>>op&1 != 0 }

// isClassified reports whether an opcode has been deliberately placed in one mask or the other.
func isClassified(op Opcode) bool {
	return op < 64 && uint64(valueOps|nonValueOps)>>op&1 != 0
}

func isRegClassified(op RegOp) bool {
	return op < 64 && uint64(regValueOps|regNonValueOps)>>op&1 != 0
}

// vopPrefix returns, for each instruction index, how many charged instructions precede it, plus a
// final entry for the end of the program. The charge for executing a contiguous run [a, b) at a live
// count L is therefore (vopPrefix[b] - vopPrefix[a]) * L.
//
// It is built ONCE per program rather than tested per instruction. Testing per instruction is not
// expensive in itself -- a bitmask shift -- but adding it, the multiply-accumulate and the budget
// comparison to the vector executor's dispatch loop together cost 11% of a string comparison scan,
// where each alone cost between 1% and 4%. That is a code-layout effect rather than arithmetic, and
// the way out of one is to take the work out of the loop rather than to shave it.
func vopPrefix(code []Instr) []int32 {
	return vopPrefixInto(nil, code)
}

// vopPrefixInto is vopPrefix reusing a caller's buffer.
func vopPrefixInto(buf []int32, code []Instr) []int32 {
	p := buf[:0]
	if cap(p) < len(code)+1 {
		p = make([]int32, 0, len(code)+1)
	}
	p = p[:len(code)+1]
	n := int32(0)
	for i, in := range code {
		p[i] = n
		if isValueOp(in.Op) {
			n++
		}
	}
	p[len(code)] = n
	return p
}
