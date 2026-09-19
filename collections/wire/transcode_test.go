package wire

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

// The claim AppendInternedFromInline rests on is that transcoding the bytes produces the SAME ad
// the decode-and-encode path produces. Anything less is silent corruption: a mis-handled node tag
// truncates a record, and a mis-rewritten name makes an expression reference a different attribute
// -- which is exactly how the columnar path once turned `RequestMemory = ProcId * 512 + 7` into
// `Slack * 512 + 7`, structurally perfect and wrong.
//
// So every test here compares against that path rather than against an expectation, and the random
// one covers the node grammar rather than a handful of shapes.

// bothWays encodes ad inline, then produces the interned form two ways: by transcoding the inline
// bytes, and by decoding them and encoding the ast. It returns both renderings for comparison.
func bothWays(t *testing.T, ad *ast.ClassAd, hot map[uint32]struct{}) (viaTranscode, viaAST string) {
	t.Helper()
	inline := EncodeInline(nil, ad)

	tt := NewInternTable()
	got, ok := AppendInternedFromInline(nil, Ad(inline), tt, hot)
	if !ok {
		t.Fatal("AppendInternedFromInline refused a well-formed inline ad")
	}
	gotAd, err := DecodeResolve(got, tt.Name)
	if err != nil {
		t.Fatalf("decoding the transcoded ad: %v", err)
	}

	rt := NewInternTable()
	decoded, err := DecodeInline(inline)
	if err != nil {
		t.Fatalf("decoding the inline ad: %v", err)
	}
	ref := Encode(nil, decoded, rt)
	refAd, err := DecodeResolve(ref, rt.Name)
	if err != nil {
		t.Fatalf("decoding the reference ad: %v", err)
	}
	return render(gotAd), render(refAd)
}

// render is a comparable rendering: attributes sorted by name, values unparsed.
func render(a *ast.ClassAd) string {
	if a == nil {
		return "<nil>"
	}
	parts := make([]string, 0, len(a.Attributes))
	for _, at := range a.Attributes {
		parts = append(parts, strings.ToLower(at.Name)+"="+at.Value.String())
	}
	// insertion-sort: the lists are short and this avoids pulling in sort for a test helper
	for i := 1; i < len(parts); i++ {
		for j := i; j > 0 && parts[j] < parts[j-1]; j-- {
			parts[j], parts[j-1] = parts[j-1], parts[j]
		}
	}
	return strings.Join(parts, "|")
}

func attr(name string, v ast.Expr) *ast.AttributeAssignment {
	return &ast.AttributeAssignment{Name: name, Value: v}
}

// TestTranscodeMatchesDecodeEncode covers each node kind the grammar has, because a tag missing
// from transcodeNode fails by truncating rather than by erroring.
func TestTranscodeMatchesDecodeEncode(t *testing.T) {
	cases := []struct {
		name string
		ad   *ast.ClassAd
	}{
		{"literals", &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
			attr("I", &ast.IntegerLiteral{Value: -98765}),
			attr("R", &ast.RealLiteral{Value: 3.5}),
			attr("S", &ast.StringLiteral{Value: "a string with spaces"}),
			attr("BT", &ast.BooleanLiteral{Value: true}),
			attr("BF", &ast.BooleanLiteral{Value: false}),
			attr("U", &ast.UndefinedLiteral{}),
			attr("E", &ast.ErrorLiteral{}),
		}}},
		{"attribute reference", &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
			attr("A", ast.NewAttributeReference("Other", ast.NoScope)),
		}}},
		{"binop over refs", &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
			attr("Mem", &ast.BinaryOp{
				Op:    "+",
				Left:  &ast.BinaryOp{Op: "*", Left: ast.NewAttributeReference("ProcId", ast.NoScope), Right: &ast.IntegerLiteral{Value: 512}},
				Right: &ast.IntegerLiteral{Value: 7},
			}),
		}}},
		{"unary", &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
			attr("N", &ast.UnaryOp{Op: "!", Expr: ast.NewAttributeReference("Flag", ast.NoScope)}),
		}}},
		{"list of refs", &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
			attr("L", &ast.ListLiteral{Elements: []ast.Expr{
				ast.NewAttributeReference("X", ast.NoScope),
				&ast.IntegerLiteral{Value: 1},
				ast.NewAttributeReference("Y", ast.NoScope),
			}}),
		}}},
		{"nested record", &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
			attr("Rec", &ast.RecordLiteral{ClassAd: &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
				attr("Inner", ast.NewAttributeReference("Deep", ast.NoScope)),
				attr("Lit", &ast.IntegerLiteral{Value: 5}),
			}}}),
		}}},
		{"conditional", &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
			attr("C", &ast.ConditionalExpr{
				Condition: ast.NewAttributeReference("Cond", ast.NoScope),
				TrueExpr:  ast.NewAttributeReference("T", ast.NoScope),
				FalseExpr: &ast.IntegerLiteral{Value: 0},
			}),
		}}},
		{"scoped reference", &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
			attr("T", ast.NewAttributeReference("Memory", ast.TargetScope)),
			attr("M", ast.NewAttributeReference("Rank", ast.MyScope)),
		}}},
		{"empty", &ast.ClassAd{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, want := bothWays(t, tc.ad, nil)
			if got != want {
				t.Fatalf("transcode disagrees with decode+encode:\n got %s\nwant %s", got, want)
			}
		})
	}
}

