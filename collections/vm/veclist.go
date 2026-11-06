package vm

import (
	"encoding/binary"
	"math"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// LIST COLUMNS.
//
// A Vec has no state that holds a list, and callFuncVec's argument boxing relies on that (see its
// comment): every value a vector element can produce is a scalar. So a list-valued attribute is not
// loaded as a vector at all. Instead the executor recognizes the few instruction shapes that CONSUME
// a list attribute immediately and produce a scalar --
//
//	OpLoadRef L, OpCallFunc f           f(L) for f in size, isList, isUndefined, sum, avg, min, max
//	OpLoadRef L, OpCallFunc f           f(x, L) for f in member, identicalMember (x already on the stack)
//	OpLoadRef L, OpPushUndef, OpBinop   L is undefined, L isnt undefined
//
// -- and, when the source can serve L as a ListColumn for this batch, computes the call's result from
// the column directly, never materializing a list in a vector. Anything else, or a source that cannot
// serve the column, takes the ordinary path: LoadColumn, which declines a list attribute, so the block
// goes to the per-record evaluator exactly as before.
//
// PARITY. The fast loops cover the cases whose semantics are one comparison: a list's length, its
// presence, and membership of a constant of the column's own element kind. Every other combination is
// computed by the reference: the record's list is boxed as a list of the literal values the column
// holds and handed to classad.ApplyFunction with the other argument, so UNDEFINED and ERROR arguments,
// cross-kind comparisons and the aggregates' coercions are the evaluator's own. The boxed list holds
// values, never expressions, so the scope-less evaluator is sufficient for it.
//
// THE SHAPE MUST BE A STRAIGHT LINE. If instruction ip+1 is a jump target -- the join of a branch whose
// last path ends in `OpLoadRef L` -- then the call consumes the branch's result, not L, and fusing would
// compute f(L) for records that took the other path. Such shapes are never fused.

// ListColumn is one batch of a list-valued attribute in the Arrow list layout: per record an offsets
// pair into one child array of every element of every record, back to back.
type ListColumn struct {
	N int
	// Present has bit k set when record k holds a list this column carries; a clear bit means the record
	// lacks the attribute (UNDEFINED). A source must not serve a batch in which a record holds the
	// attribute as anything else.
	Present []byte
	// Offs holds N+1 little-endian uint32 offsets: record k's elements are child[Offs[k]:Offs[k+1]].
	Offs []byte
	// Kind is the elements' kind: VsInt, VsReal, VsBool or VsString.
	Kind uint8
	// Fixed holds VsInt and VsReal elements, 8 bytes little-endian each (a real as math.Float64bits).
	Fixed []byte
	// Bits holds VsBool elements, one bit each.
	Bits []byte
	// Dict and Codes hold VsString elements: Codes is CodeWidth bytes little-endian per element, each an
	// index into Dict.
	Dict      []string
	Codes     []byte
	CodeWidth int
	// Valid has bit i set when element i is defined; nil when every element is. An undefined element is
	// the UNDEFINED literal in the list.
	Valid []byte
	// Except holds, by record, the value of the attribute for the records that hold it as something the
	// column cannot carry -- a list mixing kinds, a scalar where the others hold lists -- evaluated alone,
	// as the per-record path evaluates it. Their Present bit is clear and their offsets are flat. The
	// call over such a record is computed by the reference.
	Except map[int]classad.Value
	// Other, when non-nil, has bit k set for each record in Except. For the source's use.
	Other []byte
}

// ListColumnSource is a ColumnSource that can also serve a list-valued attribute as a ListColumn.
// ok=false means it cannot for this batch, and the executor takes the ordinary path.
type ListColumnSource interface {
	LoadListColumn(name string, scope ast.AttributeScope) (*ListColumn, bool)
}

func bitAt(b []byte, i int) bool { return b[i>>3]&(1<<uint(i&7)) != 0 }

// Has reports whether record k holds a list.
func (lc *ListColumn) Has(k int) bool { return bitAt(lc.Present, k) }

// Bounds returns record k's element range in the child array.
func (lc *ListColumn) Bounds(k int) (lo, hi int) {
	return int(binary.LittleEndian.Uint32(lc.Offs[4*k:])), int(binary.LittleEndian.Uint32(lc.Offs[4*k+4:]))
}

func (lc *ListColumn) defined(i int) bool { return lc.Valid == nil || bitAt(lc.Valid, i) }

func (lc *ListColumn) code(i int) int {
	switch lc.CodeWidth {
	case 1:
		return int(lc.Codes[i])
	case 2:
		return int(binary.LittleEndian.Uint16(lc.Codes[2*i:]))
	}
	return int(binary.LittleEndian.Uint32(lc.Codes[4*i:]))
}

// Elem returns element i as a value.
func (lc *ListColumn) Elem(i int) classad.Value {
	if !lc.defined(i) {
		return classad.NewUndefinedValue()
	}
	switch lc.Kind {
	case VsInt:
		return classad.NewIntValue(int64(binary.LittleEndian.Uint64(lc.Fixed[8*i:])))
	case VsReal:
		return classad.NewRealValue(math.Float64frombits(binary.LittleEndian.Uint64(lc.Fixed[8*i:])))
	case VsBool:
		return classad.NewBoolValue(bitAt(lc.Bits, i))
	case VsString:
		return classad.NewStringValue(lc.Dict[lc.code(i)])
	}
	return classad.NewErrorValue()
}

// listFuncs are the built-ins fused with a list load, and how many arguments each takes; the list is
// always the last.
var listFuncs = map[string]int{
	"size": 1, "islist": 1, "isundefined": 1, "sum": 1, "avg": 1, "min": 1, "max": 1,
	"member": 2, "identicalmember": 2,
}

// ListFusedCalls counts calls served from a list column, so a test can see the path ran.
var ListFusedCalls atomic.Int64

// isJumpTarget reports whether some control-flow instruction of p lands on instruction t.
func isJumpTarget(p *Program, t int) bool {
	for _, in := range p.code {
		switch in.Op {
		case OpShortAnd, OpShortOr, OpJmpIfNotUndef, OpJmp, OpJmpVoid:
			if int(in.A) == t {
				return true
			}
		case OpBranchCond, OpBranchVoid:
			ct := p.conds[in.A]
			if int(ct.onFalse) == t || int(ct.onUndef) == t || int(ct.onError) == t || int(ct.end) == t || int(ct.header) == t {
				return true
			}
		}
	}
	return false
}

// listFusion reports how the instructions after the OpLoadRef at ip consume it: the function to apply
// and how many instructions the fused shape spans, or ok=false when they do not form a fusable shape.
func listFusion(p *Program, ip int) (fn string, argc, span int, ok bool) {
	if ip+1 >= len(p.code) {
		return "", 0, 0, false
	}
	switch nx := p.code[ip+1]; nx.Op {
	case OpCallFunc:
		f := p.funcs[nx.A]
		want, known := listFuncs[f.name]
		if !known || want != f.argc {
			return "", 0, 0, false
		}
		if isJumpTarget(p, ip+1) {
			return "", 0, 0, false
		}
		return f.name, f.argc, 2, true
	case OpPushUndef:
		if ip+2 >= len(p.code) || p.code[ip+2].Op != OpBinop {
			return "", 0, 0, false
		}
		op := p.ops[p.code[ip+2].A]
		if op != opIs && op != opIsnt {
			return "", 0, 0, false
		}
		if isJumpTarget(p, ip+1) || isJumpTarget(p, ip+2) {
			return "", 0, 0, false
		}
		return op, 1, 3, true
	}
	return "", 0, 0, false
}

// tryListFused serves the OpLoadRef at ip and the call consuming it from a list column, when the shape
// fuses and the source can serve the column. handled=false leaves everything to the ordinary path;
// handled with ok=false declines the whole evaluation.
func (s *VecScratch) tryListFused(p *Program, ip int, src ColumnSource) (next int, handled, ok bool) {
	ls, isList := src.(ListColumnSource)
	if !isList {
		return 0, false, true
	}
	fn, argc, span, fuse := listFusion(p, ip)
	if !fuse {
		return 0, false, true
	}
	r := p.refs[p.code[ip].A]
	if r.curTime {
		return 0, false, true
	}
	if argc == 2 && s.depth < 1 {
		return 0, false, true
	}
	lc, served := ls.LoadListColumn(r.name, r.scope)
	if !served || lc == nil || lc.N != s.n {
		return 0, false, true
	}
	if !s.listCall(fn, argc, lc) {
		return 0, true, false
	}
	ListFusedCalls.Add(1)
	return ip + span, true, true
}

// listCall computes fn over the list column (and, for a two-argument call, the vector on top of the
// stack), leaving the result where the call's result would be.
func (s *VecScratch) listCall(fn string, argc int, lc *ListColumn) bool {
	var x *Vec
	base := s.depth
	if argc == 2 {
		base = s.depth - 1
		x = s.stack[base]
		x.toData()
	}
	dst := s.push()
	if x != nil {
		x = s.stack[base] // push may have grown the stack
	}
	dst.Dict = nil
	n := s.n
	switch fn {
	case "size":
		for k := 0; k < n; k++ {
			if lc.Has(k) {
				lo, hi := lc.Bounds(k)
				dst.SetInt(k, int64(hi-lo))
			} else {
				dst.St[k] = VsUndef
			}
		}
	case "islist":
		for k := 0; k < n; k++ {
			dst.SetBool(k, lc.Has(k))
		}
	case "isundefined", opIs:
		for k := 0; k < n; k++ {
			dst.SetBool(k, !lc.Has(k))
		}
	case opIsnt:
		for k := 0; k < n; k++ {
			dst.SetBool(k, lc.Has(k))
		}
	case "member", "identicalmember":
		if !s.listMember(fn == "identicalmember", lc, x, dst) {
			return false
		}
	case "sum":
		if lc.Kind == VsInt && lc.Valid == nil {
			for k := 0; k < n; k++ {
				if !lc.Has(k) {
					dst.St[k] = VsUndef
					continue
				}
				lo, hi := lc.Bounds(k)
				var sum int64
				for i := lo; i < hi; i++ {
					sum += int64(binary.LittleEndian.Uint64(lc.Fixed[8*i:]))
				}
				dst.SetInt(k, sum)
			}
			break
		}
		if !s.listGeneric(fn, lc, nil, dst) {
			return false
		}
	default:
		if !s.listGeneric(fn, lc, x, dst) {
			return false
		}
	}
	dst.Const, dst.Mask, dst.Raw = false, false, nil
	if len(lc.Except) > 0 && !s.listExceptions(fn, lc, x, dst) {
		return false
	}
	// The result takes the slot of the deepest argument, as callFuncVec leaves it.
	if argc == 2 {
		s.stack[base], s.stack[base+1] = s.stack[base+1], s.stack[base]
	}
	s.depth = base + 1
	return true
}

// listMember is member / identicalMember of x in each record's list. A constant x of the column's own
// element kind is decided by one comparison per element, and for strings once per dictionary entry;
// anything else goes through the reference per record.
func (s *VecScratch) listMember(identical bool, lc *ListColumn, x, dst *Vec) bool {
	fn := "member"
	if identical {
		fn = "identicalmember"
	}
	if !x.Const {
		return s.listGeneric(fn, lc, x, dst)
	}
	xv := x.valueData(0)
	n := s.n
	hit, ok := memberHit(identical, lc, xv)
	if !ok {
		return s.listGeneric(fn, lc, x, dst)
	}
	for k := 0; k < n; k++ {
		if !lc.Has(k) {
			dst.St[k] = VsUndef // the list argument is UNDEFINED, which both calls return
			continue
		}
		lo, hi := lc.Bounds(k)
		found := false
		for i := lo; i < hi && !found; i++ {
			// An UNDEFINED element is never == to a defined x, nor =?= to one.
			found = lc.defined(i) && hit(i)
		}
		dst.SetBool(k, found)
	}
	return true
}

// memberHit returns the per-element test for a constant x of the column's own element kind, and ok=false
// for any other x, which the reference handles.
func memberHit(identical bool, lc *ListColumn, xv classad.Value) (func(i int) bool, bool) {
	switch {
	case lc.Kind == VsString && xv.IsString():
		str, err := xv.StringValue()
		if err != nil {
			return nil, false
		}
		match := make([]bool, len(lc.Dict))
		for c, e := range lc.Dict {
			if identical {
				match[c] = e == str
			} else {
				match[c] = classad.CompareStringsFold(e, str) == 0
			}
		}
		return func(i int) bool { return match[lc.code(i)] }, true
	case lc.Kind == VsInt && xv.IsInteger():
		want, err := xv.IntValue()
		if err != nil {
			return nil, false
		}
		return func(i int) bool { return int64(binary.LittleEndian.Uint64(lc.Fixed[8*i:])) == want }, true
	case lc.Kind == VsReal && xv.IsReal():
		want, err := xv.RealValue()
		if err != nil {
			return nil, false
		}
		return func(i int) bool { return math.Float64frombits(binary.LittleEndian.Uint64(lc.Fixed[8*i:])) == want }, true
	case lc.Kind == VsBool && xv.IsBool():
		want, err := xv.BoolValue()
		if err != nil {
			return nil, false
		}
		return func(i int) bool { return bitAt(lc.Bits, i) == want }, true
	}
	return nil, false
}

// listGeneric computes fn per record through the reference implementation, boxing each record's list
// from the column. x, when non-nil, is the call's first argument.
func (s *VecScratch) listGeneric(fn string, lc *ListColumn, x, dst *Vec) bool {
	argc := 1
	if x != nil {
		argc = 2
	}
	ev := s.evaluator()
	var elems []classad.Value
	for k := 0; k < s.n; k++ {
		args := s.args(argc)
		if x != nil {
			args[0] = x.valueData(k)
		}
		lv := classad.NewUndefinedValue()
		if lc.Has(k) {
			lo, hi := lc.Bounds(k)
			elems = elems[:0]
			for i := lo; i < hi; i++ {
				elems = append(elems, lc.Elem(i))
			}
			// A fresh slice per record: the list value is handed to the built-in, which must not see
			// the next record's elements through a shared backing array.
			lv = classad.NewListValue(append([]classad.Value(nil), elems...))
		}
		args[argc-1] = lv
		if !dst.setValueData(k, classad.ApplyFunction(ev, fn, args)) {
			return false
		}
	}
	return true
}

// listExceptions overwrites the results of the records the column holds as exceptions, computing each
// through the reference from the value the source evaluated.
func (s *VecScratch) listExceptions(fn string, lc *ListColumn, x, dst *Vec) bool {
	ev := s.evaluator()
	for k, v := range lc.Except {
		if k < 0 || k >= s.n {
			return false
		}
		var r classad.Value
		switch fn {
		case opIs, opIsnt:
			r = ev.ApplyBinaryOp(fn, v, classad.NewUndefinedValue())
		default:
			args := s.args(1)
			if x != nil {
				args = s.args(2)
				args[0] = x.valueData(k)
			}
			args[len(args)-1] = v
			r = classad.ApplyFunction(ev, fn, args)
		}
		if !dst.setValueData(k, r) {
			return false
		}
	}
	return true
}
