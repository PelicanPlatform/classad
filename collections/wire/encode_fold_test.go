package wire

import (
	"bytes"
	"math"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
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
//
// The int `-0` is the one place the encoder folds less than the unparser: 0 is its own negation,
// so folding it would make `--0` unstable across a decode/re-encode (see foldNegatedLiteral).
func TestEncodeDoesNotFoldBeyondOneLiteral(t *testing.T) {
	for _, src := range []string{"X = +1", "X = -(1)", "X = -a", "X = -(1 + 2)", "X = -0"} {
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
		node, _ := Ad(EncodeInline(nil, ad.AST())).LookupByName("X")
		_, folded := LiteralValue(node)
		// The reference serializer renders a folded operand with no space after "X =";
		// an unfolded unary operator keeps the leading space ("X =  -a").
		refFolded := !bytes.HasPrefix([]byte(ad.MarshalOld()), []byte("X =  "))
		if folded != refFolded {
			t.Errorf("%s: encoder folds=%v, reference unparser folds=%v (MarshalOld=%q)", src, folded, refFolded, ad.MarshalOld())
		}
	}
}

// negationFoldSeeds are the spellings at the edge of the fold: nested minuses over numeric
// literals (including both zeros), the shapes the fold must leave alone, and the int64 extremes.
// Both round-trip fuzzers seed from them.
var negationFoldSeeds = []string{
	`[A = --0]`, `[A = --1]`, `[A = --1.5]`, `[A = -0]`, `[A = -0.0]`, `[A = --0.0]`,
	`[A = ---0]`, `[A = ---1]`, `[A = - - 1]`, `[A = -(-1)]`, `[A = - -x]`,
	`[A = 9223372036854775807]`, `[A = -9223372036854775807]`, `[A = --9223372036854775807]`,
	`[A = -9223372036854775808]`, `[A = - -9223372036854775808]`,
}

// TestEncodeNegationFoldIdempotent pins that encode -> decode -> encode reproduces the same
// bytes for every spelling at the edge of the fold, in both the interned and the inline form,
// that the value survives the trip exactly (same type, same number, same sign of zero), and
// that the decoded ad still compares Equal to the parsed one.
//
// The fold runs bottom-up, so `--1` is stored as a minus over the literal -1, and decoding
// yields a minus over a negative literal -- a shape the parser never builds. If the encoder
// folded that shape too, the second encode would store the literal 1 and the bytes would move
// on every rewrite. This is the deterministic form of what FuzzRoundTrip and
// FuzzInlineRoundTrip found on `[A=--0]`.
func TestEncodeNegationFoldIdempotent(t *testing.T) {
	type form struct {
		name   string
		encode func(*testing.T, string) []byte
		decode func([]byte) (*classad.ClassAd, error)
	}
	forms := []form{
		{"interned",
			func(t *testing.T, src string) []byte {
				ad, err := parser.ParseClassAd(src)
				if err != nil {
					t.Fatalf("parse %q: %v", src, err)
				}
				return EncodeStandalone(ad)
			},
			func(b []byte) (*classad.ClassAd, error) {
				ad, err := DecodeStandalone(b)
				return toClassAd(ad), err
			}},
		{"inline",
			func(t *testing.T, src string) []byte {
				ad, err := parser.ParseClassAd(src)
				if err != nil {
					t.Fatalf("parse %q: %v", src, err)
				}
				return EncodeInline(nil, ad)
			},
			func(b []byte) (*classad.ClassAd, error) {
				ad, err := DecodeInline(b)
				return toClassAd(ad), err
			}},
	}
	reencode := map[string]func(*classad.ClassAd) []byte{
		"interned": func(c *classad.ClassAd) []byte { return EncodeStandalone(c.AST()) },
		"inline":   func(c *classad.ClassAd) []byte { return EncodeInline(nil, c.AST()) },
	}
	for _, src := range negationFoldSeeds {
		orig, err := parser.ParseClassAd(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		want := toClassAd(orig).EvaluateAttr("A")
		for _, f := range forms {
			first := f.encode(t, src)
			got, err := f.decode(first)
			if err != nil {
				t.Fatalf("%s %s: decode: %v", f.name, src, err)
			}
			second := reencode[f.name](got)
			if !bytes.Equal(first, second) {
				t.Errorf("%s %s: re-encode not stable:\n first  %x\n second %x", f.name, src, first, second)
			}
			v := got.EvaluateAttr("A")
			if !sameValue(want, v) {
				t.Errorf("%s %s: value changed across the round trip: %v -> %v", f.name, src, want, v)
			}
			// The stored ad must still compare Equal to the parsed one, or an unchanged ad reads
			// as changed. A fold wider than one minus (`--1` stored as the literal 1) is stable
			// and keeps the value, and this is the check that rejects it.
			if !toClassAd(orig).Equal(got) {
				t.Errorf("%s %s: decoded ad is not Equal to the parsed one: %s vs %s", f.name, src, orig, got.AST())
			}
		}
	}
}

// sameValue is strict identity for the values the fold touches: same type, and for numbers the
// same bits (so negative zero is not equal to positive zero).
func sameValue(a, b classad.Value) bool {
	if a.Type() != b.Type() {
		return false
	}
	switch {
	case a.IsInteger():
		x, _ := a.IntValue()
		y, _ := b.IntValue()
		return x == y
	case a.IsReal():
		x, _ := a.RealValue()
		y, _ := b.RealValue()
		return math.Float64bits(x) == math.Float64bits(y)
	}
	return a.String() == b.String()
}