// TestTranscodeRandomMatchesDecodeEncode walks the grammar randomly and to depth, which is where a
// recursion bug lives -- the hand-written cases above are all shallow.
func TestTranscodeRandomMatchesDecodeEncode(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	names := []string{"ClusterId", "ProcId", "Owner", "Requirements", "Memory", "Rank", "Env", "Cmd"}
	var mkExpr func(depth int) ast.Expr
	mkExpr = func(depth int) ast.Expr {
		if depth > 3 {
			return &ast.IntegerLiteral{Value: int64(r.Intn(1000))}
		}
		switch r.Intn(12) {
		case 0:
			return &ast.IntegerLiteral{Value: int64(r.Intn(1<<20)) - (1 << 19)}
		case 1:
			return &ast.RealLiteral{Value: r.Float64() * 1e6}
		case 2:
			return &ast.StringLiteral{Value: strings.Repeat("s", r.Intn(30))}
		case 3:
			return &ast.BooleanLiteral{Value: r.Intn(2) == 0}
		case 4:
			return ast.NewAttributeReference(names[r.Intn(len(names))], ast.NoScope)
		case 5:
			return ast.NewAttributeReference(names[r.Intn(len(names))], ast.TargetScope)
		case 6:
			return &ast.BinaryOp{Op: []string{"+", "*", "&&", "==", "<"}[r.Intn(5)],
				Left: mkExpr(depth + 1), Right: mkExpr(depth + 1)}
		case 7:
			return &ast.UnaryOp{Op: "!", Expr: mkExpr(depth + 1)}
		case 8:
			els := make([]ast.Expr, r.Intn(4))
			for i := range els {
				els[i] = mkExpr(depth + 1)
			}
			return &ast.ListLiteral{Elements: els}
		case 9:
			inner := &ast.ClassAd{}
			for i := 0; i < 1+r.Intn(3); i++ {
				inner.Attributes = append(inner.Attributes,
					attr(names[r.Intn(len(names))]+fmt.Sprint(i), mkExpr(depth+1)))
			}
			return &ast.RecordLiteral{ClassAd: inner}
		case 10:
			return &ast.ConditionalExpr{Condition: mkExpr(depth + 1),
				TrueExpr: mkExpr(depth + 1), FalseExpr: mkExpr(depth + 1)}
		default:
			return &ast.UndefinedLiteral{}
		}
	}
	for iter := 0; iter < 500; iter++ {
		ad := &ast.ClassAd{}
		for i := 0; i < 1+r.Intn(8); i++ {
			ad.Attributes = append(ad.Attributes, attr(fmt.Sprintf("A%d", i), mkExpr(0)))
		}
		got, want := bothWays(t, ad, nil)
		if got != want {
			t.Fatalf("iter %d: transcode disagrees with decode+encode:\n got %s\nwant %s", iter, got, want)
		}
	}
}

// TestTranscodeHotHeader: the hot header has to come out the way EncodeWithHotEnc writes it, or a
// reader following a hot offset lands in the middle of another attribute's node.
func TestTranscodeHotHeader(t *testing.T) {
	ad := &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
		attr("ClusterId", &ast.IntegerLiteral{Value: 42}),
		attr("Owner", &ast.StringLiteral{Value: "alice"}),
		attr("Memory", &ast.IntegerLiteral{Value: 2048}),
	}}
	inline := EncodeInline(nil, ad)
	tt := NewInternTable()
	// Intern the hot names first so their ids are known before the transcode runs.
	hotID := tt.Intern("Memory")
	got, ok := AppendInternedFromInline(nil, Ad(inline), tt, map[uint32]struct{}{hotID: {}})
	if !ok {
		t.Fatal("refused")
	}
	seen := map[uint32]string{}
	a := Ad(got)
	a.ForEachHot(func(id uint32, node []byte) bool {
		name, _ := tt.Name(id)
		expr, err := DecodeNodeResolve(node, tt.Name)
		if err != nil {
			t.Fatalf("hot node for %s did not decode: %v", name, err)
		}
		seen[id] = expr.String()
		return true
	})
	if len(seen) != 1 {
		t.Fatalf("hot header holds %d entries, want 1: %v", len(seen), seen)
	}
	if seen[hotID] != "2048" {
		t.Fatalf("hot entry for Memory is %q, want 2048 -- a wrong offset lands inside another node", seen[hotID])
	}
}

