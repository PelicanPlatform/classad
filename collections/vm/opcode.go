// Package vm compiles ClassAd expressions to a linear instruction stream and
// interprets them against a scope, replacing per-query AST walks.
//
// Parity with the tree-walking evaluator is the overriding requirement: the
// interpreter routes every value operation through the exported hooks in the
// classad package (ApplyBinaryOp/ApplyUnaryOp/ResolveRef/ShortCircuit) and
// delegates any node type it does not natively compile back to the evaluator
// via an EvalNode instruction. It therefore cannot diverge from classad.Eval by
// construction; the differential fuzz test enforces this.
package vm

// Opcode identifies an instruction. The program is a flat []Instr slice (a
// struct-of-op-and-arg IR); packing it to a dense byte stream is a future
// optimization that does not affect semantics.
type Opcode uint8

const (
	// OpPushConst pushes consts[A].
	OpPushConst Opcode = iota
	// OpPushTrue/False/Undef/Error push the corresponding literal (no arg).
	OpPushTrue
	OpPushFalse
	OpPushUndef
	OpPushError

	// OpLoadRef resolves refs[A] (name+scope) in the scope and pushes the result.
	OpLoadRef

	// OpBinop pops right,left and pushes ApplyBinaryOp(ops[A], left, right).
	// Used for all non-short-circuiting binary operators.
	OpBinop
	// OpUnop pops v and pushes ApplyUnaryOp(ops[A], v).
	OpUnop

	// OpShortAnd/OpShortOr peek the left operand already on the stack. If it
	// short-circuits the logical operator, they replace it with the operator's
	// result and jump to A; otherwise they leave it on the stack and fall
	// through to the compiled right operand.
	OpShortAnd
	OpShortOr
	// OpCombineAnd/OpCombineOr pop right,left and push the combined logical value
	// (reached only when the right operand was evaluated).
	OpCombineAnd
	OpCombineOr

	// OpJmpIfNotUndef implements the Elvis operator: peek top; if it is NOT
	// undefined, jump to A leaving it on the stack (the result); otherwise pop it
	// and fall through to the compiled fallback expression.
	OpJmpIfNotUndef

	// OpStrListMember applies stringListMember over the constant list strLists[A] to the value on
	// top of the stack, replacing it with the result.
	//
	// Only the case-sensitive variant over a LITERAL list compiles this way; see strlist.go for why
	// stringListIMember does not.
	OpStrListMember

	// OpJmp jumps unconditionally to A. Emitted only by a conditional, to leave a taken branch.
	OpJmp

	// OpBranchCond pops the condition and dispatches on classad.ConditionState using the target
	// triple conds[A]: CondTrue falls through to the following instruction, and CondFalse, CondUndef
	// and CondError jump to onFalse, onUndef and onError.
	//
	// This is what makes ifThenElse native. It cannot be a call, however strict-looking: it
	// evaluates only the branch it selects, so an eager call would evaluate both -- and a branch is
	// allowed to be a cyclic self-reference that must never be reached. A branch, not a call, is also
	// why the UNDEFINED and ERROR cases get their own targets rather than a shared one: they produce
	// a value without evaluating either branch, which only control flow can express.
	//
	// Note it does NOT lower the ternary `c ? t : f`, whose semantics differ in exactly one place: an
	// UNDEFINED condition still evaluates BOTH branches (absorbing an error value but propagating a
	// cyclic reference), which a jump-based lowering can only express by emitting both branches
	// twice -- exponential in nesting depth. The ternary stays delegated.
	OpBranchCond

	// OpCallFunc calls funcs[A], a STRICT built-in: it pops the function's argc
	// argument values (arg0 deepest), applies classad.ApplyFunction to them and
	// pushes the result.
	//
	// Only built-ins that classad.StrictFunction accepts are compiled to this.
	// A lazy one (ifThenElse, strcat), one needing the scope or the raw AST
	// (eval, unparse, countMatches, evalInEachContext), a nondeterministic one
	// (random, time, formatTime), an unknown name and a wrong argument count all
	// stay OpEvalNode -- see StrictFunction's comment for why each must.
	//
	// The point of compiling a call natively is not call speed. It is that a
	// program containing one stays Native(), and Native() is what the columnar
	// accelerators gate on: with the call delegated, a single substr() or
	// regexp() in a constraint takes the whole query off the index probes, the
	// column prefilter and the vector executor and onto the per-record path.
	OpCallFunc

	// OpEvalNode delegates nodes[A] (an ast.Expr subtree) to the tree-walking
	// evaluator and pushes its value. The escape hatch for node types the
	// compiler does not lower to native instructions.
	OpEvalNode

	// OpMember replaces the top of the stack (member's or identicalMember's
	// first argument) with the call's value against members[A], a list of
	// constants compiled to sets. See member.go.
	OpMember

	// OpLoadLocal pushes the value bound to locals[A] by an earlier statement of a script.
	//
	// A script's locals are NOT attributes: `x = Cpus + 1` binds a name that later statements read
	// without the scope ever seeing it, so a reference to x must not compile to OpLoadRef and must
	// not be shadowed by -- or shadow -- an attribute of the same name. The compiler decides which
	// a reference is, because only the compiler knows what is bound at that point.
	//
	// It appears only in a script program, and the register lowering resolves it to the local's
	// register, so it never reaches a running program in stack form. The stack interpreter therefore
	// does not implement it; see ScriptProgram.
	OpLoadLocal

	// OpStoreLocal pops the top of the stack and binds it to locals[A].
	//
	// A local is a PLACE, not a value: it has one register for the whole script, written here and
	// read by OpLoadLocal. That is what lets a binding made on one path of a branch and read after
	// the merge be the same register either way, with no phi to construct -- which a model that gave
	// each binding a fresh register could not express, and is why that model was replaced.
	//
	// Popping is what makes a statement stack-neutral, so the next one starts clean.
	OpStoreLocal

	// OpBranchVoid pops the condition and dispatches on classad.ConditionState using conds[A], like
	// OpBranchCond -- but it produces NO value and has no join.
	//
	// The two cannot be the same opcode. OpBranchCond is an expression: each of its paths leaves a
	// value and they converge on one register. A statement branch's paths leave nothing, and what
	// converges is the LOCALS they wrote, each of which is already a single register. There is
	// nothing to join, and a lowering that tried to would be looking for values that are not there.
	OpBranchVoid

	// OpJmpVoid jumps unconditionally to A, carrying no value. OpJmp is its expression counterpart
	// and moves the path's value into the conditional's destination first.
	OpJmpVoid

	// OpFault ends the script with ERROR.
	//
	// It is reached when a condition is neither true nor false -- UNDEFINED, or a type that cannot
	// be one, such as a string or a list. That is a FAULT rather than a branch not taken, because
	// the alternative is to treat "I could not tell" as "no", which is how a misspelled attribute
	// silently turns a script into one that does nothing.
	//
	// In the scalar context a fault ends the script, which is the degenerate case of faulting the
	// one record being evaluated.
	OpFault
)

// Instr is one instruction: an opcode and a single integer argument whose
// meaning depends on the opcode (a pool index or an absolute jump target).
type Instr struct {
	Op Opcode
	A  int32
}
