package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/parser"
)

// The write side refuses exactly what the readers refuse. The tests here hold the two to one
// another: every value CheckDepth accepts reads back through every reader, and every value it
// refuses the full-ad decoder refuses too.

// deepAttr is the attribute deepAd builds its deep value under.
const deepAttr = "Deep"

// deepAd builds a two-attribute ad whose deepAttr value nests to a depth data chooses, within a few
// of MaxDepth either side, out of a mix of every construct the encoder writes. It returns the depth
// as it built it -- each construct's contribution counted here, independently of ExprDepth -- and
// whether data asks for the value to be sealed.
func deepAd(data []byte) (ad *ast.ClassAd, depth int, sealed bool) {
	i := 0
	pick := func() int {
		if len(data) == 0 {
			return 0
		}
		b := data[i%len(data)]
		i++
		return int(b)
	}
	target := MaxDepth - 4 + pick()%9
	sealed = pick()%2 == 1
	lit := func() ast.Expr { return &ast.IntegerLiteral{Value: 3} }
	ref := func() ast.Expr { return &ast.AttributeReference{Name: "x"} }

	// The leaf, including the forms a minus folds into (stored as the literal, depth 0) and the ones
	// it does not (a minus over its operand, depth 1).
	var v ast.Expr
	switch pick() % 9 {
	case 0:
		v, depth = ref(), 0
	case 1:
		v, depth = &ast.StringLiteral{Value: "s"}, 0
	case 2:
		v, depth = &ast.UnaryOp{Op: "-", Expr: &ast.IntegerLiteral{Value: 5}}, 0
	case 3:
		v, depth = &ast.UnaryOp{Op: "-", Expr: &ast.RealLiteral{Value: 2.5}}, 0
	case 4:
		v, depth = &ast.UnaryOp{Op: "-", Expr: &ast.IntegerLiteral{Value: 0}}, 1
	case 5:
		v, depth = &ast.UnaryOp{Op: "-", Expr: &ast.UnaryOp{Op: "-", Expr: &ast.IntegerLiteral{Value: 1}}}, 1
	case 6:
		v, depth = &ast.RecordLiteral{}, 1 // its (empty) body is one deeper
	case 7:
		v, depth = &ast.UnaryOp{Op: "-", Expr: &ast.RealLiteral{Value: math.Copysign(0, -1)}}, 1
	default:
		v, depth = &ast.ListLiteral{}, 0
	}
	for depth < target {
		switch pick() % 12 {
		case 0:
			v = &ast.BinaryOp{Op: "||", Left: v, Right: ref()}
		case 1:
			v = &ast.BinaryOp{Op: "+", Left: lit(), Right: v}
		case 2:
			v = &ast.UnaryOp{Op: "!", Expr: v}
		case 3:
			if foldsToLiteral(&ast.UnaryOp{Op: "-", Expr: v}) {
				v = &ast.UnaryOp{Op: "~", Expr: v} // a minus here would fold, not nest
			} else {
				v = &ast.UnaryOp{Op: "-", Expr: v}
			}
		case 4:
			v = &ast.ParenExpr{Inner: v}
		case 5:
			v = &ast.ListLiteral{Elements: []ast.Expr{lit(), v, lit()}}
		case 6:
			v = &ast.FunctionCall{Name: "f", Args: []ast.Expr{v, lit()}}
		case 7:
			v = &ast.RecordLiteral{ClassAd: &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
				{Name: "n", Value: lit()}, {Name: "m", Value: v},
			}}}
		case 8:
			switch pick() % 3 {
			case 0:
				v = &ast.ConditionalExpr{Condition: v, TrueExpr: lit(), FalseExpr: lit()}
			case 1:
				v = &ast.ConditionalExpr{Condition: ref(), TrueExpr: v, FalseExpr: lit()}
			default:
				v = &ast.ConditionalExpr{Condition: ref(), TrueExpr: lit(), FalseExpr: v}
			}
		case 9:
			v = &ast.ElvisExpr{Left: ref(), Right: v}
		case 10:
			v = &ast.SelectExpr{Record: v, Attr: "a"}
		default:
			if pick()%2 == 0 {
				v = &ast.SubscriptExpr{Container: v, Index: lit()}
			} else {
				v = &ast.SubscriptExpr{Container: ref(), Index: v}
			}
		}
		depth++
	}
	ad = &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
		{Name: "Other", Value: lit()},
		{Name: deepAttr, Value: v},
	}}
	return ad, depth, sealed
}