// TestTranscodeRefusesNonInline: an already-interned or standalone ad names its attributes against
// a table this transcode does not have, so it must refuse rather than reinterpret ids as names.
func TestTranscodeRefusesNonInline(t *testing.T) {
	ad := &ast.ClassAd{Attributes: []*ast.AttributeAssignment{attr("A", &ast.IntegerLiteral{Value: 1})}}
	interned := Encode(nil, ad, NewInternTable())
	if _, ok := AppendInternedFromInline(nil, Ad(interned), NewInternTable(), nil); ok {
		t.Error("transcoded an already-interned ad")
	}
	if _, ok := AppendInternedFromInline(nil, Ad("nonsense"), NewInternTable(), nil); ok {
		t.Error("transcoded a malformed ad")
	}
}

// TestTranscodeMixedCaseNames is the shape the real data has and the random test above does not:
// ONE record calling the same function under two spellings. Names are interned, and the table
// canonicalizes to the spelling it saw first, so which of the two survives depends on the order the
// names get interned in -- an order the transcode and the ast encoder must agree on, or a
// compaction pass produces a different (still semantically equal) rendering depending on which
// route it took.
func TestTranscodeMixedCaseNames(t *testing.T) {
	call := func(name string) ast.Expr {
		return &ast.FunctionCall{Name: name, Args: []ast.Expr{
			&ast.StringLiteral{Value: "osdf"},
			ast.NewAttributeReference("HasFileTransferPluginMethods", ast.TargetScope),
		}}
	}
	cases := []struct {
		name string
		ad   *ast.ClassAd
	}{
		{"two spellings in one value", &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
			attr("Requirements", &ast.BinaryOp{Op: "&&", Left: call("stringListIMember"), Right: call("StringListIMember")}),
		}}},
		{"two spellings across values", &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
			attr("A", call("StringListIMember")),
			attr("B", call("stringListIMember")),
		}}},
		{"attribute name cased two ways", &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
			attr("X", ast.NewAttributeReference("ProcId", ast.NoScope)),
			attr("Y", ast.NewAttributeReference("procid", ast.NoScope)),
		}}},
		{"attribute ref matching an attribute name", &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
			attr("procid", &ast.IntegerLiteral{Value: 1}),
			attr("Y", ast.NewAttributeReference("ProcId", ast.NoScope)),
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, want := bothWays(t, tc.ad, nil)
			if got != want {
				t.Errorf("transcode and decode+encode choose different spellings:\n transcode %s\n ast       %s", got, want)
			}
		})
	}
}

// TestTranscodeStreamBytesMatch compares the two routes over a SEQUENCE of records sharing one
// intern table, which is what a compaction segment actually is. A single record cannot show an
// ordering difference; the ids a record gets depend on every record before it, so equality has to
// be checked over the stream, and at the byte level -- two encodings that render alike can still
// differ in which spelling the table made canonical.
func TestTranscodeStreamBytesMatch(t *testing.T) {
	spell := []string{"stringListIMember", "StringListIMember", "STRINGLISTIMEMBER"}
	var ads []*ast.ClassAd
	for i := 0; i < 20; i++ {
		ads = append(ads, &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
			attr("ClusterId", &ast.IntegerLiteral{Value: int64(i)}),
			attr("Requirements", &ast.BinaryOp{Op: "&&",
				Left: &ast.FunctionCall{Name: spell[i%len(spell)], Args: []ast.Expr{
					&ast.StringLiteral{Value: "osdf"},
					ast.NewAttributeReference("HasFileTransferPluginMethods", ast.TargetScope)}},
				Right: &ast.FunctionCall{Name: spell[(i+1)%len(spell)], Args: []ast.Expr{
					&ast.StringLiteral{Value: "stash"},
					ast.NewAttributeReference("HasFileTransferPluginMethods", ast.TargetScope)}},
			}),
		}})
	}
	tw, ta := NewInternTable(), NewInternTable()
	for i, ad := range ads {
		inline := EncodeInline(nil, ad)
		gotWire, ok := AppendInternedFromInline(nil, Ad(inline), tw, nil)
		if !ok {
			t.Fatalf("record %d: transcode refused", i)
		}
		decoded, err := DecodeInline(inline)
		if err != nil {
			t.Fatal(err)
		}
		gotAST := Encode(nil, decoded, ta)
		if string(gotWire) != string(gotAST) {
			t.Fatalf("record %d: the two routes encode different bytes\n wire %x\n ast  %x", i, gotWire, gotAST)
		}
	}
	// And the tables themselves: same ids assigned to the same canonical spellings.
	for id := uint32(0); ; id++ {
		nw, okw := tw.Name(id)
		na, oka := ta.Name(id)
		if okw != oka {
			t.Fatalf("id %d present in one table only (wire %v, ast %v)", id, okw, oka)
		}
		if !okw {
			break
		}
		if nw != na {
			t.Fatalf("id %d is %q via wire but %q via ast -- the canonical spelling diverged", id, nw, na)
		}
	}
}
