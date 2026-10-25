package vm

import (
	"math"
	"math/bits"
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// member(x, {c1, c2, ...}) and identicalMember(x, {...}) over a list of CONSTANTS, compiled to one
// set lookup instead of a delegated function call.
//
// Delegated, the call rebuilds the list value and compares x against every element, per record: a
// 300-element list is 300 boxed comparisons for each record a scan visits. The list is fixed at
// compile time, so the comparisons can be decided once, into hash sets keyed the way the comparison
// itself keys values, and each record pays one or two lookups.
//
// THE SEMANTICS BEING REPRODUCED (the reference engine's testMember, which the tree-walker matches):
//
//	member:          x UNDEFINED -> UNDEFINED; x ERROR, a list or an ad -> ERROR; otherwise TRUE when
//	                 some element compares == to x, else FALSE. An element whose comparison is not
//	                 TRUE -- UNDEFINED, ERROR, a string against a number -- is skipped, not propagated,
//	                 so the result is never UNDEFINED or ERROR because of the list's contents.
//	identicalMember: x a list or an ad -> ERROR; otherwise TRUE when some element is =?= x, else
//	                 FALSE. =?= is total, so an UNDEFINED or ERROR x is looked for like any value.
//
// == coerces across the numeric types (a boolean is 0 or 1, an integer meets a real as a double) and
// folds ASCII case for strings; =?= coerces nothing and folds nothing. The sets below are keyed so a
// lookup answers exactly those comparisons -- see memberSet.
//
// Only a list whose every element is a constant compiles this way (constElem). Anything else -- an
// attribute reference in the list, a list held in an attribute, a nested list -- stays a delegated call.

// memberSet is a compiled constant list. Read-only after construction, so one Program's sets serve
// every concurrent evaluation of it.
type memberSet struct {
	identical bool

	// ints holds the integer elements; for member also the booleans, as 0 and 1, since == promotes a
	// boolean to an integer before comparing.
	ints map[int64]struct{}
	// reals holds the real elements, NaN excluded: a NaN is == and =?= to nothing, itself included.
	reals map[float64]struct{}
	// intReals holds float64(i) for every integer (and, for member, boolean) element, so a REAL x finds
	// an integer element the way == finds it -- by converting the integer to a double. Two integers past
	// 2^53 may share one double; == cannot tell them apart from a real either, so neither does this.
	// Unused for identicalMember, where an integer is never identical to a real.
	intReals map[float64]struct{}
	// strs holds the string elements: ASCII-folded for member (== folds case), exact for identicalMember.
	strs map[string]struct{}

	// The identicalMember-only cases: =?= finds a boolean only among booleans (true =?= 1 is FALSE),
	// and finds UNDEFINED and ERROR, which == never does.
	boolTrue, boolFalse bool
	hasUndef, hasError  bool
}

// isMemberFunc reports whether name is member or identicalMember (case-insensitively, as every
// function name is), and which.
func isMemberFunc(name string) (identical, ok bool) {
	switch {
	case strings.EqualFold(name, "member"):
		return false, true
	case strings.EqualFold(name, "identicalMember"):
		return true, true
	}
	return false, false
}

// constListOf returns the values of a member call's list argument when every element is a constant,
// with ok=false for anything else. The list must be a literal: a list read from an attribute differs
// per record.
func constListOf(e ast.Expr) ([]classad.Value, bool) {
	l, ok := unparen(e).(*ast.ListLiteral)
	if !ok {
		return nil, false
	}
	// The elements read no attribute, so any scope will do for evaluating them.
	ev := classad.NewEvaluator(nil)
	out := make([]classad.Value, 0, len(l.Elements))
	for _, el := range l.Elements {
		v, ok := constElem(ev, el)
		if !ok {
			return nil, false
		}
		out = append(out, v)
	}
	return out, true
}

// constElem evaluates a list element that is a CONSTANT: a scalar literal, possibly parenthesized and
// under unary operators (`-1` parses as unary minus applied to 1). Those are the only forms accepted,
// because they are the ones whose value cannot depend on the record, the clock, or anything else, so
// evaluating them once here gives the value the tree-walker computes per record. ok=false for anything
// else, and for a constant whose value is not a scalar.
func constElem(ev *classad.Evaluator, e ast.Expr) (classad.Value, bool) {
	if !isConstScalarExpr(e) {
		return classad.Value{}, false
	}
	v := ev.Evaluate(e)
	switch v.Type() {
	case classad.IntegerValue, classad.RealValue, classad.StringValue, classad.BooleanValue,
		classad.UndefinedValue, classad.ErrorValue:
		return v, true
	}
	return classad.Value{}, false
}

// isConstScalarExpr reports whether e is built only from scalar literals, parentheses and unary
// operators.
func isConstScalarExpr(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.IntegerLiteral, *ast.RealLiteral, *ast.StringLiteral, *ast.BooleanLiteral,
		*ast.UndefinedLiteral, *ast.ErrorLiteral:
		return true
	case *ast.ParenExpr:
		return v.Inner != nil && isConstScalarExpr(v.Inner)
	case *ast.UnaryOp:
		return isConstScalarExpr(v.Expr)
	}
	return false
}