// longestChain is the most terms a `||` chain may have: n terms reach depth n-1.
const longestChain = MaxDepth + 1

// orChain is the n-term chain `a0 || a1 || ...` as the parser builds it.
func orChain(t testing.TB, n int) ast.Expr {
	t.Helper()
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteString(" || ")
		}
		fmt.Fprintf(&b, "a%d", i)
	}
	e, err := parser.ParseExpr(b.String())
	if err != nil {
		t.Fatalf("parsing a %d-term chain: %v", n, err)
	}
	return e
}

// encoding is one form the store writes an ad in, with the full-ad decoder for it and, for an
// interned form, the table its ids index.
type encoding struct {
	name   string
	enc    []byte
	tbl    *InternTable
	decode func([]byte) (*ast.ClassAd, error)
}

func encodingsOf(ad *ast.ClassAd, sealed bool) []encoding {
	tbl := NewInternTable()
	decTbl := func(b []byte) (*ast.ClassAd, error) { return Decode(b, tbl) }
	if !sealed {
		return []encoding{
			{"inline", EncodeInline(nil, ad), nil, DecodeInline},
			{"interned", Encode(nil, ad, tbl), tbl, decTbl},
			{"interned-hot", EncodeWithHot(nil, ad, tbl, map[uint32]struct{}{tbl.Intern(deepAttr): {}}), tbl, decTbl},
			{"standalone", EncodeStandalone(ad), nil, DecodeStandalone},
		}
	}
	seal := xorSealer{}
	enc := func(name string) bool { return name == deepAttr }
	return []encoding{
		{"inline-sealed", EncodeInlineWithHotEnc(nil, ad, nil, enc, seal), nil,
			func(b []byte) (*ast.ClassAd, error) { return DecodeInlineEnc(b, seal) }},
		{"interned-sealed", EncodeWithHotEnc(nil, ad, tbl, nil, enc, seal), tbl,
			func(b []byte) (*ast.ClassAd, error) { return DecodeResolveEnc(b, tbl.Name, seal) }},
	}
}

// checkDepthAgreement asserts, for one ad, that CheckDepth's verdict is the full-ad decoder's in
// every encoding, and that an accepted ad reads back through every reader.
func checkDepthAgreement(t *testing.T, ad *ast.ClassAd, sealed bool) {
	t.Helper()
	accepted := CheckDepth(ad) == nil
	for _, e := range encodingsOf(ad, sealed) {
		got, err := e.decode(e.enc)
		if accepted != (err == nil) {
			t.Fatalf("%s: CheckDepth accepted=%v but the decoder returned err=%v", e.name, accepted, err)
		}
		if !accepted {
			continue
		}
		if len(got.Attributes) != len(ad.Attributes) {
			t.Fatalf("%s: decoded %d attributes, want %d", e.name, len(got.Attributes), len(ad.Attributes))
		}
		readEveryWay(t, e, sealed)
	}
}

