package vm

import (
	"encoding/binary"
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
// STRINGS are served, in two representations. A plain string element carries the string; a
// dictionary-encoded column carries a CODE into the block's dictionary, and because that dictionary is
// sorted by the evaluator's fold comparison, a comparison against a literal is an integer range test
// over codes with no string comparison at all. Every other string shape -- code against code, code
// against a plain string, either against a number -- resolves the code and compares with the
// evaluator's own fold comparison, or hooks, so the representation never reaches the semantics.
//
// BRANCHES become BLENDS. The Elvis operator and a conditional (ifThenElse) both select per record,
// which a vector cannot do by jumping, so every path is evaluated and the join picks each element
// from the path that record's condition chose. See blendPending.
//
// Evaluating a path the scalar executor would have skipped is safe here, and for the same reason
// OpShortAnd is a no-op above: ClassAd values are TOTAL, so a skipped path still produces a value --
// ERROR, perhaps -- and the blend discards it. The one way a skipped path could differ observably is
// a cyclic reference, which panics rather than producing a value; that cannot arise in this executor
// because OpLoadRef is served by the ColumnSource and never by evaluating an ad attribute's
// expression, and OpEvalNode is excluded by Native(). If a reference here ever resolved through the
// evaluator, this reasoning would fail and both constructs would have to decline again.
//
// NOT HANDLED, returning ok=false so the caller falls back rather than guessing: the magic CurrentTime
// attribute, delegated subtrees (OpEvalNode, which Native() already excludes), and any built-in call
// whose result a vector cannot hold (a list or a nested ad -- see callFuncVec).
//
// FUNCTION CALLS are handled, but elementwise rather than as kernels: see callFuncVec for why that is
// the useful increment and what would change that. member/identicalMember over a constant list is the
// one call that does NOT go that way -- it has a real kernel, memberVec, and the compiler prefers it.

// Element states. A vector carries one per element, which is how three-valued logic and ERROR survive
// vectorization, and how int and real stay distinguishable -- 7/2 is not 7.0/2.0.
const (
	VsInt      uint8 = iota // I[i] is the integer
	VsReal                  // I[i] is math.Float64bits of the real
	VsBool                  // I[i] is 0 or 1
	VsUndef                 // UNDEFINED
	VsError                 // ERROR
	VsString                // S[i] is the string
	VsDictCode              // I[i] is a code into Vec.Dict: a dictionary-encoded STRING
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

	// Raw, when non-nil, is the column in its STORED form: n values of RawWidth bytes, contiguous, as the
	// block holds them. I is NOT populated while it is set.
	//
	// It exists for one reason: lanes come from width. A column fitted to two bytes is 8 lanes of a 128-bit
	// vector against 2 as int64, and widening it at load throws that away before any kernel sees it. So a
	// comparison against a literal can run width-native (see simdCompareRaw) and everything else calls
	// ensureInts, which widens exactly as loadIntBatch used to and clears Raw. A transient optimization that
	// collapses to the ordinary representation on first non-specialized use.
	Raw         []byte
	RawWidth    int
	RawUnsigned bool
	RawReal     bool // a real's slot holds math.Float64bits, so widening it yields VsReal rather than VsInt

	// Dict, when non-nil, resolves VsDictCode elements: the column is a dictionary-encoded string, so I[i]
	// holds a code rather than a value and S[i] is not populated.
	//
	// A comparison against a string LITERAL specializes on this and becomes an integer range test, which is
	// the whole point -- the dictionary is fold-ordered, so the entries matching a literal are contiguous.
	// Everything else stays correct without knowing about it, because valueData resolves a code to its
	// string and the parity hook takes it from there.
	Dict DictResolver

	// Const marks a vector every record of which holds the SAME value, kept once in element 0 with the
	// rest left as garbage. A literal in an expression is the dominant case, and materializing one across
	// a block cost 9.8% of a scan in the profile -- nine bytes per record written per block, to say the
	// same thing 1000 times.
	//
	// Every consumer must either specialize on it (compareVec does, which also tightens its loop by
	// reading a scalar instead of a second array) or call materialize first. valueData and IsTrue read
	// element 0 when it is set, so the parity-hook path and the counters are safe without either.
	Const bool

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
	if v.Const {
		i = 0
	}
	if rawSupported && v.Raw != nil {
		raw := readIntLE(v.Raw[i*v.RawWidth:], v.RawWidth, v.RawUnsigned)
		if v.RawReal {
			return classad.NewRealValue(math.Float64frombits(uint64(raw)))
		}
		return classad.NewIntValue(raw)
	}
	switch v.St[i] {
	case VsInt:
		return classad.NewIntValue(v.I[i])
	case VsReal:
		return classad.NewRealValue(math.Float64frombits(uint64(v.I[i])))
	case VsBool:
		return classad.NewBoolValue(v.I[i] != 0)
	case VsString:
		return classad.NewStringValue(v.S[i])
	case VsDictCode:
		if v.Dict != nil {
			if str, ok := v.Dict.At(int(v.I[i])); ok {
				return classad.NewStringValue(str)
			}
		}
		return classad.NewErrorValue()
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

// DictResolver resolves a dictionary-encoded string column for one block.
//
// Range is what makes a comparison an integer test: because the dictionary is sorted by the evaluator's fold
// comparison, the entries that compare EQUAL to a literal are contiguous, so a match is `lo <= code < hi`
// and an ordering comparison is a test against one boundary. lo == hi means the literal is absent, and the
// caller can conclude that no record matches without reading a code.
type DictResolver interface {
	Range(lit string) (lo, hi int, ok bool)
	At(code int) (string, bool)
	Len() int
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
		// A branching construct converging here blends its paths before this instruction runs.
		if !s.applyBlends(ip) {
			return nil, false
		}
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
			// A column reads an absent CurrentTime as undefined, where the evaluator reads
			// the time; per-record evaluation gets that right.
			if r.curTime || !src.LoadColumn(r.name, r.scope, s.push()) {
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
		case OpMember:
			if !memberVec(p.members[in.A], s.top(), s) {
				return nil, false
			}
		case OpCallFunc:
			if !callFuncVec(ev, p.funcs[in.A], s) {
				return nil, false
			}
		case OpJmpIfNotUndef:
			// The left operand stays on the stack as the blend's selector; the fallback is evaluated
			// unconditionally and the join picks per record.
			if s.depth == 0 {
				return nil, false
			}
			s.pushBlend(blendPending{target: int(in.A), base: s.depth - 1})
		case OpBranchCond:
			if s.depth == 0 {
				return nil, false
			}
			s.pushBlend(blendPending{target: int(p.conds[in.A].end), base: s.depth - 1, cond: true})
		case OpJmp:
			// No-op, like the short-circuit opcodes: every path is evaluated and the join blends them.
		default:
			return nil, false // OpEvalNode, anything added later
		}
		ip++
	}
	// Constructs converging on the end of the program.
	if !s.applyBlends(len(p.code)) {
		return nil, false
	}
	if s.depth != 1 || len(s.blends) != 0 {
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
	// codeBits is OpMember's per-block bitset over a dictionary's codes (see memberCodes).
	codeBits []uint64

	// argbuf is the boxed argument list handed to one built-in call, reused across elements and
	// across batches so an elementwise call does not allocate per record.
	argbuf []classad.Value

	// blends are the branching constructs whose joins have not been reached, innermost last.
	blends []blendPending
}

// blendPending is one branching construct whose paths are being evaluated, waiting for its join.
//
// base is the stack slot holding the construct's SELECTOR -- the Elvis left operand, or the
// conditional's condition -- which is deliberately left on the stack rather than popped. Popping it
// would hand its slot straight back to the next push, because VecScratch recycles by depth, and the
// selector is needed at the join. So the paths' values accumulate above it at known offsets and the
// join reads them from there.
type blendPending struct {
	target int  // the stack-form index this construct converges on
	base   int  // stack slot of the selector; path values follow it
	cond   bool // a conditional (four paths); otherwise Elvis (two)
}

// pushBlend records a construct whose join has not been reached.
func (s *VecScratch) pushBlend(b blendPending) { s.blends = append(s.blends, b) }

// applyBlends resolves every construct that converges on stack-form index ip, innermost first.
//
// Innermost first is required, not merely tidy: with `A ?: (B ?: 7)` both Elvis operators converge on
// the same instruction, and the inner one's result is the outer one's right operand.
func (s *VecScratch) applyBlends(ip int) bool {
	for len(s.blends) > 0 && s.blends[len(s.blends)-1].target == ip {
		b := s.blends[len(s.blends)-1]
		s.blends = s.blends[:len(s.blends)-1]
		if !s.blend(b) {
			return false
		}
	}
	return true
}

// blend computes one construct's result and leaves it in the selector's slot.
//
// The result is built in a freshly pushed vector and swapped into place rather than written over the
// selector, because the selector is still being READ per element -- and for a mask-form selector the
// read is from the bitplanes while the write would be to the data arrays, so writing in place would
// mean a vector that is half one form and half the other.
func (s *VecScratch) blend(b blendPending) bool {
	// How many path values sit above the selector. A conditional pushes four (true, false, undefined,
	// error). Elvis pushes ONE: its left operand is both the selector and the value taken when the
	// selector is defined, so only the fallback is a separate path.
	nPaths := 1
	if b.cond {
		nPaths = 4
	}
	if s.depth != b.base+1+nPaths {
		return false // a path did not leave exactly one value; decline rather than guess
	}
	sel := s.stack[b.base]
	paths := s.stack[b.base+1 : b.base+1+nPaths]
	dst := s.push()
	dst.Dict, dst.Const, dst.Mask = nil, false, false
	for i := 0; i < dst.n; i++ {
		var from *Vec
		if b.cond {
			switch classad.ConditionState(sel.value(i)) {
			case classad.CondTrue:
				from = paths[0]
			case classad.CondFalse:
				from = paths[1]
			case classad.CondUndef:
				from = paths[2]
			default:
				from = paths[3]
			}
		} else if sel.value(i).IsUndefined() {
			from = paths[0] // the Elvis fallback
		} else {
			from = sel // the left operand IS the value when it is defined
		}
		from.toData()
		if !dst.CopyElem(i, from, i) {
			return false
		}
	}
	// Move the result into the selector's slot and drop the paths, swapping pointers so every vector
	// stays in the pool (see callFuncVec).
	s.stack[b.base], s.stack[s.depth-1] = s.stack[s.depth-1], s.stack[b.base]
	s.depth = b.base + 1
	return true
}

// args returns argbuf sized to exactly argc elements.
func (s *VecScratch) args(argc int) []classad.Value {
	if cap(s.argbuf) < argc {
		s.argbuf = make([]classad.Value, argc)
	}
	return s.argbuf[:argc]
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
	s.blends = s.blends[:0]
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
	// Dict as well as Raw: the stack is pooled across queries, so a slot that served a dictionary-encoded
	// column would otherwise still point at that block's dictionary. Harmless today -- a resolver is only
	// consulted for a VsDictCode element and nothing else writes one -- but a stale pointer to another
	// table's dictionary is not a thing to leave lying in a reused buffer.
	v.Mask, v.Const, v.Raw, v.Dict = false, false, nil, nil
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
	v.St[0], v.I[0], v.Const, v.Mask = VsBool, x, true, false
}

func fillState(v *Vec, st uint8) {
	v.St[0], v.Const, v.Mask = st, true, false
}

// broadcast fills a vector with one constant, kept in element 0 (see Vec.Const).
func broadcast(v *Vec, c classad.Value) bool {
	v.Const, v.Mask = true, false
	switch c.Type() {
	case classad.IntegerValue:
		x, err := c.IntValue()
		if err != nil {
			return false
		}
		v.St[0], v.I[0] = VsInt, x
	case classad.RealValue:
		f, err := c.RealValue()
		if err != nil {
			return false
		}
		v.St[0], v.I[0] = VsReal, int64(math.Float64bits(f))
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
		v.St[0], v.S[0] = VsString, str
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
	case opIs, opIsnt:
		return identicalVec(op, l, r)
	default:
		return hookAll(ev, op, l, r)
	}
}
func arithVec(ev *classad.Evaluator, op string, l, r *Vec) bool {
	l.ensureInts()
	r.ensureInts()
	l.toData()
	r.toData()
	l.materialize()
	r.materialize()
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
		// A real result that OVERFLOWS to positive infinity is ERROR in the reference, while -Inf and
		// NaN are ordinary real values (classad's realArithResult). The integer path above already
		// hands its overflow to the hook; this is the same case one type over, and leaving it out
		// served +Inf where the evaluator says error -- found by FuzzVecParity, not by reading.
		if math.IsInf(f, 1) {
			if !hookOne(ev, op, l, r, i) {
				return false
			}
			continue
		}
		l.SetReal(i, f)
	}
	return true
}

// callFuncVec applies a strict built-in to the argc argument vectors on top of the stack, replacing
// them with one result vector.
//
// It is ELEMENTWISE: it boxes each element's arguments and calls classad.ApplyFunction, the same
// entry point the scalar interpreter uses, once per record. There are deliberately no vector kernels
// yet, for two reasons. Parity -- the built-in semantics come from the reference implementation
// rather than from a second copy here, which is the same bargain hookAll strikes for the rare
// operand states, taken for all of them. And value -- what a native call buys is not call speed but
// Native(): with the call delegated the whole query falls off the index probes, the column prefilter
// and this executor onto the per-record path, so an elementwise call already wins the large factor
// and a kernel would win a small one on top of it.
//
// The first kernels worth writing are visible from here. A built-in whose arguments are all Const is
// already called once instead of n times, below. Beyond that, the shape that matters is a Const
// argument that is EXPENSIVE to re-consume per element -- regexp()'s pattern above all, which
// classad compiles with regexp.Compile on every call (functions.go) and so recompiles per record in
// the scalar path too. Hoisting that compile out of the element loop is a bigger win than
// vectorizing any of the arithmetic here, and it is a change to the built-in, not to this file.
//
// ok=false declines the whole evaluation, never a partial answer: the result of a call can be a list
// or a nested ad (split, splitUserName, regexps), which a vector cannot hold, and setValueData says
// so per element.
//
// THE EVALUATOR. classad.ApplyFunction takes one because ten list-consuming built-ins need it to
// materialize a lazy list argument, and the one this executor has is scope-less by construction
// (VecScratch.evaluator: "never resolves a reference through the evaluator"). That is sound here, and
// provably rather than incidentally: an argument element is boxed by valueData, whose codomain is
// int, real, bool, string, UNDEFINED and ERROR -- it cannot produce a list or a nested ad, because a
// Vec has no state that holds one. So the lazy-list path inside those ten is unreachable from here,
// and they return ERROR for a non-list argument exactly as the tree-walker would for the same value.
// If a Vec ever gains a list state, this reasoning fails and the lazy-list argument has to decline.
func callFuncVec(ev *classad.Evaluator, f funcInfo, s *VecScratch) bool {
	base := s.depth - f.argc
	if base < 0 {
		return false
	}
	dst := s.push() // may grow s.stack, so the argument window is taken after it
	args := s.stack[base : base+f.argc]

	// A mask-form argument -- isUndefined(Cpus > 2) -- has its value in the bitplanes, so it has to be
	// converted before valueData can read it. Const is NOT materialized: valueData reads element 0 for
	// a const vector, so an unmaterialized literal is read correctly n times for free.
	allConst := true
	for _, a := range args {
		a.toData()
		if !a.Const {
			allConst = false
		}
	}

	// Every argument const means the result is too: compute it once and leave dst const, which keeps a
	// call on literals -- quantize(4, 8), pow(2, 10) -- off the per-record loop entirely.
	n := dst.n
	if allConst {
		n = 1
	}
	dst.Dict = nil // a call result is never dictionary-coded, whatever its arguments were
	vals := s.args(f.argc)
	for i := 0; i < n; i++ {
		for j, a := range args {
			vals[j] = a.valueData(i)
		}
		if !dst.setValueData(i, classad.ApplyFunction(ev, f.name, vals)) {
			return false
		}
	}
	dst.Const = allConst

	// Move the result into the deepest argument's slot and drop the arguments. Swapping the pointers
	// rather than copying the payload keeps every vector in the pool: the displaced argument vector
	// lands in the slot dst came from and is handed out again by the next push.
	s.stack[base], s.stack[base+f.argc] = s.stack[base+f.argc], s.stack[base]
	s.depth = base + 1
	return true
}

func unopVec(ev *classad.Evaluator, op string, v *Vec) bool {
	v.ensureInts()
	if op == "!" {
		// Truthiness first -- !42 is FALSE and !"x" is an ERROR -- then TRUE and FALSE swap while
		// UNDEFINED and ERROR pass through, which is two operations per 64 records.
		v.toMask()
		planeNot(v.Hi, v.Lo, v.Hi, v.Lo)
		return true
	}
	v.toData()
	v.materialize()
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
	l.ensureInts()
	r.ensureInts()
	l.toData()
	r.toData()
	l.materialize()
	r.materialize()
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
	// A raw numeric column is never a match: the store takes a boolean or nothing (see isTrueValue). Answering
	// from Raw rather than widening first is also what lets loadNum skip filling the state array at all.
	if v.Raw != nil {
		return false
	}
	if v.Const {
		i = 0
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
	if rawSupported && v.Raw != nil {
		return 0 // a constraint whose value is a number never matches; no need to widen to say so
	}
	n := 0
	for i := 0; i < v.n; i++ {
		if live[i/64]&(1<<uint(i%64)) != 0 && v.IsTrue(i) {
			n++
		}
	}
	return n
}

// ensureInts widens a raw column into I, so a kernel that was not written for the stored form can read it.
// A no-op when Raw is nil, which is the common case.
func (v *Vec) ensureInts() {
	if !rawSupported || v.Raw == nil {
		return
	}
	st := uint8(VsInt)
	if v.RawReal {
		st = VsReal
	}
	widenTyped(v.I, v.Raw, v.RawWidth, v.RawUnsigned, v.n)
	for k := 0; k < v.n; k++ {
		v.St[k] = st
	}
	v.Raw = nil
}

// widenTyped widens a stored column into int64, with the width decision hoisted OUT of the loop so each case
// is a single machine load rather than a byte loop plus a branch.
//
// A column is one width for the whole block by construction, and doing this per element instead is what the
// batch-load work removed: reading a value a byte at a time cost 0.69x to 0.84x of a scan when this function
// was first written the obvious way.
func widenTyped(dst []int64, src []byte, width int, unsigned bool, n int) {
	switch {
	case width == 1 && unsigned:
		for k := 0; k < n; k++ {
			dst[k] = int64(src[k])
		}
	case width == 1:
		for k := 0; k < n; k++ {
			dst[k] = int64(int8(src[k]))
		}
	case width == 2 && unsigned:
		for k := 0; k < n; k++ {
			dst[k] = int64(binary.LittleEndian.Uint16(src[k*2:]))
		}
	case width == 2:
		for k := 0; k < n; k++ {
			dst[k] = int64(int16(binary.LittleEndian.Uint16(src[k*2:])))
		}
	case width == 4 && unsigned:
		for k := 0; k < n; k++ {
			dst[k] = int64(binary.LittleEndian.Uint32(src[k*4:]))
		}
	case width == 4:
		for k := 0; k < n; k++ {
			dst[k] = int64(int32(binary.LittleEndian.Uint32(src[k*4:])))
		}
	case width == 8:
		for k := 0; k < n; k++ {
			dst[k] = int64(binary.LittleEndian.Uint64(src[k*8:]))
		}
	default: // a 6-byte or otherwise unusual fitted width
		for k := 0; k < n; k++ {
			dst[k] = readIntLE(src[k*width:], width, unsigned)
		}
	}
}

// readIntLE reads a little-endian integer of the given width, sign-extending unless unsigned. The same
// decode the store does; kept here so ensureInts does not need to call back into it.
func readIntLE(b []byte, width int, unsigned bool) int64 {
	var u uint64
	for i := 0; i < width; i++ {
		u |= uint64(b[i]) << (8 * uint(i))
	}
	if !unsigned && width < 8 {
		if shift := uint(64 - 8*width); u&(1<<(8*uint(width)-1)) != 0 {
			return int64(u<<shift) >> shift
		}
	}
	return int64(u)
}

// materialize expands a Const vector so every element carries the value, for a kernel that does not
// specialize on it.
func (v *Vec) materialize() {
	if !v.Const {
		return
	}
	v.Raw = nil // a Const vector's value is in element 0, not in a column
	v.Const = false
	st, i0, s0 := v.St[0], v.I[0], v.S[0]
	for k := 1; k < v.n; k++ {
		v.St[k], v.I[k], v.S[k] = st, i0, s0
	}
}

// toMask converts the vector in place to mask form, applying ClassAd's logical-operand truthiness. A
// no-op when it is already a mask.
func (v *Vec) toMask() {
	if v.Mask {
		return
	}
	v.ensureInts()
	if v.Const {
		// One state repeated: the planes are all-ones or all-zeros words, so there is nothing to walk.
		fillMaskConst(v, dataStateToMask(v.St[0], v.I[0]))
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

// toData converts the vector in place to data form. It deliberately does NOT materialize a Const vector:
// compareVec calls this and then specializes on Const, so materializing here would silently disable that
// specialization -- which it did, and cost the whole optimization until the benchmark showed no gain.
// Kernels that index elements call materialize themselves.
func (v *Vec) toData() {
	if !v.Mask {
		return
	}
	v.Raw = nil
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
	v.Mask, v.Const = false, false
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

// strState reports whether an element is a STRING, in either representation. A dictionary-encoded
// column carries codes rather than strings, and a kernel that tests for VsString alone silently sends
// every such element to the hook -- correct, and several times the cost of resolving the code.
func strState(st uint8) bool { return st == VsString || st == VsDictCode }

// flipCmp is the operator that compares the same two operands in the opposite order: `lit < col` is
// `col > lit`. Used only where the flipped form is a total kernel over every element, so no hook ever
// sees the swapped argument order.
func flipCmp(c cmpOp) cmpOp {
	switch c {
	case cmpLT:
		return cmpGT
	case cmpLE:
		return cmpGE
	case cmpGT:
		return cmpLT
	case cmpGE:
		return cmpLE
	}
	return c // == and != are symmetric
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
	// A literal on the LEFT against a dictionary-encoded column -- `"user3" == Owner`, which the probe
	// extractor already treats as the same predicate as the other order. Flipping it reaches the integer
	// range test instead of materializing the literal across the block and hooking every element. Tried
	// BEFORE materialize, which would otherwise have already spread the literal over n elements.
	//
	// Safe to flip only because compareVecDict is total: it serves every element or declines the whole
	// column, so no hook is ever handed the operands in the wrong order.
	if l.Const && !r.Const && r.Dict != nil && l.St[0] == VsString {
		r.ensureInts()
		if compareVecDict(flipCmp(c), l, r, l.S[0]) {
			return true
		}
	}
	l.materialize()
	if !r.Const {
		l.ensureInts()
		r.ensureInts()
	}
	if r.Const {
		// WIDTH-NATIVE first: a raw column against an integer literal is the shape SIMD widens, and it is the
		// shape most constraints are. Declining here just widens and falls through.
		if rawSupported && l.Raw != nil && !l.RawReal && r.St[0] == VsInt {
			if simdCompareRaw(c, l, r.I[0]) {
				return true
			}
		}
		l.ensureInts()
		// A dictionary-encoded column against a literal: an integer range test, no string comparison at all.
		if l.Dict != nil && r.St[0] == VsString {
			if ok := compareVecDict(c, l, l, r.S[0]); ok {
				return true
			}
		}
		return compareVecConst(ev, op, c, l, r)
	}
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
			case strState(ls) && strState(rs):
				// At least one side is a dictionary CODE: two dictionary-encoded columns, or one
				// against a plain string. Codes from two columns index two different dictionaries,
				// so they can only be compared once resolved -- but resolving is an indexed read,
				// against boxing both sides for the hook.
				lstr, lok := l.str(i)
				rstr, rok := r.str(i)
				if !lok || !rok {
					var ok bool
					st, ok = valueToMaskState(ev.ApplyBinaryOp(op, l.valueData(i), r.valueData(i)))
					if !ok {
						return false
					}
					break
				}
				st = boolMask(cmpOrder(c, classad.CompareStringsFold(lstr, rstr)))
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

// compareVecConst compares a column against a LITERAL -- `Attr OP constant`, the shape most constraints
// are made of.
//
// Two things it avoids. The literal is never materialized across the block, which the profile put at 9.8%
// of a scan: nine bytes per record written per block to repeat one value. And the loop reads a scalar held
// in registers rather than a second array, so the comparison touches one column's memory instead of two.
// Both are also what a SIMD version wants -- a literal becomes one broadcast into a vector register, not a
// memory fill.
func compareVecConst(ev *classad.Evaluator, op string, c cmpOp, l, r *Vec) bool {
	rst := r.St[0]
	rInt, rFloat, rStr := r.I[0], r.Float(0), r.S[0]
	rIsInt := rst == VsInt
	rIsNum := numeric(rst)
	rIsStr := rst == VsString
	for w := range l.Hi {
		base := w * 64
		end := base + 64
		if end > l.n {
			end = l.n
		}
		var hi, lo uint64
		for i := base; i < end; i++ {
			ls := l.St[i]
			var st int
			switch {
			case rIsInt && ls == VsInt:
				st = boolMask(cmpInt(c, l.I[i], rInt))
			case rIsStr && ls == VsString:
				st = boolMask(cmpOrder(c, classad.CompareStringsFold(l.S[i], rStr)))
			case rIsStr && ls == VsDictCode:
				// A code column the range test could not take: one with undefined elements (a group
				// column's non-members) or with plain strings mixed in (a value repaired from the
				// cold tail). Resolve the code and fold-compare rather than boxing for the hook.
				a, ok := l.str(i)
				if !ok {
					st, ok = valueToMaskState(ev.ApplyBinaryOp(op, l.valueData(i), r.valueData(i)))
					if !ok {
						return false
					}
					break
				}
				st = boolMask(cmpOrder(c, classad.CompareStringsFold(a, rStr)))
			case rIsNum && numeric(ls):
				st = boolMask(cmpFloat(c, l.Float(i), rFloat))
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

// compareVecDict compares a dictionary-encoded string column against a literal as an integer test.
//
// The dictionary is sorted by the evaluator's fold comparison, so the entries equal to lit are the contiguous
// range [lo, hi) and every operator falls out of where a code sits relative to it:
//
//	== lo <= c < hi     != !(lo <= c < hi)     < c < lo     <= c < hi     > c >= hi     >= c >= lo
//
// dst is where the mask lands and col is where the codes come from. They are the same vector in the usual
// `col OP lit` form; they differ when the literal was on the LEFT and the comparison was flipped to get
// here, since the result still has to land on the operand the stack left on top.
//
// ok=false leaves the caller to take the ordinary path -- which happens when the resolver cannot answer, and
// for any element that is not a code (an escaped record's value comes from the cold tail as a real string,
// and a group column's non-members have no value at all).
func compareVecDict(c cmpOp, dst, col *Vec, lit string) bool {
	lo, hi, ok := col.Dict.Range(lit)
	if !ok {
		return false
	}
	for _, st := range col.St {
		if st != VsDictCode {
			return false // a mixed column: let the string path handle all of it rather than half of it
		}
	}
	for w := range dst.Hi {
		base := w * 64
		end := base + 64
		if end > col.n {
			end = col.n
		}
		var lob uint64
		for i := base; i < end; i++ {
			code := int(col.I[i])
			var res bool
			switch c {
			case cmpEQ:
				res = code >= lo && code < hi
			case cmpNE:
				res = code < lo || code >= hi
			case cmpLT:
				res = code < lo
			case cmpLE:
				res = code < hi
			case cmpGT:
				res = code >= hi
			default: // cmpGE
				res = code >= lo
			}
			if res {
				lob |= 1 << uint(i-base)
			}
		}
		dst.Hi[w], dst.Lo[w] = 0, lob
	}
	// Const must be cleared as well as Mask: dst is the LITERAL's vector in the flipped form, and a
	// mask whose Const flag is still set reads element 0's bit for every record.
	dst.Mask, dst.Const = true, false
	return true
}

// identKind maps a state to the ClassAd TYPE identity compares on, collapsing the two representations of a
// string into one.
//
// VsDictCode and VsString are both strings; only the encoding differs. Comparing raw states instead treats a
// dictionary-encoded column and a plain one as different types -- and worse, made two dictionary-encoded
// columns compare EQUAL for every record, because the state bytes matched and neither payload case ran.
// `Owner =?= Iwd` returned 19842 instead of 0.
func identKind(st uint8) uint8 {
	if st == VsDictCode {
		return VsString
	}
	return st
}

// str returns element i as a string, from whichever representation holds it. Codes from two different
// columns index two different dictionaries, so they can only be compared once resolved.
func (v *Vec) str(i int) (string, bool) {
	if v.St[i] == VsDictCode {
		if v.Dict == nil {
			return "", false
		}
		return v.Dict.At(int(v.I[i]))
	}
	return v.S[i], true
}

// The identity operators as the COMPILER spells them. `=?=` and `is` are the same operator and the parser
// normalizes both to "is", so a kernel keyed on the source spelling never runs: this one did not, for as
// long as it has existed, and every identity comparison went to the hook instead. Named constants here and
// asserted against a compiled program in TestVecIdentityOpStrings, so the next rename cannot do it again.
const (
	opIs   = "is"
	opIsnt = "isnt"
)

// identicalVec is =?= / =!=: total, never undefined, and case SENSITIVE for strings. Two elements are
// identical when their ClassAd TYPES match and their payloads match -- so 1 =?= 1.0 is false, which is why
// int and real are distinct states rather than one numeric state.
//
// Hand-written rather than hooked because the rule is a type check plus a payload compare with no coercion
// to get wrong, and because a vector cannot hold the values (lists, nested ads) whose identity the
// reference treats as an error.
func identicalVec(op string, l, r *Vec) bool {
	var negate bool
	switch op {
	case opIs:
	case opIsnt:
		negate = true
	default:
		return false // an operator string this kernel was not written for: decline rather than guess
	}
	l.ensureInts()
	r.ensureInts()
	l.toData()
	r.toData()
	// One side a LITERAL. Identity is symmetric -- `a is b` asks the same question as `b is a` -- so the
	// literal is handled on either side by the same kernel, and the result still lands on l because that
	// is the operand the stack left on top. Worth separating because the alternative is materializing the
	// literal across the block: for a string that is a 16-byte header per record, written to say the same
	// thing n times, and it cost more than the comparison did.
	if l.Const != r.Const {
		col, lit := l, r
		if l.Const {
			col, lit = r, l
		}
		return identicalConst(negate, l, col, lit)
	}
	l.materialize()
	r.materialize()
	for w := range l.Hi {
		base := w * 64
		end := base + 64
		if end > l.n {
			end = l.n
		}
		var lo uint64
		for i := base; i < end; i++ {
			same := identKind(l.St[i]) == identKind(r.St[i])
			if same {
				switch identKind(l.St[i]) {
				case VsInt, VsBool:
					same = l.I[i] == r.I[i]
				case VsReal:
					// By VALUE, not by bits. The reference compares two reals with ==, so 0.0 and
					// -0.0 are identical although their bit patterns differ, and a NaN is identical
					// to nothing although its bits may match. Comparing the payload words instead
					// gets both backwards, which is what this kernel did for as long as nothing
					// reached it.
					same = l.Float(i) == r.Float(i)
				case VsString:
					ls, lok := l.str(i)
					rs, rok := r.str(i)
					same = lok && rok && ls == rs
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

// identicalConst is identicalVec against a literal, read once into registers rather than spread over the
// block. dst is where the mask lands; col and lit are the column and the literal, in whichever order the
// expression wrote them.
func identicalConst(negate bool, dst, col, lit *Vec) bool {
	lk := identKind(lit.St[0])
	lInt, lFloat := lit.I[0], lit.Float(0)
	lStr, lStrOK := lit.str(0)
	for w := range dst.Hi {
		base := w * 64
		end := base + 64
		if end > col.n {
			end = col.n
		}
		var lo uint64
		for i := base; i < end; i++ {
			same := identKind(col.St[i]) == lk
			if same {
				switch lk {
				case VsInt, VsBool:
					same = col.I[i] == lInt
				case VsReal:
					same = col.Float(i) == lFloat
				case VsString:
					cs, cok := col.str(i)
					same = cok && lStrOK && cs == lStr
				}
			}
			if same != negate {
				lo |= 1 << uint(i-base)
			}
		}
		dst.Hi[w], dst.Lo[w] = 0, lo
	}
	dst.Mask, dst.Const = true, false
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

// NewVec allocates a data-form vector of n elements, every one UNDEFINED.
//
// For a caller outside this package that materializes a column the loaders cannot produce -- one
// that exists for only SOME records, so it has to be presented at full length with the rest
// undefined. Undefined rather than zero is the safe start: an element nothing writes then reads as
// "no value here" instead of as the integer 0.
func NewVec(n int) *Vec {
	v := &Vec{n: n, I: make([]int64, n), S: make([]string, n), St: make([]uint8, n)}
	for i := range v.St {
		v.St[i] = VsUndef
	}
	return v
}

// CopyElem copies element j of src into element i of v, preserving its state -- int stays int, real
// stays real, a dictionary code stays a code. Returns false if either index is out of range.
//
// This is the scatter primitive. Boxing through classad.Value would work and is what a caller
// without it must do, but it allocates for strings and loses the dictionary encoding, turning a
// code copy into a string materialization per record.
//
// A src in RAW form is widened first: raw is a stored-width column, and an element of it means
// nothing once separated from its width and signedness.
func (v *Vec) CopyElem(i int, src *Vec, j int) bool {
	if i < 0 || i >= len(v.St) || src == nil || j < 0 {
		return false
	}
	src.ensureInts()
	if src.Const {
		j = 0
	}
	if j >= len(src.St) {
		return false
	}
	v.St[i] = src.St[j]
	v.I[i] = src.I[j]
	if src.St[j] == VsString {
		v.S[i] = src.S[j]
	}
	return true
}

// Len is the vector's element count.
func (v *Vec) Len() int { return v.n }
