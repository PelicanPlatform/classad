package vm

import (
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// CompileProgram lowers a ClassAd expression to a Program. Node types with
// subtle scope/short-circuit/laziness semantics (conditional, list, record,
// select, subscript) are emitted as OpEvalNode and delegated to the tree-walking
// evaluator at run time; the rest are native instructions.
//
// A function call is native when classad.StrictFunction accepts it -- the
// built-in is a pure function of its argument values -- and delegated otherwise.
// That split is what keeps a program containing substr() or regexp() Native(),
// and so keeps it eligible for the columnar accelerators. member/identicalMember
// over a list of constants is compiled to its own specialized OpMember, which is
// preferred over the generic call when it applies.
func CompileProgram(expr ast.Expr) *Program {
	c := &compiler{p: &Program{}, seen: map[string]bool{}}
	if astHeight(expr) >= maxNativeDepth {
		// Pathologically deep expression: delegate the whole thing to the
		// tree-walking evaluator so its recursion-depth guard (maxEvalDepth)
		// governs the result identically. The flat interpreter accumulates no
		// per-node depth, so a deep operator chain would otherwise compute a value
		// where the tree-walker's guard fires and returns error. Real expressions
		// are a handful of levels deep; this only trips on adversarial input.
		c.ins(OpEvalNode, c.addNode(expr))
	} else {
		c.emit(expr)
	}
	c.p.readAttrs = collectReads(expr, map[string]bool{}, nil)
	c.p.expr = expr
	c.p.vops = vopPrefix(c.p.code)
	c.p.refSlots, c.p.nColSlots = colSlots(c.p.refs)
	return c.p
}

// maxNativeDepth bounds the AST nesting the compiler lowers to native
// instructions. It must stay comfortably below classad's maxEvalDepth (the
// tree-walker's 2000-level recursion guard): a natively-compiled expression of
// height h makes the tree-walker reach depth h, so keeping h < maxNativeDepth <
// maxEvalDepth guarantees native evaluation never straddles the guard. Deeper
// expressions are delegated wholesale (see CompileProgram), which evaluates them
// through the tree-walker from depth 0 -- bit-identical to the reference, guard
// and all.
const maxNativeDepth = 1000

// astHeight returns the maximum node nesting depth of e, counting every node
// (matching the tree-walker, which increments its depth counter once per node).
func astHeight(e ast.Expr) int {
	switch v := e.(type) {
	case nil:
		return 1
	case *ast.ParenExpr:
		return 1 + astHeight(v.Inner)
	case *ast.UnaryOp:
		return 1 + astHeight(v.Expr)
	case *ast.BinaryOp:
		return 1 + max(astHeight(v.Left), astHeight(v.Right))
	case *ast.ElvisExpr:
		return 1 + max(astHeight(v.Left), astHeight(v.Right))
	case *ast.ConditionalExpr:
		return 1 + max(astHeight(v.Condition), astHeight(v.TrueExpr), astHeight(v.FalseExpr))
	case *ast.ListLiteral:
		h := 0
		for _, el := range v.Elements {
			h = max(h, astHeight(el))
		}
		return 1 + h
	case *ast.FunctionCall:
		h := 0
		for _, a := range v.Args {
			h = max(h, astHeight(a))
		}
		return 1 + h
	case *ast.SelectExpr:
		return 1 + astHeight(v.Record)
	case *ast.SubscriptExpr:
		return 1 + max(astHeight(v.Container), astHeight(v.Index))
	case *ast.RecordLiteral:
		h := 0
		if v.ClassAd != nil {
			for _, attr := range v.ClassAd.Attributes {
				h = max(h, astHeight(attr.Value))
			}
		}
		return 1 + h
	default:
		return 1
	}
}

type compiler struct {
	p    *Program
	seen map[string]bool

	// locals maps a folded local name to its index, for a SCRIPT. Nil for an ordinary expression, in
	// which case every unscoped reference is an attribute. See Script.
	locals map[string]int32
}

// ins appends an instruction and returns its index (for later backpatching).
func (c *compiler) ins(op Opcode, a int32) int {
	c.p.code = append(c.p.code, Instr{Op: op, A: a})
	return len(c.p.code) - 1
}

// patch sets the argument of a previously-emitted instruction (a jump target).
func (c *compiler) patch(i int, target int) {
	c.p.code[i].A = int32(target)
}

func (c *compiler) pushConst(v classad.Value) {
	c.p.consts = append(c.p.consts, v)
	c.ins(OpPushConst, int32(len(c.p.consts)-1))
}

func (c *compiler) addOp(op string) int32 {
	c.p.ops = append(c.p.ops, op)
	return int32(len(c.p.ops) - 1)
}

func (c *compiler) addRef(name string, scope ast.AttributeScope) int32 {
	node := ast.NewAttributeReference(name, scope)
	c.p.refs = append(c.p.refs, refInfo{name: name, scope: scope, node: node, curTime: classad.IsCurrentTimeName(name)})
	return int32(len(c.p.refs) - 1)
}

func (c *compiler) addFunc(name string, argc int) int32 {
	c.p.funcs = append(c.p.funcs, funcInfo{name: strings.ToLower(name), argc: argc})
	return int32(len(c.p.funcs) - 1)
}

func (c *compiler) addNode(n ast.Expr) int32 {
	c.p.nodes = append(c.p.nodes, n)
	return int32(len(c.p.nodes) - 1)
}

func (c *compiler) emit(expr ast.Expr) {
	switch v := expr.(type) {
	case nil, *ast.UndefinedLiteral:
		c.ins(OpPushUndef, 0)
	case *ast.ErrorLiteral:
		c.ins(OpPushError, 0)
	case *ast.BooleanLiteral:
		if v.Value {
			c.ins(OpPushTrue, 0)
		} else {
			c.ins(OpPushFalse, 0)
		}
	case *ast.IntegerLiteral:
		c.pushConst(classad.NewIntValue(v.Value))
	case *ast.RealLiteral:
		c.pushConst(classad.NewRealValue(v.Value))
	case *ast.StringLiteral:
		c.pushConst(classad.NewStringValue(v.Value))
	case *ast.AttributeReference:
		// In a script, a BARE name that is currently bound is a local, not an attribute. A scoped
		// reference (MY.x, TARGET.x) is always an attribute, which is what gives a script a way to
		// reach an attribute whose name a local has taken.
		if v.Scope == ast.NoScope && c.locals != nil {
			if idx, ok := c.locals[v.NormalizedName()]; ok {
				c.ins(OpLoadLocal, idx)
				return
			}
		}
		c.ins(OpLoadRef, c.addRef(v.Name, v.Scope))
	case *ast.ParenExpr:
		// Parentheses are transparent to evaluation.
		c.emit(v.Inner)
	case *ast.UnaryOp:
		c.emit(v.Expr)
		c.ins(OpUnop, c.addOp(v.Op))
	case *ast.BinaryOp:
		switch v.Op {
		case "&&":
			c.emitLogical(v, OpShortAnd, OpCombineAnd)
		case "||":
			c.emitLogical(v, OpShortOr, OpCombineOr)
		default:
			c.emit(v.Left)
			c.emit(v.Right)
			c.ins(OpBinop, c.addOp(v.Op))
		}
	case *ast.ElvisExpr:
		// left ?: right  ==  (left is undefined) ? right : left
		c.emit(v.Left)
		j := c.ins(OpJmpIfNotUndef, -1)
		c.emit(v.Right)
		c.patch(j, len(c.p.code))
	case *ast.FunctionCall:
		// ifThenElse becomes a branch, not a call, so it must be tried before the strict-call path
		// (which refuses it anyway, because an eager call would evaluate both branches).
		if c.emitIfThenElse(v) {
			return
		}
		// Three outcomes, in order of preference. member/identicalMember over a
		// constant list has a real vector kernel, so it gets its specialized
		// OpMember rather than the generic call -- memberVec tests a per-block
		// bitset over dictionary codes, where a generic call would box every
		// element and scan the list per record.
		if c.emitMember(v) {
			return
		}
		// stringListMember over a literal list is a set lookup rather than a per-record split.
		if c.emitStrListMember(v) {
			return
		}
		// Otherwise a STRICT built-in becomes a native call: its arguments are
		// ordinary compiled expressions and the call itself is a pure value
		// operation. Everything else falls through to delegation -- a lazy
		// built-in, one that needs the scope or the raw AST, a nondeterministic
		// one, an unknown name and a wrong argument count, all of which
		// StrictFunction refuses and each of which would be observably wrong as
		// an eager call (see its comment).
		if !classad.StrictFunction(v.Name, len(v.Args)) {
			c.ins(OpEvalNode, c.addNode(expr))
			return
		}
		for _, a := range v.Args {
			c.emit(a)
		}
		c.ins(OpCallFunc, c.addFunc(v.Name, len(v.Args)))
	default:
		// ConditionalExpr, ListLiteral, RecordLiteral, SelectExpr, SubscriptExpr,
		// or any future node: delegate to the evaluator. (A FunctionCall reaches
		// the evaluator too, but only the ones StrictFunction refuses; the case
		// above compiles the rest.)
		c.ins(OpEvalNode, c.addNode(expr))
	}
}

// emitIfThenElse compiles ifThenElse(cond, t, f) to a branch, and reports whether it did.
//
// Layout, with each path pushing exactly one value so the stack stays balanced at the join:
//
//	  <cond>
//	  branch          ; true falls through; false/undef/error jump
//	  <t>
//	  jmp end
//	onFalse:
//	  <f>
//	  jmp end
//	onUndef:
//	  push undefined  ; NEITHER branch is evaluated
//	  jmp end
//	onError:
//	  push error      ; likewise
//	end:
//
// A call with the wrong argument count is declined, which leaves it delegated -- and delegation is
// what gets it right, because the reference returns ERROR for a bad arity WITHOUT evaluating any
// argument (evaluateFunctionCall special-cases ifThenElse before its arity check).
func (c *compiler) emitIfThenElse(v *ast.FunctionCall) bool {
	if !strings.EqualFold(v.Name, "ifThenElse") || len(v.Args) != 3 {
		return false
	}
	c.emit(v.Args[0])
	c.p.conds = append(c.p.conds, condTargets{})
	k := int32(len(c.p.conds) - 1)
	c.ins(OpBranchCond, k)

	c.emit(v.Args[1]) // the true branch falls through
	jTrue := c.ins(OpJmp, -1)

	onFalse := len(c.p.code)
	c.emit(v.Args[2])
	jFalse := c.ins(OpJmp, -1)

	onUndef := len(c.p.code)
	c.ins(OpPushUndef, 0)
	jUndef := c.ins(OpJmp, -1)

	onError := len(c.p.code)
	c.ins(OpPushError, 0)

	end := len(c.p.code)
	c.p.conds[k] = condTargets{
		onFalse: int32(onFalse), onUndef: int32(onUndef), onError: int32(onError), end: int32(end),
	}
	c.patch(jTrue, end)
	c.patch(jFalse, end)
	c.patch(jUndef, end)
	return true
}

// emitLogical compiles a short-circuiting logical operator (&& or ||):
//
//	<left>
//	short   L_end     ; if left short-circuits, replace top with result, jump end
//	<right>
//	combine           ; pop right,left; push combined value
//	L_end:
func (c *compiler) emitLogical(v *ast.BinaryOp, short, combine Opcode) {
	c.emit(v.Left)
	j := c.ins(short, -1)
	c.emit(v.Right)
	c.ins(combine, 0)
	c.patch(j, len(c.p.code))
}

// collectReads walks expr collecting distinct unscoped attribute-reference names
// (in first-seen order) that the expression may read. It over-approximates
// (e.g. it includes the argument of unparse(), which is not actually read),
// which is safe for the query planner. Scoped references (MY/TARGET/PARENT) are
// excluded because they do not resolve against the current ad's attributes.
func collectReads(expr ast.Expr, seen map[string]bool, out []string) []string {
	switch v := expr.(type) {
	case nil:
		return out
	case *ast.AttributeReference:
		if v.Scope == ast.NoScope && !seen[v.Name] {
			seen[v.Name] = true
			out = append(out, v.Name)
		}
	case *ast.ParenExpr:
		out = collectReads(v.Inner, seen, out)
	case *ast.UnaryOp:
		out = collectReads(v.Expr, seen, out)
	case *ast.BinaryOp:
		out = collectReads(v.Left, seen, out)
		out = collectReads(v.Right, seen, out)
	case *ast.ElvisExpr:
		out = collectReads(v.Left, seen, out)
		out = collectReads(v.Right, seen, out)
	case *ast.ConditionalExpr:
		out = collectReads(v.Condition, seen, out)
		out = collectReads(v.TrueExpr, seen, out)
		out = collectReads(v.FalseExpr, seen, out)
	case *ast.ListLiteral:
		for _, el := range v.Elements {
			out = collectReads(el, seen, out)
		}
	case *ast.FunctionCall:
		for _, a := range v.Args {
			out = collectReads(a, seen, out)
		}
	case *ast.SelectExpr:
		out = collectReads(v.Record, seen, out)
	case *ast.SubscriptExpr:
		out = collectReads(v.Container, seen, out)
		out = collectReads(v.Index, seen, out)
	case *ast.RecordLiteral:
		if v.ClassAd != nil {
			for _, attr := range v.ClassAd.Attributes {
				out = collectReads(attr.Value, seen, out)
			}
		}
	}
	return out
}
