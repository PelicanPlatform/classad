package wire

import "testing"

// nodeFor encodes one standalone expression to its wire node, so the cases below are
// classified from bytes the real encoder produced rather than from hand-rolled tags --
// a hand-rolled test would keep passing if a tag's meaning changed underneath it.
func nodeFor(t *testing.T, expr string) []byte {
	t.Helper()
	b, err := ParseExprToWire(expr, NewInternTable(), nil)
	if err != nil {
		t.Fatalf("ParseExprToWire(%q): %v", expr, err)
	}
	return b
}

func TestClassifyNode(t *testing.T) {
	cases := []struct {
		expr string
		want NodeClass
	}{
		{`undefined`, ClassUndefined},
		{`error`, ClassError},
		{`true`, ClassBool},
		{`false`, ClassBool},
		{`1`, ClassInt},
		{`-5`, ClassInt},
		{`3.14`, ClassReal},
		{`"hi"`, ClassString},
		{`{1, 2, 3}`, ClassList},
		{`{}`, ClassList},
		{`[a = 1]`, ClassRecord},
		{`[]`, ClassRecord},

		// Every unevaluated form is one class: a profile cares that the value needs
		// evaluating, not which syntax produced it.
		{`x`, ClassExpr},
		{`TARGET.Cpus`, ClassExpr},
		{`a + b`, ClassExpr},
		{`!x`, ClassExpr},
		{`a == b`, ClassExpr},
		{`strcat("a", "b")`, ClassExpr},
		{`a ? b : c`, ClassExpr},
		{`a ?: b`, ClassExpr},
		{`a.b`, ClassExpr},
		{`L[0]`, ClassExpr},

		// A parenthesized literal is an EXPRESSION, matching how storage classifies it:
		// LiteralValue rejects nParen, so the columnar encoder escapes it to the cold
		// tail. Unwrapping here would make a profile disagree with the escape counts it
		// is measured against.
		{`(5)`, ClassExpr},
	}
	for _, c := range cases {
		if got := ClassifyNode(nodeFor(t, c.expr)); got != c.want {
			t.Errorf("ClassifyNode(%q) = %v, want %v", c.expr, got, c.want)
		}
	}
}

// TestClassifyNodeAgreesWithLiteralValue pins the two classifiers together: a class that
// claims to be scalar must be one LiteralValue also accepts, and vice versa. They are
// separate switches over the same tag table, so this is what keeps them from drifting.
func TestClassifyNodeAgreesWithLiteralValue(t *testing.T) {
	for _, expr := range handledExprs {
		node := nodeFor(t, expr)
		class := ClassifyNode(node)
		lit, isLit := LiteralValue(node)

		// LiteralValue accepts the four scalars plus undefined and error; Scalar() is
		// only the four, so compare against the right set on each side.
		wantLit := class.Scalar() || class == ClassUndefined || class == ClassError
		if isLit != wantLit {
			t.Errorf("%q: class %v but LiteralValue ok=%v", expr, class, isLit)
			continue
		}
		if !isLit {
			continue
		}
		want := map[LitKind]NodeClass{
			LitUndef:  ClassUndefined,
			LitError:  ClassError,
			LitBool:   ClassBool,
			LitInt:    ClassInt,
			LitReal:   ClassReal,
			LitString: ClassString,
		}[lit.Kind]
		if class != want {
			t.Errorf("%q: class %v, LiteralValue kind implies %v", expr, class, want)
		}
	}
}

func TestClassifyNodeInvalid(t *testing.T) {
	if got := ClassifyNode(nil); got != ClassInvalid {
		t.Errorf("ClassifyNode(nil) = %v, want ClassInvalid", got)
	}
	if got := ClassifyNode([]byte{}); got != ClassInvalid {
		t.Errorf("ClassifyNode(empty) = %v, want ClassInvalid", got)
	}
	// TagExternalRef is reserved and never written by this package; it must not fall
	// into a real class, or a storage layer's placeholder would be profiled as a value.
	if got := ClassifyNode([]byte{TagExternalRef}); got != ClassInvalid {
		t.Errorf("ClassifyNode(TagExternalRef) = %v, want ClassInvalid", got)
	}
	if got := ClassifyNode(nil).String(); got != "invalid" {
		t.Errorf("ClassInvalid.String() = %q", got)
	}
}

// TestClassifyNodeEncrypted asserts an encrypted value reports as encrypted rather than
// as whatever it wraps -- the class is the boundary a caller without a Sealer stops at.
func TestClassifyNodeEncrypted(t *testing.T) {
	// nEncrypted's payload is uvarint(len nonce) + nonce + uvarint(len ct) + ct; the
	// class is read from the tag alone, so an empty payload is enough here.
	node := []byte{nEncrypted, 0, 0}
	if got := ClassifyNode(node); got != ClassEncrypted {
		t.Fatalf("ClassifyNode(encrypted) = %v, want ClassEncrypted", got)
	}
	if ClassEncrypted.Scalar() {
		t.Error("ClassEncrypted.Scalar() = true; an opaque value is not a scalar")
	}
}
