package vm

// Cell reads element i in the form the vector holds it, without boxing it into a classad.Value.
//
// It exists for a VALUE producer rather than for a predicate. Every other reader of a vector is
// deciding a comparison, so it specializes on Raw, Const and Dict and never needs the element
// itself; valueData is the one that does need it and it boxes, which for a producer is an
// allocation per cell to undo immediately. A producer needs the element, needs it typed, and must
// not become the fourth place in this package that knows a real is stored as its bits.
//
// st is never VsDictCode: a code is RESOLVED here, because the dictionary is per block and a
// caller holding the cell past that block has nothing left to resolve it against. str is empty
// unless st is VsString, and num is meaningful only for VsInt, VsReal (as math.Float64bits) and
// VsBool (0 or 1).
//
// Total by construction: every state, both forms, and an unresolvable code all map to some cell,
// because a producer that met an unexpected shape and dropped the row would lose a record rather
// than report an odd value for it.
func (v *Vec) Cell(i int) (st uint8, num int64, str string) {
	if v.Mask {
		switch maskStateAt(v.Hi, v.Lo, i) {
		case mTrue:
			return VsBool, 1, ""
		case mFalse:
			return VsBool, 0, ""
		case mUndef:
			return VsUndef, 0, ""
		default:
			return VsError, 0, ""
		}
	}
	if v.Const {
		i = 0
	}
	if rawSupported && v.Raw != nil {
		raw := readIntLE(v.Raw[i*v.RawWidth:], v.RawWidth, v.RawUnsigned)
		if v.RawReal {
			return VsReal, raw, ""
		}
		return VsInt, raw, ""
	}
	switch v.St[i] {
	case VsInt, VsReal, VsBool:
		return v.St[i], v.I[i], ""
	case VsString:
		return VsString, 0, v.S[i]
	case VsDictCode:
		if v.Dict != nil {
			if s, ok := v.Dict.At(int(v.I[i])); ok {
				return VsString, 0, s
			}
		}
		return VsError, 0, ""
	case VsUndef:
		return VsUndef, 0, ""
	}
	return VsError, 0, ""
}

// NewColumnVec allocates a vector that can receive a loaded COLUMN, mask planes included.
//
// NewVec is for a caller building values element by element and leaves Hi and Lo nil, which is
// right for it and wrong here: a boolean column with no escape is delivered as a MASK, so a vector
// without planes takes none of its bits and then reads past the end of a nil slice on the way out.
func NewColumnVec(n int) *Vec {
	v := newVec(n)
	for i := range v.St {
		v.St[i] = VsUndef
	}
	return v
}
