package vm

import (
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// stringListMember over a CONSTANT list, compiled to a set lookup.
//
// The reference splits its list argument on every call -- strings.FieldsFunc plus a TrimSpace per
// element, allocating a slice and a string per element (classad's parseStringList) -- and then scans
// the result linearly. For a literal list that is the same work repeated for every ad in a scan, and
// `stringListMember(Owner, "alice,bob,carol")` is a shape real constraints are made of.
//
// So a literal list is split ONCE at compile time into a set, and each record becomes one lookup.
// The split itself comes from classad.SplitStringList rather than being reimplemented here, because
// the delimiter defaulting and the per-element trimming are exactly the kind of detail that drifts.
//
// ONLY the case-SENSITIVE stringListMember is compiled this way. stringListIMember compares with
// strings.EqualFold, which is Unicode case folding and NOT equivalent to keying a map by
// strings.ToLower -- there are characters for which the two disagree. A set lookup can only replace a
// linear scan when it decides exactly what the scan decided, so the case-insensitive variant stays a
// delegated call rather than a nearly-right fast path.

// strListSet is a compiled constant string list. Read-only after construction, so one Program's sets
// serve every concurrent evaluation of it.
type strListSet struct {
	elems map[string]struct{}
}

// newStrListSet builds the set from already-split elements.
func newStrListSet(elems []string) *strListSet {
	s := &strListSet{elems: make(map[string]struct{}, len(elems))}
	for _, e := range elems {
		s.elems[e] = struct{}{}
	}
	return s
}

// lookup answers stringListMember(item, <the constant list>) as two separate outcomes: whether the
// item is in the list, and whether the item was usable at all.
//
// Keeping them separate is what lets the vector kernel write a result directly. A single Value
// return would make every element's answer a boxed value that the kernel then has to unbox, and
// would leave it with an "unrepresentable result" branch that cannot happen -- the answers here are
// exactly a boolean and ERROR, both of which a Vec holds.
//
// The item coercion is the reference's, not a simplification of it: a string is itself, UNDEFINED is
// the EMPTY STRING, and anything else is unusable. (An undefined item does not thereby match
// "a,,b": the split drops empty fields, so that list has two elements and no empty one.) The
// reference's "both arguments undefined is UNDEFINED" rule cannot apply, because this is only
// compiled when the list argument is a string literal, which is never undefined; emitStrListMember
// is what guarantees that.
func (s *strListSet) lookup(item classad.Value) (found, usable bool) {
	str, ok := classad.StringListArg(item)
	if !ok {
		return false, false
	}
	_, found = s.elems[str]
	return found, true
}

// eval is lookup boxed back into a Value, for the scalar and register executors.
func (s *strListSet) eval(item classad.Value) classad.Value {
	found, usable := s.lookup(item)
	if !usable {
		return classad.NewErrorValue()
	}
	return classad.NewBoolValue(found)
}

// constStringOf returns the value of a constant string expression, with ok=false for anything else.
// Parentheses are transparent, as they are everywhere else in the compiler.
func constStringOf(e ast.Expr) (string, bool) {
	for {
		p, isParen := e.(*ast.ParenExpr)
		if !isParen {
			break
		}
		e = p.Inner
	}
	lit, ok := e.(*ast.StringLiteral)
	if !ok {
		return "", false
	}
	return lit.Value, true
}

// emitStrListMember compiles stringListMember over a constant list to its first argument plus one
// OpStrListMember, and reports whether it did.
//
// Declining leaves the call to the generic strict-call path, which is already correct -- it is only
// slower. That matters for the arity check: stringListMember accepts two or three arguments, and a
// call with any other count must be ERROR without its arguments being evaluated, which the delegated
// path gets right and a compiled call could not.
func (c *compiler) emitStrListMember(fc *ast.FunctionCall) bool {
	if !strings.EqualFold(fc.Name, "stringListMember") {
		return false
	}
	if len(fc.Args) != 2 && len(fc.Args) != 3 {
		return false
	}
	list, ok := constStringOf(fc.Args[1])
	if !ok {
		return false
	}
	delim := ""
	if len(fc.Args) == 3 {
		if delim, ok = constStringOf(fc.Args[2]); !ok {
			return false
		}
	}
	c.emit(fc.Args[0])
	c.p.strLists = append(c.p.strLists, newStrListSet(classad.SplitStringList(list, delim)))
	c.ins(OpStrListMember, int32(len(c.p.strLists)-1))
	return true
}

// strListVec is OpStrListMember over a batch: v (the item's vector) is replaced, in place, by the
// call's result.
//
// Elementwise, like callFuncVec and for the same reason -- the set lookup is the win, not the loop
// shape. A dictionary-encoded column could be decided per DISTINCT value instead, marking the codes
// whose string is in the set once per block and testing a bit per record, which is what memberVec
// does; that is a further specialization of this one and not a different mechanism.
func strListVec(s *strListSet, v *Vec) bool {
	v.toData()
	v.ensureInts()
	set := func(i int, found, usable bool) {
		if usable {
			v.SetBool(i, found)
			return
		}
		v.St[i] = VsError
	}
	if v.Const {
		// Every record holds the same item, so the answer is the same for all of them and element 0
		// stands for the batch.
		found, usable := s.lookup(v.valueData(0))
		set(0, found, usable)
		v.Const, v.Dict = true, nil
		return true
	}
	// valueData resolves a dictionary code to its string, so a coded column is handled without the
	// result carrying the dictionary over.
	for i := 0; i < v.n; i++ {
		found, usable := s.lookup(v.valueData(i))
		set(i, found, usable)
	}
	v.Dict = nil
	return true
}
