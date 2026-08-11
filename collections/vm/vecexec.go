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
	VsInt    uint8 = iota // I[i] is the integer
	VsReal                // I[i] is math.Float64bits of the real
	VsBool                // I[i] is 0 or 1
	VsUndef               // UNDEFINED
	VsError               // ERROR
	VsString              // S[i] is the string
)

// Vec is one batch of values. The payload is a single int64 slice because that is how both
// classad.Value and the columnar block already store a real (as IEEE-754 bits), so loading a column
// into a vector is a copy rather than a conversion, and no precision is lost for integers above 2^53.
type Vec struct {
	n int

	// DATA form: one element per record.
	I  []int64
	S  []string
	St []uint8

	// MASK form: three-valued logic as two bitplanes, two bits per record. See vecmask.go.
	Hi, Lo []uint64

	// Mask selects which form holds this vector's value. A comparison consumes data and produces a
	// mask; a logical operator consumes and produces masks; arithmetic consumes and produces data.
	// Conversion in either direction is available (toMask/toData) but only happens where an operator
	// genuinely needs the other form.
	Mask bool
}

// newVec allocates the string payload unconditionally rather than on first use. A numeric-only program
// pays 16 bytes per element for a slice it never reads, which is worth not having a Vec whose payload
// can be missing: LoadColumn receives a Vec by value and could not allocate into its caller's.
// The stacks are pooled across queries, so this is paid once, not per query.
func newVec(n int) *Vec {
	return &Vec{
		n: n,
		I: make([]int64, n), S: make([]string, n), St: make([]uint8, n),
		Hi: make([]uint64, maskWords(n)), Lo: make([]uint64, maskWords(n)),
	}
}

// Float returns element i as a float64 whatever its numeric state.
func (v *Vec) Float(i int) float64 {
	if v.St[i] == VsReal {
		return math.Float64frombits(uint64(v.I[i]))
	}
	return float64(v.I[i])
}

// SetReal/SetInt/SetBool write one element.
func (v Vec) SetReal(i int, f float64)   { v.St[i], v.I[i] = VsReal, int64(math.Float64bits(f)) }
func (v Vec) SetInt(i int, x int64)      { v.St[i], v.I[i] = VsInt, x }
func (v *Vec) SetString(i int, s string) { v.St[i], v.S[i] = VsString, s }

func (v *Vec) SetBool(i int, b bool) {
	v.St[i] = VsBool
	if b {
		v.I[i] = 1
	} else {
		v.I[i] = 0
	}
}

// value boxes element i for the parity hook or a caller, in whichever form the vector holds.
func (v *Vec) value(i int) classad.Value {
	if v.Mask {
		switch maskStateAt(v.Hi, v.Lo, i) {
		case mTrue:
			return classad.NewBoolValue(true)
		case mFalse:
			return classad.NewBoolValue(false)
		case mUndef:
			return classad.NewUndefinedValue()
		default:
			return classad.NewErrorValue()
		}
	}
	return v.valueData(i)
}

// valueData boxes element i from the DATA form, which is what a kernel reads while it is writing the
// mask form of the same vector: the two forms occupy different arrays, so a comparison can read its left
// operand's values while filling its planes.
func (v *Vec) valueData(i int) classad.Value {
	switch v.St[i] {
	case VsInt:
		return classad.NewIntValue(v.I[i])
	case VsReal:
		return classad.NewRealValue(math.Float64frombits(uint64(v.I[i])))
	case VsBool:
		return classad.NewBoolValue(v.I[i] != 0)
	case VsString:
		return classad.NewStringValue(v.S[i])
	case VsUndef:
		return classad.NewUndefinedValue()
	default:
		return classad.NewErrorValue()
	}
}

// setValue unboxes a hook result back into element i. ok=false for a value a vector cannot hold (a
// string, list or nested ad), which makes the whole evaluation decline rather than truncate.
func (v *Vec) setValueData(i int, val classad.Value) bool {
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
	case classad.StringValue:
		str, err := val.StringValue()
		if err != nil {
			return false
		}
		v.SetString(i, str)
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
	LoadColumn(name string, scope ast.AttributeScope, dst *Vec) bool
}

