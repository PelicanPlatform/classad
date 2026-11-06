package wire

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/parser"
)

// reencodeScalarList re-encodes a list ScalarListElems accepts from its element VALUES, the way a reader
// holding only the typed values would have to rebuild it.
func reencodeScalarList(node []byte) ([]byte, bool) {
	var elems [][]byte
	n, ok := ScalarListElems(node, func(e []byte) bool { elems = append(elems, e); return true })
	if !ok {
		return nil, false
	}
	out := binary.AppendUvarint([]byte{nList}, uint64(n))
	for _, e := range elems {
		lit, ok := LiteralValue(e)
		if !ok {
			return nil, false
		}
		switch lit.Kind {
		case LitUndef:
			out = AppendUndefinedNode(out)
		case LitBool:
			out = AppendBoolNode(out, lit.Bool)
		case LitInt:
			out = AppendIntNode(out, lit.Int)
		case LitReal:
			out = AppendRealNode(out, lit.Real)
		case LitString:
			out = AppendStringNode(out, lit.Str)
		default:
			return nil, false
		}
	}
	return out, true
}

// TestScalarListEncoderIsCanonical: every list the encoder writes re-encodes from its values to the same
// bytes -- including NaN payloads, -0, the integer extremes and empty strings.
func TestScalarListEncoderIsCanonical(t *testing.T) {
	lists := []*ast.ListLiteral{
		{},
		{Elements: []ast.Expr{&ast.IntegerLiteral{Value: math.MinInt64}, &ast.IntegerLiteral{Value: math.MaxInt64}, &ast.IntegerLiteral{Value: 0}}},
		{Elements: []ast.Expr{&ast.RealLiteral{Value: math.Copysign(0, -1)}, &ast.RealLiteral{Value: math.NaN()}, &ast.RealLiteral{Value: math.Inf(-1)}}},
		{Elements: []ast.Expr{&ast.StringLiteral{Value: ""}, &ast.StringLiteral{Value: "busy"}, &ast.UndefinedLiteral{}}},
		{Elements: []ast.Expr{&ast.BooleanLiteral{Value: true}, &ast.BooleanLiteral{Value: false}}},
	}
	for _, l := range lists {
		ad := EncodeInline(nil, &ast.ClassAd{Attributes: []*ast.AttributeAssignment{{Name: "L", Value: l}}})
		node, ok := Ad(ad).LookupByName("L")
		if !ok {
			t.Fatalf("%v: not found", l)
		}
		re, ok := reencodeScalarList(node)
		if !ok || !bytes.Equal(re, node) {
			t.Errorf("%v: re-encoded %x, written %x", l, re, node)
		}
	}
}

// TestScalarListFormatIsNotCanonical: the format is not canonical, only this encoder is. A decoder accepts
// a varint with a redundant continuation byte, so the same values have more than one encoding, and a
// list rebuilt from its values is not the list as stored. Anything that moves a list out of its record
// and splices it back must therefore verify each cell's re-encoding against its stored bytes.
func TestScalarListFormatIsNotCanonical(t *testing.T) {
	stored := []byte{nList, 0x81, 0x00, nInt, 0x02} // a count of 1 spelled in two bytes
	if _, ok := ScalarListElems(stored, nil); !ok {
		t.Fatal("the reader rejects a non-minimal count; the format would then be canonical for counts")
	}
	re, ok := reencodeScalarList(stored)
	if !ok || bytes.Equal(re, stored) {
		t.Fatalf("re-encoded %x from %x", re, stored)
	}
}

// FuzzScalarListRoundTrip: whatever ScalarListElems accepts re-encodes to a list it also accepts, with the
// same values, and a list that is its own re-encoding (the encoder's form) is reproduced exactly.
func FuzzScalarListRoundTrip(f *testing.F) {
	f.Add([]byte{nList, 0x00})
	f.Add([]byte{nList, 0x02, nInt, 0x02, nUndefined})
	f.Add([]byte{nList, 0x01, nString, 0x02, 'h', 'i'})
	f.Add([]byte{nList, 0x81, 0x00, nBoolTrue})
	f.Fuzz(func(t *testing.T, b []byte) {
		re, ok := reencodeScalarList(b)
		if !ok {
			return
		}
		re2, ok := reencodeScalarList(re)
		if !ok || !bytes.Equal(re, re2) {
			t.Fatalf("the re-encoding %x of %x is not stable: %x", re, b, re2)
		}
		var a, c []Literal
		ScalarListElems(b, func(e []byte) bool { l, _ := LiteralValue(e); a = append(a, l); return true })
		ScalarListElems(re, func(e []byte) bool { l, _ := LiteralValue(e); c = append(c, l); return true })
		if len(a) != len(c) {
			t.Fatalf("%x: %d elements, re-encoded %d", b, len(a), len(c))
		}
		for i := range a {
			x, y := a[i], c[i]
			if x.Kind != y.Kind || x.Bool != y.Bool || x.Int != y.Int || x.Str != y.Str ||
				math.Float64bits(x.Real) != math.Float64bits(y.Real) {
				t.Fatalf("%x element %d: %+v, re-encoded %+v", b, i, x, y)
			}
		}
	})
}

// TestScalarListPoolCanonical re-encodes every qualifying list of the committed pool sample, as both
// encoders write it (the AST encoder and the wire-native expression parser), from its values, and
// requires the stored bytes back.
func TestScalarListPoolCanonical(t *testing.T) {
	f, err := os.Open("../vm/testdata/pool_sample.ads.gz")
	if err != nil {
		t.Skip("pool sample not found")
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	lists, elems := 0, 0
	for sc.Scan() {
		line := sc.Text()
		eq := strings.Index(line, " = {")
		if eq < 0 {
			continue
		}
		val := line[eq+3:]
		tbl := NewInternTable()
		node, err := ParseExprToWire(val, tbl, nil)
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		expr, err := parser.ParseExpr(val)
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		ad := EncodeInline(nil, &ast.ClassAd{Attributes: []*ast.AttributeAssignment{{Name: "L", Value: expr}}})
		astNode, _ := Ad(ad).LookupByName("L")
		for _, nd := range [][]byte{node, astNode} {
			n, ok := ScalarListElems(nd, nil)
			if !ok {
				continue
			}
			lists++
			elems += n
			if re, _ := reencodeScalarList(nd); !bytes.Equal(re, nd) {
				t.Errorf("%s: stored %x, re-encoded %x", line[:eq], nd, re)
			}
		}
	}
	if lists == 0 {
		t.Fatal("no qualifying list in the pool sample")
	}
	t.Logf("%d qualifying list encodings, %d elements, all canonical", lists, elems)
}

// TestScalarListElemsRejects: only plain scalar literals and UNDEFINED qualify as elements.
func TestScalarListElemsRejects(t *testing.T) {
	for name, node := range map[string][]byte{
		"ERROR element":     {nList, 0x01, nError},
		"nested list":       {nList, 0x01, nList, 0x00},
		"trailing bytes":    {nList, 0x00, nUndefined},
		"short count":       {nList, 0x02, nUndefined},
		"not a list":        {nInt, 0x02},
		"truncated string":  {nList, 0x01, nString, 0x05, 'a'},
		"signed over a lit": {nList, 0x01, nUnOp, 0x00, nInt, 0x02},
	} {
		if _, ok := ScalarListElems(node, nil); ok {
			t.Errorf("%s: %x qualified", name, node)
		}
	}
}