// readEveryWay runs every reader that takes an ad or one of its attribute nodes over e, an ad the
// write side accepted, and fails on the first that refuses it.
func readEveryWay(t *testing.T, e encoding, sealed bool) {
	t.Helper()
	seal := xorSealer{}
	a := Ad(e.enc)
	if e.enc[2]&flagStandalone != 0 {
		return // its ids index the table embedded in it, which only DecodeStandalone reads
	}
	if e.tbl == nil { // inline names
		node, ok := a.LookupByName(deepAttr)
		if !ok {
			t.Fatalf("%s: LookupByName found no %s", e.name, deepAttr)
		}
		mustRead(t, e.name, "NodeLenKeyed", func() error { return okErr(NodeLenKeyed(node, true)) })
		mustRead(t, e.name, "ForEachNameNode", func() error {
			return okErr(0, a.ForEachNameNode(func(_, _ []byte) bool { return true }))
		})
		mustRead(t, e.name, "DecodeNodeInlineEnc", func() error { _, err := DecodeNodeInlineEnc(node, seal); return err })
		mustRead(t, e.name, "AppendNodeTextInlineOldEnc", func() error {
			_, err := AppendNodeTextInlineOldEnc(nil, node, seal, false)
			return err
		})
		mustRead(t, e.name, "AppendNodeTextInlineOldEnc(redact)", func() error {
			_, err := AppendNodeTextInlineOldEnc(nil, node, nil, true)
			return err
		})
		mustRead(t, e.name, "DecodeInlineRedact", func() error { _, err := DecodeInlineRedact(e.enc); return err })
		mustRead(t, e.name, "AppendAdSubsetInline", func() error {
			out, ok := AppendAdSubsetInline(nil, a, nil, seal)
			if !ok {
				return errors.New("refused")
			}
			_, err := DecodeInline(out)
			return err
		})
		if sealed {
			return
		}
		mustRead(t, e.name, "DecodeNodeInline", func() error { _, err := DecodeNodeInline(node); return err })
		mustRead(t, e.name, "AppendNodeTextInline", func() error { _, err := AppendNodeTextInline(nil, node); return err })
		mustRead(t, e.name, "AppendNodeTextInlineOld", func() error { _, err := AppendNodeTextInlineOld(nil, node); return err })
		mustRead(t, e.name, "AppendNodeRefsExact", func() error {
			return okErr(0, AppendNodeRefsExact(node, true, &NodeRefs{}, nil))
		})
		mustRead(t, e.name, "AppendInternedFromInline", func() error {
			tbl := NewInternTable()
			out, ok := AppendInternedFromInline(nil, a, tbl, nil)
			if !ok {
				return errors.New("refused")
			}
			_, err := Decode(out, tbl)
			return err
		})
		return
	}
	node, ok := a.Lookup(e.tbl.Intern(deepAttr))
	if !ok {
		t.Fatalf("%s: Lookup found no %s", e.name, deepAttr)
	}
	mustRead(t, e.name, "NodeLenKeyed", func() error { return okErr(NodeLenKeyed(node, false)) })
	mustRead(t, e.name, "ForEachNamed", func() error {
		return okErr(0, a.ForEachNamed(e.tbl, func(string, []byte) bool { return true }))
	})
	mustRead(t, e.name, "DecodeNodeResolveEnc", func() error { _, err := DecodeNodeResolveEnc(node, e.tbl.Name, seal); return err })
	mustRead(t, e.name, "AppendNodeTextOldEnc", func() error {
		_, err := AppendNodeTextOldEnc(nil, node, e.tbl, seal, false)
		return err
	})
	mustRead(t, e.name, "DecodeResolveEncRedact", func() error { _, err := DecodeResolveEncRedact(e.enc, e.tbl.Name, nil); return err })
	mustRead(t, e.name, "AppendAdSubsetInlineFromInterned", func() error {
		out, ok := AppendAdSubsetInlineFromInterned(nil, a, e.tbl.Name, nil, seal, &SubsetScratch{})
		if !ok {
			return errors.New("refused")
		}
		_, err := DecodeInline(out)
		return err
	})
	if sealed {
		return
	}
	mustRead(t, e.name, "DecodeNode", func() error { _, err := DecodeNode(node, e.tbl); return err })
	mustRead(t, e.name, "AppendNodeText", func() error { _, err := AppendNodeText(nil, node, e.tbl); return err })
	mustRead(t, e.name, "AppendNodeTextOld", func() error { _, err := AppendNodeTextOld(nil, node, e.tbl); return err })
	mustRead(t, e.name, "AppendNodeRefsExact", func() error {
		return okErr(0, AppendNodeRefsExact(node, false, &NodeRefs{}, func(uint32) bool { return false }))
	})
}

// okErr turns a reader's ok=false into an error, for mustRead.
func okErr[T any](_ T, ok bool) error {
	if !ok {
		return errors.New("refused")
	}
	return nil
}

func mustRead(t *testing.T, enc, reader string, fn func() error) {
	t.Helper()
	if err := fn(); err != nil {
		t.Fatalf("%s: %s refused a value the write side accepted: %v", enc, reader, err)
	}
}

