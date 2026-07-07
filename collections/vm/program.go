package vm

import (
	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// refInfo is a pooled attribute reference (name + scope) targeted by OpLoadRef.
type refInfo struct {
	name  string
	scope ast.AttributeScope
}

// Program is a compiled expression: a flat instruction stream plus the constant
// pools its instructions index into.
type Program struct {
	code   []Instr
	consts []classad.Value // OpPushConst
	ops    []string        // OpBinop / OpUnop operator strings
	refs   []refInfo       // OpLoadRef
	nodes  []ast.Expr      // OpEvalNode (delegated subtrees)

	// readAttrs is the set of distinct unscoped attribute names the program may
	// read, in first-seen order. Used by the store's query planner (M3) to pick
	// the hot-header fast path; nil-safe to ignore.
	readAttrs []string

	// expr is the source expression, retained so the store can compute a read
	// plan (see ReadPlan) for partial-decode evaluation.
	expr ast.Expr
}

// ReadAttrs returns the distinct unscoped attribute names the program may read.
// The slice must not be mutated.
func (p *Program) ReadAttrs() []string { return p.readAttrs }

// Len returns the number of instructions (for tests/benchmarks).
func (p *Program) Len() int { return len(p.code) }
