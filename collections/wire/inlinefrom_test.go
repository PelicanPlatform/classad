package wire

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/collections/crypt"
)

// AppendAdSubsetInlineFromInterned rests on the same claim transcode.go's tests make, read the
// other way: rewriting an INTERNED record's bytes into inline-names form must produce the ad
// the decode-and-encode path produces. A tag missing from inlineNode truncates the record, and
// a mis-resolved id makes an expression reference a different attribute -- both of which look
// like a perfectly well-formed ad at the far end of the relay.
//
// So these compare against that path, and the random case walks the grammar rather than a
// handful of shapes.

// inlineBothWays encodes ad against an intern table, then produces the inline form two ways: by
// the byte transcode under test, and by decoding the interned bytes and encoding the ast.
func inlineBothWays(t *testing.T, ad *ast.ClassAd) (viaTranscode, viaAST string) {
	t.Helper()
	tt := NewInternTable()
	interned := Encode(nil, ad, tt)

	var sc SubsetScratch
	got, ok := AppendAdSubsetInlineFromInterned(nil, Ad(interned), tt.Name, nil, nil, &sc)
	if !ok {
		t.Fatal("AppendAdSubsetInlineFromInterned refused a well-formed interned ad")
	}
	if Ad(got)[2]&flagInlineNames == 0 {
		t.Fatal("the assembled row is not flagged inline-names")
	}
	gotAd, err := DecodeInline(got)
	if err != nil {
		t.Fatalf("decoding the transcoded ad: %v", err)
	}

	decoded, err := DecodeResolve(interned, tt.Name)
	if err != nil {
		t.Fatalf("decoding the interned ad: %v", err)
	}
	refAd, err := DecodeInline(EncodeInline(nil, decoded))
	if err != nil {
		t.Fatalf("decoding the reference ad: %v", err)
	}
	return render(gotAd), render(refAd)
}

// TestInlineFromInternedMatchesDecodeEncode covers each node kind the grammar has, with the
// name-carrying ones (reference, function, selector, nested record) that the rewrite touches.
func TestInlineFromInternedMatchesDecodeEncode(t *testing.T) {
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
		{"scoped reference", &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
			attr("T", ast.NewAttributeReference("Memory", ast.TargetScope)),
			attr("M", ast.NewAttributeReference("Rank", ast.MyScope)),
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
		{"function call", &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
			attr("F", &ast.FunctionCall{Name: "strcat", Args: []ast.Expr{
				ast.NewAttributeReference("Owner", ast.NoScope),
				&ast.StringLiteral{Value: "@"},
				&ast.FunctionCall{Name: "toUpper", Args: []ast.Expr{ast.NewAttributeReference("Host", ast.NoScope)}},
			}}),
			attr("Z", &ast.FunctionCall{Name: "time"}),
		}}},
		{"selector", &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
			attr("S", &ast.SelectExpr{Record: ast.NewAttributeReference("Rec", ast.NoScope), Attr: "Inner"}),
		}}},
		{"subscript and paren", &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
			attr("Sub", &ast.SubscriptExpr{Container: ast.NewAttributeReference("L", ast.NoScope),
				Index: &ast.IntegerLiteral{Value: 1}}),
			attr("P", &ast.ParenExpr{Inner: ast.NewAttributeReference("Q", ast.NoScope)}),
		}}},
		{"elvis and conditional", &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
			attr("C", &ast.ConditionalExpr{
				Condition: ast.NewAttributeReference("Cond", ast.NoScope),
				TrueExpr:  ast.NewAttributeReference("T", ast.NoScope),
				FalseExpr: &ast.IntegerLiteral{Value: 0},
			}),
			attr("EL", &ast.ElvisExpr{Left: ast.NewAttributeReference("Maybe", ast.NoScope),
				Right: &ast.IntegerLiteral{Value: 9}}),
		}}},
		{"unknown operator falls back to the string form", &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
			attr("W", &ast.BinaryOp{Op: "=?=", Left: ast.NewAttributeReference("A", ast.NoScope),
				Right: &ast.IntegerLiteral{Value: 1}}),
			attr("V", &ast.UnaryOp{Op: "??", Expr: ast.NewAttributeReference("B", ast.NoScope)}),
		}}},
		{"empty", &ast.ClassAd{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, want := inlineBothWays(t, tc.ad)
			if got != want {
				t.Fatalf("transcode disagrees with decode+encode:\n got %s\nwant %s", got, want)
			}
		})
	}
}