// newMemberSet compiles a constant list for member (identical=false) or identicalMember.
func newMemberSet(identical bool, elems []classad.Value) *memberSet {
	s := &memberSet{
		identical: identical,
		ints:      map[int64]struct{}{},
		reals:     map[float64]struct{}{},
		intReals:  map[float64]struct{}{},
		strs:      map[string]struct{}{},
	}
	addInt := func(i int64) {
		s.ints[i] = struct{}{}
		if !identical {
			s.intReals[float64(i)] = struct{}{}
		}
	}
	// Each accessor below is called only on a value of its own type (the switch), so its error is
	// unreachable; checking it rather than discarding it costs nothing at compile time.
	for _, v := range elems {
		switch v.Type() {
		case classad.IntegerValue:
			if i, err := v.IntValue(); err == nil {
				addInt(i)
			}
		case classad.RealValue:
			if r, err := v.RealValue(); err == nil && !math.IsNaN(r) {
				s.reals[r] = struct{}{}
			}
		case classad.BooleanValue:
			b, err := v.BoolValue()
			switch {
			case err != nil:
			case identical && b:
				s.boolTrue = true
			case identical:
				s.boolFalse = true
			case b:
				addInt(1)
			default:
				addInt(0)
			}
		case classad.StringValue:
			str, err := v.StringValue()
			if err != nil {
				continue
			}
			if !identical {
				str = foldASCII(str)
			}
			s.strs[str] = struct{}{}
		case classad.UndefinedValue:
			s.hasUndef = true // member: UNDEFINED == x is never TRUE, so only =?= reads it
		case classad.ErrorValue:
			s.hasError = true // likewise
		}
	}
	return s
}

// foldASCII lowers ASCII letters only, which is the fold every ClassAd comparison operator applies
// (classad.CompareStringsFold); strings.ToLower would also fold non-ASCII letters that == keeps apart.
func foldASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; 'A' <= c && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if c := b[j]; 'A' <= c && c <= 'Z' {
					b[j] = c + ('a' - 'A')
				}
			}
			return string(b)
		}
	}
	return s
}

