package vm

import (
	"math"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// A VECTORIZED executor for the same compiled Program the scalar interpreter runs.
//
// The scalar interpreter dispatches once per instruction per RECORD. This one dispatches once per
// instruction per BATCH: a ref becomes a column load, a binop becomes a kernel over two vectors, a
// logical combine becomes a three-valued elementwise merge. Dispatch is amortized over the batch and
// the inner loops are contiguous, which is why the hand-written column scans are ~10x the per-record
// evaluator for the one shape they support. This generalizes that to arbitrary expressions.
//
// It executes the EXISTING Program rather than a second IR. The compiler, the constant pool, the ref
// table and the operator strings are shared, so an expression is lowered in exactly one place -- and
// the scalar executor becomes a per-record reference to test this one against on the same Program.
// That mirrors how EvalResolved reuses a Program with a different resolver.
//
// PARITY. The package's overriding requirement is that it cannot diverge from classad.Eval, which the
// scalar interpreter achieves by routing every value operation through the ApplyBinaryOp/ApplyUnaryOp
// hooks. Hand-writing vector kernels for the full semantics would abandon that. So each kernel has a
// fast loop for the ONE state combination that is unambiguous and dominant -- number OP number,
// bool AND bool -- and routes every other element, per element, through the same hook the scalar
// interpreter uses. Undefined, error, division by zero, mixed types and non-boolean logical operands
// are therefore computed by the reference implementation, not reproduced here. The fast paths cover
// the values real data is made of; the hook covers the ones semantics arguments are about.
//
// SHORT-CIRCUIT. OpShortAnd/OpShortOr are NO-OPS here. They exist to skip evaluating the right
// operand, which a vector cannot do per record, and the compiler always emits the matching
// OpCombineAnd/OpCombineOr anyway (emitLogical). Skipping the jump leaves the left operand on the
// stack, the right operand is evaluated, and the combine merges them -- safe rather than merely
// convenient because ClassAd's && and || are total, so `false && ERROR` is false whether or not the
// right side ran. The cost is that both sides are always evaluated; the benefit is that both are
// evaluated as vectors.
//
// NOT HANDLED, returning ok=false so the caller falls back rather than guessing: string values
// (constants or columns), the Elvis operator (a per-record select with no combine opcode to hang it
// on), and delegated subtrees (OpEvalNode, which Native() already excludes).

// Element states. A vector carries one per element, which is how three-valued logic and ERROR survive
// vectorization, and how int and real stay distinguishable -- 7/2 is not 7.0/2.0.
const (
	VsInt   uint8 = iota // I[i] is the integer
	VsReal               // I[i] is math.Float64bits of the real
	VsBool               // I[i] is 0 or 1
	VsUndef              // UNDEFINED
	VsError              // ERROR
)

// Vec is one batch of values. The payload is a single int64 slice because that is how both
// classad.Value and the columnar block already store a real (as IEEE-754 bits), so loading a column
// into a vector is a copy rather than a conversion, and no precision is lost for integers above 2^53.
type Vec struct {
	I  []int64
	St []uint8
}

func newVec(n int) Vec { return Vec{I: make([]int64, n), St: make([]uint8, n)} }

// Float returns element i as a float64 whatever its numeric state.
func (v Vec) Float(i int) float64 {
	if v.St[i] == VsReal {
		return math.Float64frombits(uint64(v.I[i]))
	}
	return float64(v.I[i])
}

// SetReal/SetInt/SetBool write one element.
func (v Vec) SetReal(i int, f float64) { v.St[i], v.I[i] = VsReal, int64(math.Float64bits(f)) }
func (v Vec) SetInt(i int, x int64)    { v.St[i], v.I[i] = VsInt, x }
func (v Vec) SetBool(i int, b bool) {
	v.St[i] = VsBool
	if b {
		v.I[i] = 1
	} else {
		v.I[i] = 0
	}
}

// value boxes element i for the parity hook.
func (v Vec) value(i int) classad.Value {
	switch v.St[i] {
	case VsInt:
		return classad.NewIntValue(v.I[i])
	case VsReal:
		return classad.NewRealValue(math.Float64frombits(uint64(v.I[i])))
	case VsBool:
		return classad.NewBoolValue(v.I[i] != 0)
	case VsUndef:
		return classad.NewUndefinedValue()
	default:
		return classad.NewErrorValue()
	}
}

// setValue unboxes a hook result back into element i. ok=false for a value a vector cannot hold (a
// string, list or nested ad), which makes the whole evaluation decline rather than truncate.
func (v Vec) setValue(i int, val classad.Value) bool {
	switch val.Type() {
	case classad.IntegerValue:
		x, err := val.IntValue()
		if err != nil {
			return false
		}
		v.SetInt(i, x)
	case classad.RealValue:
		f, err := val.RealValue()
		if err != nil {
			return false
		}
		v.SetReal(i, f)
	case classad.BooleanValue:
		b, err := val.BoolValue()
		if err != nil {
			return false
		}
		v.SetBool(i, b)
	case classad.UndefinedValue:
		v.St[i] = VsUndef
	case classad.ErrorValue:
		v.St[i] = VsError
	default:
		return false
	}
	return true
}

// ColumnSource supplies a batch of values for an attribute reference. The collections package
// implements it over a columnar block; anything that can produce n values per attribute can.
//
// ok=false means the reference cannot be served as a vector -- a string column, or an attribute whose
// values are expressions -- and the evaluation declines as a whole.
type ColumnSource interface {
	LoadColumn(name string, scope ast.AttributeScope, dst Vec) bool
}

// VecEval executes the query over a batch of n records and returns the result vector. A record matches
// a constraint when its element is VsBool with I == 1; see Vec.IsTrue.
//
// ok=false when the program uses something this executor does not implement. It never returns a
// partial answer.
func (q *Query) VecEval(src ColumnSource, n int, scratch *VecScratch) (Vec, bool) {
	if q == nil || q.prog == nil {
		return Vec{}, false
	}
	if len(q.prog.nodes) != 0 {
		return Vec{}, false // a delegated subtree needs a real per-record scope
	}
	if scratch == nil {
		scratch = &VecScratch{}
	}
	return execVec(q.prog, scratch.evaluator(), src, n, scratch)
}

func execVec(p *Program, ev *classad.Evaluator, src ColumnSource, n int, s *VecScratch) (Vec, bool) {
	s.reset(n)
	for ip := 0; ip < len(p.code); {
		in := p.code[ip]
		switch in.Op {
		case OpPushConst:
			if !broadcast(s.push(), p.consts[in.A]) {
				return Vec{}, false
			}
		case OpPushTrue:
			fillBool(s.push(), true)
		case OpPushFalse:
			fillBool(s.push(), false)
		case OpPushUndef:
			fillState(s.push(), VsUndef)
		case OpPushError:
			fillState(s.push(), VsError)
		case OpLoadRef:
			r := p.refs[in.A]
			if !src.LoadColumn(r.name, r.scope, s.push()) {
				return Vec{}, false
			}
		case OpBinop:
			right, left := s.pop(), s.top()
			if !binopVec(ev, p.ops[in.A], left, right) {
				return Vec{}, false
			}
		case OpUnop:
			if !unopVec(ev, p.ops[in.A], s.top()) {
				return Vec{}, false
			}
		case OpShortAnd, OpShortOr:
			// No-op: see the file comment. The combine below does the work.
		case OpCombineAnd:
			right, left := s.pop(), s.top()
			if !logicalVec(ev, "&&", left, right) {
				return Vec{}, false
			}
		case OpCombineOr:
			right, left := s.pop(), s.top()
			if !logicalVec(ev, "||", left, right) {
				return Vec{}, false
			}
		default:
			return Vec{}, false // OpJmpIfNotUndef (Elvis), OpEvalNode, anything added later
		}
		ip++
	}
	if s.depth != 1 {
		return Vec{}, false
	}
	return s.top(), true
}

// VecScratch holds the vector stack and the evaluator so a scan reuses both across batches instead of
// allocating per block.
type VecScratch struct {
	stack []Vec
	depth int
	n     int
	ev    *classad.Evaluator
}

func (s *VecScratch) evaluator() *classad.Evaluator {
	if s.ev == nil {
		// The vector executor never resolves a reference through the evaluator -- refs come from the
		// ColumnSource -- so an empty scope is enough for the value-operation hooks.
		s.ev = classad.NewEvaluator(nil)
	}
	return s.ev
}

func (s *VecScratch) reset(n int) {
	s.depth, s.n = 0, n
}

func (s *VecScratch) push() Vec {
	if s.depth == len(s.stack) {
		s.stack = append(s.stack, newVec(s.n))
	}
	v := s.stack[s.depth]
	if cap(v.I) < s.n {
		v = newVec(s.n)
		s.stack[s.depth] = v
	}
	v.I, v.St = v.I[:s.n], v.St[:s.n]
	s.stack[s.depth] = v
	s.depth++
	return v
}

func (s *VecScratch) pop() Vec { s.depth--; return s.stack[s.depth] }
func (s *VecScratch) top() Vec { return s.stack[s.depth-1] }

func fillBool(v Vec, b bool) {
	x := int64(0)
	if b {
		x = 1
	}
	for i := range v.St {
		v.St[i], v.I[i] = VsBool, x
	}
}

func fillState(v Vec, st uint8) {
	for i := range v.St {
		v.St[i] = st
	}
}

// broadcast fills a vector with one constant. A string constant declines: `Name == "x"` is a real
// query shape, just not one this executor serves yet.
func broadcast(v Vec, c classad.Value) bool {
	switch c.Type() {
	case classad.IntegerValue:
		x, err := c.IntValue()
		if err != nil {
			return false
		}
		for i := range v.St {
			v.St[i], v.I[i] = VsInt, x
		}
	case classad.RealValue:
		f, err := c.RealValue()
		if err != nil {
			return false
		}
		bits := int64(math.Float64bits(f))
		for i := range v.St {
			v.St[i], v.I[i] = VsReal, bits
		}
	case classad.BooleanValue:
		b, err := c.BoolValue()
		if err != nil {
			return false
		}
		fillBool(v, b)
	case classad.UndefinedValue:
		fillState(v, VsUndef)
	case classad.ErrorValue:
		fillState(v, VsError)
	default:
		return false
	}
	return true
}

// numeric reports whether an element is a number this executor's fast paths can use.
func numeric(st uint8) bool { return st == VsInt || st == VsReal }

// binopVec applies a binary operator elementwise, in place on l.
//
// Only comparisons and the three exact arithmetic operators have fast loops, and only for
// number-OP-number. Division, modulo, the identity operators and every mixed or exceptional element go
// to the hook, so their semantics come from the reference rather than from this file.
func binopVec(ev *classad.Evaluator, op string, l, r Vec) bool {
	switch op {
	case "<", "<=", ">", ">=", "==", "!=":
		return compareVec(ev, op, l, r)
	case "+", "-", "*":
		return arithVec(ev, op, l, r)
	default:
		return hookAll(ev, op, l, r)
	}
}

// compareVec: both integers compare exactly as int64; any real involved compares as float64; anything
// else goes to the hook.
func compareVec(ev *classad.Evaluator, op string, l, r Vec) bool {
	for i := range l.St {
		ls, rs := l.St[i], r.St[i]
		if !numeric(ls) || !numeric(rs) {
			if !hookOne(ev, op, l, r, i) {
				return false
			}
			continue
		}
		var res bool
		if ls == VsInt && rs == VsInt {
			a, b := l.I[i], r.I[i]
			switch op {
			case "<":
				res = a < b
			case "<=":
				res = a <= b
			case ">":
				res = a > b
			case ">=":
				res = a >= b
			case "==":
				res = a == b
			case "!=":
				res = a != b
			}
		} else {
			a, b := l.Float(i), r.Float(i)
			switch op {
			case "<":
				res = a < b
			case "<=":
				res = a <= b
			case ">":
				res = a > b
			case ">=":
				res = a >= b
			case "==":
				res = a == b
			case "!=":
				res = a != b
			}
		}
		l.SetBool(i, res)
	}
	return true
}

// arithVec handles +, -, * for number-OP-number. Integer pairs stay integral, which keeps values above
// 2^53 exact and keeps the result's TYPE right for whatever consumes it. Overflow is delegated so the
// reference decides what it means.
func arithVec(ev *classad.Evaluator, op string, l, r Vec) bool {
	for i := range l.St {
		ls, rs := l.St[i], r.St[i]
		if !numeric(ls) || !numeric(rs) {
			if !hookOne(ev, op, l, r, i) {
				return false
			}
			continue
		}
		if ls == VsInt && rs == VsInt {
			a, b := l.I[i], r.I[i]
			var x int64
			var over bool
			switch op {
			case "+":
				x = a + b
				over = (x > a) != (b > 0)
			case "-":
				x = a - b
				over = (x < a) != (b > 0)
			case "*":
				x = a * b
				over = a != 0 && (x/a != b || (a == -1 && b == math.MinInt64))
			}
			if over {
				if !hookOne(ev, op, l, r, i) {
					return false
				}
				continue
			}
			l.SetInt(i, x)
			continue
		}
		a, b := l.Float(i), r.Float(i)
		var f float64
		switch op {
		case "+":
			f = a + b
		case "-":
			f = a - b
		case "*":
			f = a * b
		}
		l.SetReal(i, f)
	}
	return true
}

func unopVec(ev *classad.Evaluator, op string, v Vec) bool {
	for i := range v.St {
		switch {
		case op == "!" && v.St[i] == VsBool:
			v.SetBool(i, v.I[i] == 0)
		case op == "-" && v.St[i] == VsInt:
			if v.I[i] == math.MinInt64 {
				if !hookOneUn(ev, op, v, i) {
					return false
				}
				continue
			}
			v.SetInt(i, -v.I[i])
		case op == "-" && v.St[i] == VsReal:
			v.SetReal(i, -v.Float(i))
		default:
			if !hookOneUn(ev, op, v, i) {
				return false
			}
		}
	}
	return true
}

// logicalVec is && / ||. Both operands boolean is the dominant case and the only one with a fast path;
// everything else -- FALSE dominating an ERROR, UNDEFINED propagation, a non-boolean operand -- is the
// reference's answer, via the same hook the scalar interpreter calls for these opcodes.
func logicalVec(ev *classad.Evaluator, op string, l, r Vec) bool {
	and := op == "&&"
	for i := range l.St {
		if l.St[i] != VsBool || r.St[i] != VsBool {
			if !hookOne(ev, op, l, r, i) {
				return false
			}
			continue
		}
		a, b := l.I[i] != 0, r.I[i] != 0
		if and {
			l.SetBool(i, a && b)
		} else {
			l.SetBool(i, a || b)
		}
	}
	return true
}

// hookOne computes element i through the parity hook.
func hookOne(ev *classad.Evaluator, op string, l, r Vec, i int) bool {
	return l.setValue(i, ev.ApplyBinaryOp(op, l.value(i), r.value(i)))
}

func hookOneUn(ev *classad.Evaluator, op string, v Vec, i int) bool {
	return v.setValue(i, ev.ApplyUnaryOp(op, v.value(i)))
}

// hookAll routes an entire operator through the hook: correct for anything, fast for nothing. Division,
// modulo and the identity operators land here, so adding a fast path for one later is an optimization
// rather than a semantics change.
func hookAll(ev *classad.Evaluator, op string, l, r Vec) bool {
	for i := range l.St {
		if !hookOne(ev, op, l, r, i) {
			return false
		}
	}
	return true
}

// IsTrue reports whether element i is TRUE, which is what counting matches needs. UNDEFINED and ERROR
// are not matches.
func (v Vec) IsTrue(i int) bool { return v.St[i] == VsBool && v.I[i] == 1 }