// TestInlineFromInternedRandom walks the grammar randomly and to depth, which is where a
// recursion bug lives -- the hand-written cases above are all shallow.
func TestInlineFromInternedRandom(t *testing.T) {
	r := rand.New(rand.NewSource(29))
	names := []string{"ClusterId", "ProcId", "Owner", "Requirements", "Memory", "Rank", "Env", "Cmd"}
	funcs := []string{"strcat", "toUpper", "ifThenElse", "regexp", "size"}
	var mkExpr func(depth int) ast.Expr
	mkExpr = func(depth int) ast.Expr {
		if depth > 3 {
			return &ast.IntegerLiteral{Value: int64(r.Intn(1000))}
		}
		switch r.Intn(16) {
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
			return &ast.BinaryOp{Op: []string{"+", "*", "&&", "==", "<", "=?="}[r.Intn(6)],
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
		case 11:
			args := make([]ast.Expr, r.Intn(3))
			for i := range args {
				args[i] = mkExpr(depth + 1)
			}
			return &ast.FunctionCall{Name: funcs[r.Intn(len(funcs))], Args: args}
		case 12:
			return &ast.SelectExpr{Record: mkExpr(depth + 1), Attr: names[r.Intn(len(names))]}
		case 13:
			return &ast.SubscriptExpr{Container: mkExpr(depth + 1), Index: mkExpr(depth + 1)}
		case 14:
			return &ast.ElvisExpr{Left: mkExpr(depth + 1), Right: mkExpr(depth + 1)}
		default:
			return &ast.ParenExpr{Inner: mkExpr(depth + 1)}
		}
	}
	for iter := 0; iter < 500; iter++ {
		ad := &ast.ClassAd{}
		for i := 0; i < 1+r.Intn(8); i++ {
			ad.Attributes = append(ad.Attributes, attr(fmt.Sprintf("A%d", i), mkExpr(0)))
		}
		got, want := inlineBothWays(t, ad)
		if got != want {
			t.Fatalf("iter %d: transcode disagrees with decode+encode:\n got %s\nwant %s", iter, got, want)
		}
	}
}

// TestInlineFromInternedKeepAndRefusals pins the contract around the happy path: the keep test
// selects entries by their RESOLVED name, an inline-names src is served by delegation rather
// than refused, and the two inputs whose ids this function cannot resolve are refused outright
// (so the caller falls back) instead of producing a row with wrong names in it.
func TestInlineFromInternedKeepAndRefusals(t *testing.T) {
	ad := &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
		attr("Owner", &ast.StringLiteral{Value: "alice"}),
		attr("Cmd", &ast.StringLiteral{Value: "/bin/true"}),
		attr("Requirements", &ast.BinaryOp{Op: "<", Left: ast.NewAttributeReference("Memory", ast.TargetScope),
			Right: &ast.IntegerLiteral{Value: 4096}}),
	}}
	tt := NewInternTable()
	interned := Encode(nil, ad, tt)
	var sc SubsetScratch

	keep := func(name, _ []byte) bool {
		return FoldEqualBytes(name, "owner") || FoldEqualBytes(name, "requirements")
	}
	got, ok := AppendAdSubsetInlineFromInterned(nil, Ad(interned), tt.Name, keep, nil, &sc)
	if !ok {
		t.Fatal("refused a well-formed interned ad")
	}
	kept := map[string]string{}
	if !Ad(got).ForEachNameNode(func(name, node []byte) bool {
		e, err := DecodeNodeInline(node)
		if err != nil {
			t.Fatalf("%s did not decode: %v", name, err)
		}
		kept[string(name)] = e.String()
		return true
	}) {
		t.Fatal("the assembled row does not walk")
	}
	if len(kept) != 2 || kept["Owner"] != `"alice"` {
		t.Fatalf("keep selected %v", kept)
	}
	if _, leaked := kept["Cmd"]; leaked {
		t.Fatalf("keep rejected Cmd but the row shipped it: %v", kept)
	}
	if want := "(TARGET.Memory < 4096)"; kept["Requirements"] != want {
		t.Fatalf("Requirements = %q, want %q", kept["Requirements"], want)
	}

	// An inline-names src is handled, not refused: callers hold a mixed population.
	inline := EncodeInline(nil, ad)
	if _, ok := AppendAdSubsetInlineFromInterned(nil, Ad(inline), tt.Name, nil, nil, &sc); !ok {
		t.Fatal("refused an inline-names src instead of delegating to AppendAdSubsetInline")
	}

	// A standalone ad carries its own table, which resolve knows nothing about: refuse.
	if _, ok := AppendAdSubsetInlineFromInterned(nil, Ad(EncodeStandalone(ad)), tt.Name, nil, nil, &sc); ok {
		t.Fatal("accepted a standalone ad, whose ids index its embedded table")
	}

	// An id the table cannot resolve is a malformed record, not an attribute to drop silently.
	if _, ok := AppendAdSubsetInlineFromInterned(nil, Ad(interned), func(uint32) (string, bool) {
		return "", false
	}, nil, nil, &sc); ok {
		t.Fatal("accepted a record whose ids do not resolve")
	}
}