// hasStr reports whether string x is in the set, folding it first for member. The fold is done into a
// stack buffer for the common short string, and the map is indexed with a []byte conversion the
// compiler does not allocate for, so a lookup allocates nothing.
func (s *memberSet) hasStr(x string) bool {
	if s.identical {
		_, ok := s.strs[x]
		return ok
	}
	upper := -1
	for i := 0; i < len(x); i++ {
		if c := x[i]; 'A' <= c && c <= 'Z' {
			upper = i
			break
		}
	}
	if upper < 0 {
		_, ok := s.strs[x]
		return ok
	}
	var stack [64]byte
	var b []byte
	if len(x) <= len(stack) {
		b = stack[:len(x)]
	} else {
		b = make([]byte, len(x))
	}
	copy(b, x)
	for i := upper; i < len(b); i++ {
		if c := b[i]; 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	_, ok := s.strs[string(b)]
	return ok
}

// hasInt reports whether an INTEGER x is in the set: an integer element equal to it, or -- for
// member, where == compares an integer with a real as doubles -- a real element equal to float64(x).
func (s *memberSet) hasInt(x int64) bool {
	if _, ok := s.ints[x]; ok {
		return true
	}
	if s.identical || len(s.reals) == 0 {
		return false
	}
	_, ok := s.reals[float64(x)]
	return ok
}

// hasReal reports whether a REAL x is in the set: a real element equal to it, or -- for member -- an
// integer or boolean element whose double equals it. A NaN is equal to nothing.
func (s *memberSet) hasReal(x float64) bool {
	if _, ok := s.reals[x]; ok {
		return true // a NaN key is never found, so NaN falls through to false
	}
	if s.identical || len(s.intReals) == 0 {
		return false
	}
	_, ok := s.intReals[x]
	return ok
}

// hasBool reports whether a BOOLEAN x is in the set. For member == promotes it to 0 or 1, so it is
// found among the integers (and, as 0.0 or 1.0, the reals); for identicalMember only among booleans.
func (s *memberSet) hasBool(b bool) bool {
	if s.identical {
		if b {
			return s.boolTrue
		}
		return s.boolFalse
	}
	if b {
		return s.hasInt(1)
	}
	return s.hasInt(0)
}

// eval is the call's value for an already-evaluated first argument.
//
// The type is switched on first and only the matching accessor called, which cannot fail; trying the
// accessors in turn instead would build an error value for every type x is not, per record.
func (s *memberSet) eval(x classad.Value) classad.Value {
	switch x.Type() {
	case classad.IntegerValue:
		if i, err := x.IntValue(); err == nil {
			return classad.NewBoolValue(s.hasInt(i))
		}
	case classad.RealValue:
		if r, err := x.RealValue(); err == nil {
			return classad.NewBoolValue(s.hasReal(r))
		}
	case classad.StringValue:
		if str, err := x.StringValue(); err == nil {
			return classad.NewBoolValue(s.hasStr(str))
		}
	case classad.BooleanValue:
		if b, err := x.BoolValue(); err == nil {
			return classad.NewBoolValue(s.hasBool(b))
		}
	case classad.UndefinedValue:
		if s.identical {
			return classad.NewBoolValue(s.hasUndef)
		}
		return classad.NewUndefinedValue()
	case classad.ErrorValue:
		if s.identical {
			return classad.NewBoolValue(s.hasError)
		}
		return classad.NewErrorValue()
	}
	// A list or a nested ad cannot be a member of anything.
	return classad.NewErrorValue()
}

// emitMember compiles a member/identicalMember call over a constant list to its first argument plus
// one OpMember, and reports whether it did. A call it does not recognize is left to the caller, which
// delegates it.
func (c *compiler) emitMember(fc *ast.FunctionCall) bool {
	identical, ok := isMemberFunc(fc.Name)
	if !ok || len(fc.Args) != 2 {
		return false // the wrong arity is an error the delegated call already gets right
	}
	elems, ok := constListOf(fc.Args[1])
	if !ok {
		return false
	}
	c.emit(fc.Args[0])
	c.p.members = append(c.p.members, newMemberSet(identical, elems))
	c.ins(OpMember, int32(len(c.p.members)-1))
	return true
}

// memberVec is OpMember over a batch: v (the first argument's vector) is replaced, in place, by the
// call's result in MASK form.
//
// Numbers, booleans and plain strings are one set lookup per element, the same lookups eval makes. A
// DICTIONARY-ENCODED string column is decided per distinct value instead: the codes a block's
// dictionary assigns to set members are marked once, in a bitset over the dictionary, and each record
// is then a bit test on its code -- the strings themselves are never compared per record.
func memberVec(s *memberSet, v *Vec, sc *VecScratch) bool {
	v.ensureInts()
	v.toData()
	if v.Const {
		st, ok := valueToMaskState(s.eval(v.valueData(0)))
		if !ok {
			return false
		}
		fillMaskConst(v, st)
		return true
	}
	var codes []uint64
	if v.Dict != nil {
		codes = sc.memberCodes(s, v.Dict)
	}
	undefSt, errSt := mUndef, mError
	if s.identical {
		undefSt, errSt = boolMask(s.hasUndef), boolMask(s.hasError)
	}
	for w := range v.Hi {
		base := w * 64
		end := base + 64
		if end > v.n {
			end = v.n
		}
		var hi, lo uint64
		for i := base; i < end; i++ {
			var st int
			switch v.St[i] {
			case VsInt:
				st = boolMask(s.hasInt(v.I[i]))
			case VsReal:
				st = boolMask(s.hasReal(math.Float64frombits(uint64(v.I[i]))))
			case VsBool:
				st = boolMask(s.hasBool(v.I[i] != 0))
			case VsString:
				st = boolMask(s.hasStr(v.S[i]))
			case VsDictCode:
				if code := v.I[i]; codes != nil && code >= 0 && int(code) < len(codes)*64 {
					st = boolMask(codes[code/64]&(1<<uint(code%64)) != 0)
				} else {
					// An unresolvable code reads as whatever valueData makes of it; let eval decide.
					var ok bool
					if st, ok = valueToMaskState(s.eval(v.valueData(i))); !ok {
						return false
					}
				}
			case VsUndef:
				st = undefSt
			default: // VsError
				st = errSt
			}
			b := uint(i - base)
			hi |= uint64(st>>1) << b
			lo |= uint64(st&1) << b
		}
		v.Hi[w], v.Lo[w] = hi, lo
	}
	v.Mask = true
	return true
}

// fillMaskConst sets every element of v to mask state st.
func fillMaskConst(v *Vec, st int) {
	hi, lo := uint64(0), uint64(0)
	if st&2 != 0 {
		hi = ^uint64(0)
	}
	if st&1 != 0 {
		lo = ^uint64(0)
	}
	for w := range v.Hi {
		v.Hi[w], v.Lo[w] = hi, lo
	}
	// Bits past n must be clear, or a popcount over the tail counts records that do not exist.
	if tail := uint(v.n % 64); tail != 0 && len(v.Hi) > 0 {
		m := (uint64(1) << tail) - 1
		v.Hi[len(v.Hi)-1] &= m
		v.Lo[len(v.Lo)-1] &= m
	}
	v.Const, v.Mask = false, true
}

// memberCodes returns a bitset over d's codes marking those whose string is in s, or nil when d
// cannot be read in full (the caller then resolves each code itself).
//
// Two ways to fill it, chosen by which touches less. Each string element can be located with Range
// -- the dictionary is sorted by the fold comparison, so the entries == to it are one contiguous run,
// found by two binary searches -- or every entry can be looked up in the set. A short list against a
// large dictionary takes the first, a long list against a small one the second; both mark the same
// codes.
func (sc *VecScratch) memberCodes(s *memberSet, d DictResolver) []uint64 {
	n := d.Len()
	words := maskWords(n)
	if cap(sc.codeBits) < words {
		sc.codeBits = make([]uint64, words)
	}
	codes := sc.codeBits[:words]
	clear(codes)
	if len(s.strs) == 0 {
		return codes
	}
	if len(s.strs)*2*bits.Len(uint(n)) < n {
		for lit := range s.strs {
			lo, hi, ok := d.Range(lit)
			if !ok {
				return nil
			}
			for code := lo; code < hi; code++ {
				if s.identical {
					// The run is every entry == to lit; =?= wants the one spelled exactly so.
					if str, ok := d.At(code); !ok || str != lit {
						continue
					}
				}
				codes[code/64] |= 1 << uint(code%64)
			}
		}
		return codes
	}
	for code := 0; code < n; code++ {
		str, ok := d.At(code)
		if !ok {
			return nil
		}
		if s.hasStr(str) {
			codes[code/64] |= 1 << uint(code%64)
		}
	}
	return codes
}