// VecEval executes the query over a batch of n records and returns the result vector. A record matches
// a constraint when its element is VsBool with I == 1; see Vec.IsTrue.
//
// ok=false when the program uses something this executor does not implement. It never returns a
// partial answer.
func (q *Query) VecEval(src ColumnSource, n int, scratch *VecScratch) (*Vec, bool) {
	if q == nil || q.prog == nil {
		return nil, false
	}
	if len(q.prog.nodes) != 0 {
		return nil, false // a delegated subtree needs a real per-record scope
	}
	if scratch == nil {
		scratch = &VecScratch{}
	}
	return execVec(q.prog, scratch.evaluator(), src, n, scratch)
}

func execVec(p *Program, ev *classad.Evaluator, src ColumnSource, n int, s *VecScratch) (*Vec, bool) {
	s.reset(n)
	for ip := 0; ip < len(p.code); {
		in := p.code[ip]
		switch in.Op {
		case OpPushConst:
			if !broadcast(s.push(), p.consts[in.A]) {
				return nil, false
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
				return nil, false
			}
		case OpBinop:
			right, left := s.pop(), s.top()
			if !binopVec(ev, p.ops[in.A], left, right) {
				return nil, false
			}
		case OpUnop:
			if !unopVec(ev, p.ops[in.A], s.top()) {
				return nil, false
			}
		case OpShortAnd, OpShortOr:
			// No-op: see the file comment. The combine below does the work.
		case OpCombineAnd:
			right, left := s.pop(), s.top()
			if !logicalVec("&&", left, right) {
				return nil, false
			}
		case OpCombineOr:
			right, left := s.pop(), s.top()
			if !logicalVec("||", left, right) {
				return nil, false
			}
		default:
			return nil, false // OpJmpIfNotUndef (Elvis), OpEvalNode, anything added later
		}
		ip++
	}
	if s.depth != 1 {
		return nil, false
	}
	return s.top(), true
}

