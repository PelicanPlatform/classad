package wire

import "encoding/binary"

// ScalarListElems walks a list literal whose every element is a plain scalar literal -- UNDEFINED, a
// boolean, an integer, a real or a string, each in the one tag the encoder writes for it -- calling fn
// with each element's node in order. It reports the element count and true when node is exactly one
// such list, and false for anything else: not a list, a malformed one, trailing bytes, or any element
// that is not one of those literals (ERROR, a nested list or ad, an expression, a number spelled as a
// sign over a literal). fn returning false stops the walk, which then reports false.
//
// It is the test a list must pass to be copied into a typed list column: what it accepts has one
// value per element, read without evaluating anything.
func ScalarListElems(node []byte, fn func(elem []byte) bool) (int, bool) {
	if len(node) < 2 || node[0] != nList {
		return 0, false
	}
	n, m := binary.Uvarint(node[1:])
	if m <= 0 || n > uint64(len(node)) {
		return 0, false
	}
	p := 1 + m
	for i := uint64(0); i < n; i++ {
		if p >= len(node) {
			return 0, false
		}
		l, ok := scalarNodeLen(node[p:])
		if !ok {
			return 0, false
		}
		if fn != nil && !fn(node[p:p+l]) {
			return 0, false
		}
		p += l
	}
	if p != len(node) {
		return 0, false
	}
	return int(n), true
}

// AppendListHeader appends the head of a list literal node of n elements: the node's tag and its element
// count, exactly as the encoder writes them. The n element nodes follow it.
func AppendListHeader(dst []byte, n int) []byte {
	return binary.AppendUvarint(append(dst, nList), uint64(n))
}

// scalarNodeLen is the length of the scalar literal node at the start of b, other than ERROR.
func scalarNodeLen(b []byte) (int, bool) {
	switch b[0] {
	case nUndefined, nBoolFalse, nBoolTrue:
		return 1, true
	case nInt:
		_, n := binary.Varint(b[1:])
		if n <= 0 {
			return 0, false
		}
		return 1 + n, true
	case nReal:
		if len(b) < 9 {
			return 0, false
		}
		return 9, true
	case nString:
		l, n := binary.Uvarint(b[1:])
		if n <= 0 || uint64(len(b)-1-n) < l {
			return 0, false
		}
		return 1 + n + int(l), true
	}
	return 0, false
}

// ElemKind is the kind of a scalar literal element node ScalarListElems accepted, read from its tag
// without decoding (and so without allocating a string).
func ElemKind(elem []byte) LitKind {
	switch elem[0] {
	case nUndefined:
		return LitUndef
	case nBoolFalse, nBoolTrue:
		return LitBool
	case nInt:
		return LitInt
	case nReal:
		return LitReal
	case nString:
		return LitString
	}
	return LitError
}
