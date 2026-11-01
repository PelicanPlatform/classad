package vm

import (
	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// funcInfo is a pooled built-in call targeted by OpCallFunc: the folded function
// name and the number of argument values the instruction pops.
//
// argc is recorded rather than re-derived from the name because several built-ins
// are variadic or have optional arguments (join, split, substr, regexp), so the
// arity accepted at compile time is the only thing that says how deep this call's
// operands sit on the stack.
type funcInfo struct {
	name string // already lower-cased, as classad matches built-in names
	argc int
}

// condTargets is where OpBranchCond goes for each condition state other than true, which falls
// through. All three are absolute instruction indices.
type condTargets struct {
	onFalse, onUndef, onError int32
}

// refInfo is a pooled attribute reference (name + scope) targeted by OpLoadRef.
type refInfo struct {
	name string
	// node is the reference as an AST node, built once at compile time. The interpreter hands it
	// straight to the evaluator, which would otherwise construct one -- heap-allocating and folding
	// the name -- on every resolution of every record. Read-only after compilation, so one program
	// can be evaluated concurrently by several Matchers.
	node  *ast.AttributeReference
	scope ast.AttributeScope
	// curTime marks a reference named CurrentTime, in any scope: when the ad it names
	// does not define it, its value is a time rather than undefined.
	curTime bool
}

// Program is a compiled expression: a flat instruction stream plus the constant
// pools its instructions index into.
type Program struct {
	code   []Instr
	consts []classad.Value // OpPushConst
	ops    []string        // OpBinop / OpUnop operator strings
	refs   []refInfo       // OpLoadRef
	nodes  []ast.Expr      // OpEvalNode (delegated subtrees)
	funcs  []funcInfo      // OpCallFunc
	conds  []condTargets   // OpBranchCond

	// members are the constant lists of member/identicalMember calls (OpMember).
	members []*memberSet

	// readAttrs is the set of distinct unscoped attribute names the program may
	// read, in first-seen order. Used by the store's query planner (M3) to pick
	// the hot-header fast path; nil-safe to ignore.
	readAttrs []string

	// expr is the source expression, retained so the store can compute a read
	// plan (see ReadPlan) for partial-decode evaluation.
	expr ast.Expr

	// now, when nonzero, is the time a CurrentTime reference the scope does not define
	// evaluates to (see Compile). Zero reads the clock, as the evaluator does.
	now int64
	// foldNow reports that the query reads an unscoped or MY CurrentTime, which the
	// planner's probes fold to now (see Probe).
	foldNow bool
}

// ReadAttrs returns the distinct unscoped attribute names the program may read.
// The slice must not be mutated.
func (p *Program) ReadAttrs() []string { return p.readAttrs }

// Len returns the number of instructions (for tests/benchmarks).
func (p *Program) Len() int { return len(p.code) }
