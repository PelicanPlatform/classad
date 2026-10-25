package wire

import (
	"bytes"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// TestEncodeFoldsNegatedLiterals pins the fold itself: `-<numeric literal>` reaches the wire as
// one signed literal node, so every reader that decides by node tag sees a stored scalar.
//
// It asserts the node TAG, not just the value, because the value reads back correctly either way
// -- NumberValue sees through a unary minus. The tag is the whole point: it is what LiteralValue,
// the schema's kind sampling and the columnar readers dispatch on.
func TestEncodeFoldsNegatedLiterals(t *testing.T) {
	cases := []struct {
		src  string
		kind LitKind
		i    int64
		f    float64
	}{
		{"X = -1", LitInt, -1, 0},
		{"X = - 42", LitInt, -42, 0},
		{"X = -0", LitInt, 0, 0},
		{"X = -2.5", LitReal, 0, -2.5},
		{"X = -1.0e10", LitReal, 0, -1e10},
	}
	for _, tc := range cases {
		ad, err := classad.ParseOld(tc.src)
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		node, ok := Ad(EncodeInline(nil, ad.AST())).LookupByName("X")
		if !ok {
			t.Fatalf("%s: X not found", tc.src)
		}
		lit, ok := LiteralValue(node)
		if !ok {
			t.Errorf("%s: encoded as a non-literal node (tag %#x), want a folded literal", tc.src, node[0])
			continue
		}
		if lit.Kind != tc.kind || lit.Int != tc.i || lit.Real != tc.f {
			t.Errorf("%s: folded to %+v, want kind=%d int=%d real=%v", tc.src, lit, tc.kind, tc.i, tc.f)
		}
	}
}

// TestEncodeDoesNotFoldBeyondOneLiteral keeps the fold where the reference engine's own
// unparser puts it (classad's unparseUnary): unary minus directly on a numeric literal, nothing
// else. Unary plus and a parenthesized operand stay expressions, so the two agree on what a
// signed literal is.
func TestEncodeDoesNotFoldBeyondOneLiteral(t *testing.T) {
	for _, src := range []string{"X = +1", "X = -(1)", "X = -a", "X = -(1 + 2)"} {
		ad, err := classad.ParseOld(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		node, _ := Ad(EncodeInline(nil, ad.AST())).LookupByName("X")
		if _, ok := LiteralValue(node); ok {
			t.Errorf("%s: folded to a literal; the reference keeps it an expression", src)
		}
	}
}

// TestEncodeNegativeOldFormDecodes pins the READ side against the fold.
//
// Records written before it carry `-1` as a unary-minus node over the literal 1, and nothing
// re-encodes them -- a segment sealed last year is read as it was written. So the decode path
// must keep handling that node exactly as it did, and the bytes here are written out by hand
// rather than produced by the encoder, which no longer emits them: an encoder-built fixture
// would silently start testing the new form the moment the fold changed.
func TestEncodeNegativeOldFormDecodes(t *testing.T) {
	// nUnOp, unOps[0] ("-"), nInt, zigzag varint 2 == 1.   This is byte-for-byte what the
	// encoder wrote for `-1` before the fold (see testdata/main_encodings.txt).
	oldForm := []byte{nUnOp, 0x00, nInt, 0x02}

	if _, ok := LiteralValue(oldForm); ok {
		t.Error("the old form is a unary-minus EXPRESSION node; LiteralValue must still decline it")
	}
	lit, ok := NumberValue(oldForm)
	if !ok || lit.Kind != LitInt || lit.Int != -1 {
		t.Errorf("NumberValue(old form) = (%+v, %v), want int -1", lit, ok)
	}
	e, err := DecodeNodeInline(oldForm)
	if err != nil {
		t.Fatalf("DecodeNodeInline(old form): %v", err)
	}
	if got := e.String(); got != "(-1)" {
		t.Errorf("old form decodes to %q, want %q -- an old record's rendering must not move", got, "(-1)")
	}
	txt, err := AppendNodeTextInline(nil, oldForm)
	if err != nil {
		t.Fatalf("AppendNodeTextInline(old form): %v", err)
	}
	if string(txt) != "(-1)" {
		t.Errorf("old form renders as %q, want %q", txt, "(-1)")
	}

	// And the new form is a different, shorter encoding of the same number.
	newForm := AppendIntNode(nil, -1)
	if bytes.Equal(newForm, oldForm) {
		t.Fatal("the fold must change the bytes; otherwise this test proves nothing")
	}
	if lit, ok := LiteralValue(newForm); !ok || lit.Int != -1 {
		t.Errorf("new form = %+v, %v; want int -1", lit, ok)
	}
	if got, _ := AppendNodeTextInline(nil, newForm); string(got) != "-1" {
		t.Errorf("new form renders as %q, want %q -- the form MarshalOld and the C++ reference already print", got, "-1")
	}
}

// TestFoldMatchesReferenceUnparser ties the fold's scope to the reference engine's: the encoder
// folds a unary minus into a literal in exactly the cases classad's old-ClassAd serializer
// (which mirrors the C++ unparser) prints as a bare signed number.
func TestFoldMatchesReferenceUnparser(t *testing.T) {
	for _, src := range []string{"X = -1", "X = - 42", "X = -2.5", "X = +1", "X = -(1)", "X = -a"} {
		ad, err := classad.ParseOld(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		folded := false
		if u, ok := ad.AST().Attributes[0].Value.(*ast.UnaryOp); ok {
			_, folded = negatedNumericLiteral(u)
		}
		// The reference serializer renders a folded operand with no space after "X =";
		// an unfolded unary operator keeps the leading space ("X =  -a").
		refFolded := !bytes.HasPrefix([]byte(ad.MarshalOld()), []byte("X =  "))
		if folded != refFolded {
			t.Errorf("%s: encoder folds=%v, reference unparser folds=%v (MarshalOld=%q)", src, folded, refFolded, ad.MarshalOld())
		}
	}
}
