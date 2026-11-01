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
// through, plus the instruction the whole conditional converges on. All four are absolute
// instruction indices.
//
// end is not needed to execute the stack form -- each branch jumps there and the interpreter just
// follows -- but the VECTOR executor evaluates every branch and blends at the join, so it has to know
// where the join is. Recording it here keeps that out of the emitter's layout: inferring it as
// "one past the error push" would make the vector executor depend on how emitIfThenElse happens to
// order its tail.
type condTargets struct {
	onFalse, onUndef, onError, end int32
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

	// strLists are the constant lists of stringListMember calls (OpStrListMember).
	strLists []*strListSet

	// faultSites are the places a script can fault, indexed by OpFault's operand. Empty for an
	// ordinary expression program, which cannot fault at all.
	faultSites []faultSite

	// locals are a script's local names, in binding order, indexed by OpLoadLocal. Empty for an
	// ordinary expression program. Kept for diagnostics; execution goes by index.
	locals []string

	// vops[i] is how many charged instructions precede instruction i, with a final entry for the end
	// (see vopPrefix). Built at compile time because a Program is shared by concurrent evaluations
	// and must be read-only once compiled.
	vops []int32

	// hasStatements marks a program with statement-level control flow -- an If or a While. It is set
	// by the script builder rather than derived, because the vector executor needs it BEFORE it has
	// looked at an instruction: it decides whether to set up the predication planes, which cost about
	// 2% of an expression scan and which an expression can never use.
	hasStatements bool

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
