package wire

// NodeClass classifies a node by the kind of thing it holds, which is coarser than
// the tag table and finer than LiteralValue's ok flag.
//
// LiteralValue already answers "is this a scalar literal, and what is it"; everything
// it rejects comes back as one undifferentiated false. That is the right answer for a
// reader deciding whether it can take a fast path, and the wrong one for a caller
// PROFILING what an attribute holds across many records, which needs to tell apart
// the reasons a value is not a scalar: an expression can be evaluated to a scalar, a
// list or a nested ad cannot, and an encrypted node must not be looked at at all.
//
// Deliberately NOT unwrapped: nParen. `(5)` is a parenthesized expression, not an int
// literal, and classifying it as one here would disagree with the storage layer --
// LiteralValue rejects it, so nodeKind calls it akNone and the columnar encoder escapes
// it to the cold tail. A profile is measured against what storage does, so it has to
// classify the way storage classifies.
type NodeClass uint8

const (
	// ClassInvalid is an empty node or a tag this package does not write.
	ClassInvalid NodeClass = iota
	ClassUndefined
	ClassError
	ClassBool
	ClassInt
	ClassReal
	ClassString
	// ClassList and ClassRecord are composite VALUES: already evaluated, and with no
	// scalar form to coerce to.
	ClassList
	ClassRecord
	// ClassEncrypted is an at-rest-encrypted value (nEncrypted). The class is as far as
	// a caller without a Sealer may go: the plaintext's own class is deliberately not
	// reported, because reaching through to it is how a redacted path leaks a value it
	// was supposed to withhold.
	ClassEncrypted
	// ClassExpr is an unevaluated expression -- a reference, operator, call,
	// conditional, select, subscript or parenthesized form. It may evaluate to any
	// class above, including a scalar, which is why it is worth distinguishing from
	// ClassList and ClassRecord rather than lumping all non-literals together.
	ClassExpr
)

// String returns the lower-case class name, suitable for a report key.
func (c NodeClass) String() string {
	switch c {
	case ClassUndefined:
		return "undefined"
	case ClassError:
		return "error"
	case ClassBool:
		return "bool"
	case ClassInt:
		return "int"
	case ClassReal:
		return "real"
	case ClassString:
		return "string"
	case ClassList:
		return "list"
	case ClassRecord:
		return "classad"
	case ClassEncrypted:
		return "encrypted"
	case ClassExpr:
		return "expr"
	}
	return "invalid"
}

// Scalar reports whether the class is a scalar value -- one that has a single
// representable value now, without evaluation.
func (c NodeClass) Scalar() bool {
	switch c {
	case ClassBool, ClassInt, ClassReal, ClassString:
		return true
	}
	return false
}

// ClassifyNode returns the node's class. It reads only the tag byte, so it is O(1) and
// does not validate the payload: a node whose tag is known but whose body is truncated
// is classified by its tag, and it is the decoders (which do validate) that reject it.
func ClassifyNode(node []byte) NodeClass {
	if len(node) == 0 {
		return ClassInvalid
	}
	switch node[0] {
	case nUndefined:
		return ClassUndefined
	case nError:
		return ClassError
	case nBoolFalse, nBoolTrue:
		return ClassBool
	case nInt:
		return ClassInt
	case nReal:
		return ClassReal
	case nString:
		return ClassString
	case nList:
		return ClassList
	case nRecord:
		return ClassRecord
	case nEncrypted:
		return ClassEncrypted
	case nAttrRef, nBinOp, nUnOp, nFunc, nCond, nElvis, nSelect, nSubscript,
		nParen, nBinOpStr, nUnOpStr, nAttrRefStr, nFuncStr, nSelectStr:
		return ClassExpr
	}
	return ClassInvalid
}