// TestExprDepthCountsWhatTheDecoderCounts pins ExprDepth against deepAd's own count of the levels it
// built, and CheckDepth against every decoder and reader, for values at the limit and either side of
// it in every shape -- a deterministic sweep of the property FuzzDepthAgreement explores.
func TestExprDepthCountsWhatTheDecoderCounts(t *testing.T) {
	var accepted, refused, sealedAtLimit int
	for seed := range 400 {
		data := []byte{byte(seed), byte(seed >> 3), byte(seed * 7), byte(seed*13 + 1), byte(seed * 31)}
		ad, want, sealed := deepAd(data)
		if got := ExprDepth(ad.Attributes[1].Value); got != want {
			t.Fatalf("seed %d: ExprDepth = %d, built %d levels", seed, got, want)
		}
		checkDepthAgreement(t, ad, sealed)
		switch {
		case want > MaxDepth:
			refused++
		default:
			accepted++
			if sealed && want == MaxDepth {
				sealedAtLimit++
			}
		}
	}
	// The sweep must straddle the limit, or it proves nothing about where the limit is.
	if accepted == 0 || refused == 0 || sealedAtLimit == 0 {
		t.Fatalf("sweep did not straddle the limit: %d accepted (%d sealed at it), %d refused", accepted, sealedAtLimit, refused)
	}
}

// FuzzDepthAgreement: whatever value the fuzzer builds near the limit, ExprDepth counts the levels
// it has, CheckDepth refuses exactly what the full-ad decoder refuses in every encoding, and anything
// accepted reads back through every reader.
func FuzzDepthAgreement(f *testing.F) {
	for off := range 9 {
		for w := range 12 {
			f.Add([]byte{byte(off), byte(w % 2), byte(w), byte(w), byte(w + 1)})
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		ad, want, sealed := deepAd(data)
		if got := ExprDepth(ad.Attributes[1].Value); got != want {
			t.Fatalf("ExprDepth = %d, built %d levels", got, want)
		}
		checkDepthAgreement(t, ad, sealed)
	})
}

// TestOrChainDepth: an N-term `||` chain reaches depth N-1, so 1001 terms is exactly the limit -- it
// stores and reads back -- and 1002 is refused, as is 5000, with the attribute and depth named.
func TestOrChainDepth(t *testing.T) {
	for _, n := range []int{longestChain, longestChain + 1, 5000} {
		v := orChain(t, n)
		if got := ExprDepth(v); got != n-1 {
			t.Fatalf("%d-term chain: ExprDepth = %d", n, got)
		}
		ad := &ast.ClassAd{Attributes: []*ast.AttributeAssignment{{Name: "Requirements", Value: v}}}
		err := CheckDepth(ad)
		if n <= longestChain {
			if err != nil {
				t.Fatalf("%d-term chain refused: %v", n, err)
			}
			got, derr := DecodeInline(EncodeInline(nil, ad))
			if derr != nil {
				t.Fatalf("%d-term chain does not read back: %v", n, derr)
			}
			if got.Attributes[0].Value.String() != v.String() {
				t.Fatalf("%d-term chain read back changed", n)
			}
			continue
		}
		if !errors.Is(err, ErrTooDeep) {
			t.Fatalf("%d-term chain: err = %v, want ErrTooDeep", n, err)
		}
		var de *DepthError
		if !errors.As(err, &de) || de.Attr != "Requirements" || de.Depth != n-1 {
			t.Fatalf("%d-term chain: err = %#v, want attribute Requirements at depth %d", n, err, n-1)
		}
		if msg := err.Error(); !strings.Contains(msg, `"Requirements"`) || !strings.Contains(msg, fmt.Sprint(n-1)) {
			t.Fatalf("message %q does not name the attribute and its depth", msg)
		}
		checkDepthAgreement(t, ad, false)
	}
}

// TestSealedValueDepthIsTransparent: an attribute exactly at the limit reads back whether or not it
// is encrypted at rest. The sealed wrapper used to cost a level, so the same value sealed was one
// level too deep, and widening the set of encrypted attributes re-sealed values that had fit into
// ones that did not.
func TestSealedValueDepthIsTransparent(t *testing.T) {
	ad := &ast.ClassAd{Attributes: []*ast.AttributeAssignment{{Name: deepAttr, Value: orChain(t, longestChain)}}}
	if err := CheckDepth(ad); err != nil {
		t.Fatalf("CheckDepth: %v", err)
	}
	for _, e := range encodingsOf(ad, true) {
		if _, err := e.decode(e.enc); err != nil {
			t.Fatalf("%s: a sealed value at the limit does not read back: %v", e.name, err)
		}
		readEveryWay(t, e, true)
	}
}