// VecScratch holds the vector stack and the evaluator so a scan reuses both across batches instead of
// allocating per block.
type VecScratch struct {
	stack []*Vec
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

func (s *VecScratch) push() *Vec {
	if s.depth == len(s.stack) {
		s.stack = append(s.stack, newVec(s.n))
	}
	v := s.stack[s.depth]
	if cap(v.I) < s.n || cap(v.Hi) < maskWords(s.n) {
		v = newVec(s.n)
		s.stack[s.depth] = v
	}
	v.n = s.n
	v.I, v.S, v.St = v.I[:s.n], v.S[:s.n], v.St[:s.n]
	v.Hi, v.Lo = v.Hi[:maskWords(s.n)], v.Lo[:maskWords(s.n)]
	v.Mask = false
	s.depth++
	return v
}

func (s *VecScratch) pop() *Vec { s.depth--; return s.stack[s.depth] }
func (s *VecScratch) top() *Vec { return s.stack[s.depth-1] }

func fillBool(v *Vec, b bool) {
	x := int64(0)
	if b {
		x = 1
	}
	for i := range v.St {
		v.St[i], v.I[i] = VsBool, x
	}
}

func fillState(v *Vec, st uint8) {
	for i := range v.St {
		v.St[i] = st
	}
}

// broadcast fills a vector with one constant. A string constant declines: `Name == "x"` is a real
// query shape, just not one this executor serves yet.
func broadcast(v *Vec, c classad.Value) bool {
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
	case classad.StringValue:
		str, err := c.StringValue()
		if err != nil {
			return false
		}
		for i := range v.St {
			v.St[i], v.S[i] = VsString, str
		}
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
func binopVec(ev *classad.Evaluator, op string, l, r *Vec) bool {
	switch op {
	case "<", "<=", ">", ">=", "==", "!=":
		return compareVec(ev, op, l, r)
	case "+", "-", "*":
		return arithVec(ev, op, l, r)
	case "=?=", "=!=":
		return identicalVec(op, l, r)
	default:
		return hookAll(ev, op, l, r)
	}
}
func arithVec(ev *classad.Evaluator, op string, l, r *Vec) bool {
	l.toData()
	r.toData()
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

func unopVec(ev *classad.Evaluator, op string, v *Vec) bool {
	if op == "!" {
		// Truthiness first -- !42 is FALSE and !"x" is an ERROR -- then TRUE and FALSE swap while
		// UNDEFINED and ERROR pass through, which is two operations per 64 records.
		v.toMask()
		planeNot(v.Hi, v.Lo, v.Hi, v.Lo)
		return true
	}
	v.toData()
	for i := 0; i < v.n; i++ {
		switch {
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
func hookOne(ev *classad.Evaluator, op string, l, r *Vec, i int) bool {
	return l.setValueData(i, ev.ApplyBinaryOp(op, l.valueData(i), r.valueData(i)))
}

func hookOneUn(ev *classad.Evaluator, op string, v *Vec, i int) bool {
	return v.setValueData(i, ev.ApplyUnaryOp(op, v.valueData(i)))
}

// hookAll routes an entire operator through the hook: correct for anything, fast for nothing. Division,
// modulo and the identity operators land here, so adding a fast path for one later is an optimization
// rather than a semantics change.
func hookAll(ev *classad.Evaluator, op string, l, r *Vec) bool {
	l.toData()
	r.toData()
	for i := range l.St {
		if !hookOne(ev, op, l, r, i) {
			return false
		}
	}
	return true
}

// IsTrue reports whether element i is TRUE, which is what counting matches needs. UNDEFINED and ERROR
// are not matches.
// IsTrue reports whether element i is strictly boolean TRUE, which is what counting matches needs.
//
// This is NOT the truthiness used for a logical OPERAND. A constraint whose value is the number 42 does
// not match -- the store's isTrueValue takes a boolean or nothing -- while `42 && true` is TRUE because
// numbers are truthy as operands. Two different notions of truth, and conflating them is a silent
// wrong-answer bug, so they are separate functions: this one, and dataStateToMask.
func (v *Vec) IsTrue(i int) bool {
	if v.Mask {
		return maskStateAt(v.Hi, v.Lo, i) == mTrue
	}
	return v.St[i] == VsBool && v.I[i] == 1
}

// CountTrue counts records that are TRUE and whose bit is set in live.
//
// In mask form that is one popcount per 64 records. In data form -- a constraint whose value is a number
// or a string, which never matches -- it walks elements, because such a constraint is rare and being
// right about it matters more than being quick.
func (v *Vec) CountTrue(live []uint64) int {
	if v.Mask {
		return planeCountTrue(v.Hi, v.Lo, live)
	}
	n := 0
	for i := 0; i < v.n; i++ {
		if live[i/64]&(1<<uint(i%64)) != 0 && v.IsTrue(i) {
			n++
		}
	}
	return n
}

// toMask converts the vector in place to mask form, applying ClassAd's logical-operand truthiness. A
// no-op when it is already a mask.
func (v *Vec) toMask() {
	if v.Mask {
		return
	}
	for w := range v.Hi {
		var hi, lo uint64
		base := w * 64
		end := base + 64
		if end > v.n {
			end = v.n
		}
		for i := base; i < end; i++ {
			st := dataStateToMask(v.St[i], v.I[i])
			b := uint(i - base)
			hi |= uint64(st>>1) << b
			lo |= uint64(st&1) << b
		}
		v.Hi[w], v.Lo[w] = hi, lo
	}
	v.Mask = true
}

// toData converts the vector in place to data form, so an arithmetic or comparison kernel can consume a
// boolean produced upstream. A no-op when it is already data.
func (v *Vec) toData() {
	if !v.Mask {
		return
	}
	for i := 0; i < v.n; i++ {
		switch maskStateAt(v.Hi, v.Lo, i) {
		case mTrue:
			v.St[i], v.I[i] = VsBool, 1
		case mFalse:
			v.St[i], v.I[i] = VsBool, 0
		case mUndef:
			v.St[i] = VsUndef
		default:
			v.St[i] = VsError
		}
	}
	v.Mask = false
}

// Comparison opcodes, resolved from the operator string ONCE per batch rather than per element. The
// string switch was per-element work that the word loops below would otherwise repeat 8192 times, and
// resolving it up front is also what a SIMD version needs: one kernel selected, then a uniform loop.
type cmpOp uint8

const (
	cmpLT cmpOp = iota
	cmpLE
	cmpGT
	cmpGE
	cmpEQ
	cmpNE
)

func parseCmpOp(op string) (cmpOp, bool) {
	switch op {
	case "<":
		return cmpLT, true
	case "<=":
		return cmpLE, true
	case ">":
		return cmpGT, true
	case ">=":
		return cmpGE, true
	case "==":
		return cmpEQ, true
	case "!=":
		return cmpNE, true
	}
	return 0, false
}

func cmpInt(c cmpOp, a, b int64) bool {
	switch c {
	case cmpLT:
		return a < b
	case cmpLE:
		return a <= b
	case cmpGT:
		return a > b
	case cmpGE:
		return a >= b
	case cmpEQ:
		return a == b
	}
	return a != b
}

func cmpFloat(c cmpOp, a, b float64) bool {
	switch c {
	case cmpLT:
		return a < b
	case cmpLE:
		return a <= b
	case cmpGT:
		return a > b
	case cmpGE:
		return a >= b
	case cmpEQ:
		return a == b
	}
	return a != b
}

func cmpOrder(c cmpOp, o int) bool {
	switch c {
	case cmpLT:
		return o < 0
	case cmpLE:
		return o <= 0
	case cmpGT:
		return o > 0
	case cmpGE:
		return o >= 0
	case cmpEQ:
		return o == 0
	}
	return o != 0
}

func boolMask(b bool) int {
	if b {
		return mTrue
	}
	return mFalse
}

// valueToMaskState maps a hook result to a mask state. ok=false for a value a mask cannot hold, which
// declines the evaluation rather than truncating it -- a comparison never yields a number, so this only
// fires if the reference grows a case this executor has not been taught.
func valueToMaskState(v classad.Value) (int, bool) {
	switch v.Type() {
	case classad.BooleanValue:
		b, err := v.BoolValue()
		if err != nil {
			return 0, false
		}
		return boolMask(b), true
	case classad.UndefinedValue:
		return mUndef, true
	case classad.ErrorValue:
		return mError, true
	}
	return 0, false
}

// compareVec applies a comparison elementwise and writes the result as a MASK, a word at a time.
//
// WORD AT A TIME is the point, not a detail. Each output word is built in a register and stored once, so
// there is no read-modify-write per element and no stale tail; the inner loop over 64 records is exactly
// what a SIMD version replaces with eight Int64x8.Greater calls whose masks concatenate into the same
// word. Writing the result as two bits rather than nine bytes also removes most of the memory traffic a
// boolean intermediate used to cost.
//
// Reading l's DATA form while writing l's MASK form is safe because they are different arrays.
func compareVec(ev *classad.Evaluator, op string, l, r *Vec) bool {
	c, ok := parseCmpOp(op)
	if !ok {
		return false
	}
	l.toData()
	r.toData()
	for w := range l.Hi {
		base := w * 64
		end := base + 64
		if end > l.n {
			end = l.n
		}
		var hi, lo uint64
		for i := base; i < end; i++ {
			ls, rs := l.St[i], r.St[i]
			var st int
			switch {
			case ls == VsString && rs == VsString:
				// Case-INSENSITIVE, via the evaluator's own comparison function. Every ClassAd
				// comparison operator folds case; only =?= and =!= do not.
				st = boolMask(cmpOrder(c, classad.CompareStringsFold(l.S[i], r.S[i])))
			case ls == VsInt && rs == VsInt:
				st = boolMask(cmpInt(c, l.I[i], r.I[i]))
			case numeric(ls) && numeric(rs):
				st = boolMask(cmpFloat(c, l.Float(i), r.Float(i)))
			default:
				var ok bool
				st, ok = valueToMaskState(ev.ApplyBinaryOp(op, l.valueData(i), r.valueData(i)))
				if !ok {
					return false
				}
			}
			b := uint(i - base)
			hi |= uint64(st>>1) << b
			lo |= uint64(st&1) << b
		}
		l.Hi[w], l.Lo[w] = hi, lo
	}
	l.Mask = true
	return true
}

// identicalVec is =?= / =!=: total, never undefined, and case SENSITIVE for strings. Two elements are
// identical when their ClassAd TYPES match and their payloads match -- so 1 =?= 1.0 is false, which is why
// int and real are distinct states rather than one numeric state.
//
// Hand-written rather than hooked because the rule is a type check plus a payload compare with no coercion
// to get wrong, and because a vector cannot hold the values (lists, nested ads) whose identity the
// reference treats as an error.
func identicalVec(op string, l, r *Vec) bool {
	negate := op == "=!="
	l.toData()
	r.toData()
	for w := range l.Hi {
		base := w * 64
		end := base + 64
		if end > l.n {
			end = l.n
		}
		var lo uint64
		for i := base; i < end; i++ {
			same := l.St[i] == r.St[i]
			if same {
				switch l.St[i] {
				case VsInt, VsReal, VsBool:
					same = l.I[i] == r.I[i]
				case VsString:
					same = l.S[i] == r.S[i]
				}
			}
			if same != negate {
				lo |= 1 << uint(i-base)
			}
		}
		l.Hi[w], l.Lo[w] = 0, lo
	}
	l.Mask = true
	return true
}

// logicalVec is && / ||, over mask planes: a fixed sequence of word-wise ANDs and ORs per 64 records,
// with no branch and no dependence on how many records were UNDEFINED or ERROR.
//
// Operands arrive as masks when they came from a comparison, which is the common case, and are converted
// with ClassAd's operand truthiness otherwise -- numbers are truthy, strings are an error. See
// dataStateToMask, and note that is a DIFFERENT notion of truth from IsTrue.
func logicalVec(op string, l, r *Vec) bool {
	l.toMask()
	r.toMask()
	if op == "&&" {
		planeAnd(l.Hi, l.Lo, r.Hi, r.Lo, l.Hi, l.Lo)
	} else {
		planeOr(l.Hi, l.Lo, r.Hi, r.Lo, l.Hi, l.Lo)
	}
	return true
}

// identicalVec is =?= / =!=: total, never undefined, and case SENSITIVE for strings. Two elements are
// identical when their ClassAd TYPES match and their payloads match -- so 1 =?= 1.0 is false, which is
// why int and real are distinct states rather than one numeric state.
//
// Hand-written rather than hooked because the rule is a type check plus a payload compare, with no
// coercion to get wrong, and because a Vec cannot hold the values (lists, nested ads) whose identity
// the reference treats as an error.
func identicalVecOLD(op string, l, r *Vec) bool {
	negate := op == "=!="
	for i := range l.St {
		same := l.St[i] == r.St[i]
		if same {
			switch l.St[i] {
			case VsInt, VsReal, VsBool:
				same = l.I[i] == r.I[i]
			case VsString:
				same = l.S[i] == r.S[i]
			}
		}
		if negate {
			same = !same
		}
		l.SetBool(i, same)
	}
	return true
}

// Release drops the vector stack's references to string payloads and returns the scratch to a state
// safe to keep in a pool.
//
// A string element aliases a block's decompressed string region rather than copying it, so a pooled
// stack would otherwise pin whichever region it last read -- for as long as the pool holds it, and even
// if the table it came from has since been dropped. The regions are heap buffers, so this is a retention
// question rather than the use-after-munmap hazard aliasing mmap'd memory would create, but a few
// thousand pointer writes per query is a cheap way not to have the question at all.
func (s *VecScratch) Release() {
	for i := range s.stack {
		clear(s.stack[i].S)
	}
}