// TestInlineFromInternedSealed covers the at-rest-encrypted entry on both sides of the
// entitlement rule the relay runs on: with a key the value is OPENED into the row (the consumer
// holds no key, so a still-sealed value would be unreadable there), and without one the entry is
// dropped rather than shipped. An interned record is not sealed by the write path today, so this
// is the guard that keeps it correct if it ever is.
func TestInlineFromInternedSealed(t *testing.T) {
	dek, err := crypt.NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	s := dekSealer{dek}
	ad := &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
		attr("Owner", &ast.StringLiteral{Value: "alice"}),
		attr("ClaimId", &ast.StringLiteral{Value: "top-secret"}),
	}}
	tt := NewInternTable()
	seal := func(name string) bool { return strings.EqualFold(name, "ClaimId") }
	interned := EncodeWithHotEnc(nil, ad, tt, nil, seal, s)

	var sc SubsetScratch
	entitled, ok := AppendAdSubsetInlineFromInterned(nil, Ad(interned), tt.Name, nil, s, &sc)
	if !ok {
		t.Fatal("refused a sealed interned ad for an entitled reader")
	}
	seen := map[string]string{}
	Ad(entitled).ForEachNameNode(func(name, node []byte) bool {
		e, derr := DecodeNodeInline(node)
		if derr != nil {
			t.Fatalf("%s did not decode: %v", name, derr)
		}
		seen[string(name)] = e.String()
		return true
	})
	if seen["ClaimId"] != `"top-secret"` {
		t.Fatalf("entitled row ClaimId = %q, want the opened value", seen["ClaimId"])
	}

	redacted, ok := AppendAdSubsetInlineFromInterned(nil, Ad(interned), tt.Name, nil, nil, &sc)
	if !ok {
		t.Fatal("refused a sealed interned ad for an unentitled reader")
	}
	if strings.Contains(string(redacted), "top-secret") {
		t.Fatal("a row assembled without a key carries the plaintext secret")
	}
	seen = map[string]string{}
	Ad(redacted).ForEachNameNode(func(name, node []byte) bool {
		seen[string(name)] = ""
		return true
	})
	if _, present := seen["ClaimId"]; present {
		t.Fatal("a row assembled without a key still ships the sealed entry")
	}
	if _, present := seen["Owner"]; !present {
		t.Fatal("the unsealed attributes were dropped along with the sealed one")
	}
}