// TestNestedSealedValueStillCostsALevel: a sealed value inside a sealed value's plaintext -- which no
// encoder writes -- is read one level down, so a crafted chain of them is bounded by MaxDepth rather
// than by the input's size.
func TestNestedSealedValueStillCostsALevel(t *testing.T) {
	seal := xorSealer{}
	nest := func(layers int) []byte {
		node := AppendIntNode(nil, 1)
		for range layers {
			nonce, ct, _ := seal.Seal(node)
			node = append([]byte{nEncrypted}, appendBytes(appendBytes(nil, nonce), ct)...)
		}
		return node
	}
	// A bare node is level 0 and the outermost sealed layer is transparent, so the literal under N
	// layers sits at level N-1.
	ok, deep := nest(MaxDepth+1), nest(MaxDepth+2)
	if _, err := DecodeNodeInlineEnc(ok, seal); err != nil {
		t.Fatalf("%d layers of sealed values do not decode: %v", MaxDepth+1, err)
	}
	if _, err := AppendNodeTextInlineOldEnc(nil, ok, seal, false); err != nil {
		t.Fatalf("%d layers of sealed values do not render: %v", MaxDepth+1, err)
	}
	if _, err := DecodeNodeInlineEnc(deep, seal); !errors.Is(err, ErrMalformed) {
		t.Fatalf("%d layers of sealed values decoded: err = %v", MaxDepth+2, err)
	}
	if _, err := AppendNodeTextInlineOldEnc(nil, deep, seal, false); err == nil {
		t.Fatalf("%d layers of sealed values rendered", MaxDepth+2)
	}
}

// appendBytes appends b with its uvarint length prefix, as a sealed node lays out nonce and
// ciphertext.
func appendBytes(dst, b []byte) []byte {
	return append(binary.AppendUvarint(dst, uint64(len(b))), b...)
}

// TestStreamEncoderRefusesTooDeep: the stream encoder refuses a too-deep value from either entry
// point, without writing a partial entry, and reports it through Err until Reset.
func TestStreamEncoderRefusesTooDeep(t *testing.T) {
	var b strings.Builder
	for i := range longestChain + 1 {
		if i > 0 {
			b.WriteString(" || ")
		}
		fmt.Fprintf(&b, "a%d", i)
	}
	text := b.String()
	for _, inline := range []bool{true, false} {
		s := NewStreamEncoder(NewInternTable(), nil)
		if inline {
			s = NewInlineStreamEncoder(nil)
		}
		s.Int("A", 1)
		before := len(s.entries)
		if err := s.ExprWire("R", text); !errors.Is(err, ErrTooDeep) {
			t.Fatalf("inline=%v: ExprWire err = %v, want ErrTooDeep", inline, err)
		}
		if len(s.entries) != before || s.Count() != 1 {
			t.Fatalf("inline=%v: ExprWire left a partial entry", inline)
		}
		if s.Err() != nil {
			t.Fatalf("inline=%v: ExprWire's rollback set Err: %v", inline, s.Err())
		}
		s.Expr("R", orChain(t, longestChain+1))
		var de *DepthError
		if !errors.As(s.Err(), &de) || de.Attr != "R" || de.Depth != MaxDepth+1 {
			t.Fatalf("inline=%v: Err = %v, want R at depth %d", inline, s.Err(), MaxDepth+1)
		}
		if s.Count() != 1 {
			t.Fatalf("inline=%v: Expr wrote the refused attribute", inline)
		}
		s.Reset()
		if s.Err() != nil {
			t.Fatalf("inline=%v: Reset kept Err", inline)
		}
		// At the limit, both entry points write it, and it reads back.
		atLimit := text[:strings.LastIndex(text, " || ")]
		if err := s.ExprWire("R", atLimit); err != nil {
			t.Fatalf("inline=%v: ExprWire at the limit: %v", inline, err)
		}
		s.Expr("S", orChain(t, longestChain))
		if s.Err() != nil {
			t.Fatalf("inline=%v: Expr at the limit: %v", inline, s.Err())
		}
		out := s.Bytes(nil)
		var err error
		if inline {
			_, err = DecodeInline(out)
		} else {
			_, err = Decode(out, s.t)
		}
		if err != nil {
			t.Fatalf("inline=%v: a stream-encoded ad at the limit does not read back: %v", inline, err)
		}
	}
}
