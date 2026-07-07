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

	// OpEvalNode delegates nodes[A] (an ast.Expr subtree) to the tree-walking
	// evaluator and pushes its value. The escape hatch for node types the
	// compiler does not lower to native instructions.
	OpEvalNode
)

// Instr is one instruction: an opcode and a single integer argument whose
// meaning depends on the opcode (a pool index or an absolute jump target).
type Instr struct {
	Op Opcode
	A  int32
}
