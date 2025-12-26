package classad

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

func TestExprEqual(t *testing.T) {
	a, err := ParseExpr("Foo + bar")
	if err != nil {
		t.Fatalf("parse expr a: %v", err)
	}
	b, err := ParseExpr("foo + BAR")
	if err != nil {
		t.Fatalf("parse expr b: %v", err)
	}
	c, err := ParseExpr("foo + baz")
	if err != nil {
		t.Fatalf("parse expr c: %v", err)
	}

	if !a.Equal(b) {
		t.Fatalf("expected expressions to be equal")
	}
	if a.Equal(c) {
		t.Fatalf("expected expressions to differ")
	}
}

func TestClassAdEqual(t *testing.T) {
	ad1, err := Parse(`[
        A = 1;
        list = {1, 2};
        nested = [b = 2; a = 1];
    ]`)
	if err != nil {
		t.Fatalf("parse ad1: %v", err)
	}

	ad2, err := Parse(`[
        nested = [a = 1; b = 2];
        list = {1, 2};
        a = 1;
    ]`)
	if err != nil {
		t.Fatalf("parse ad2: %v", err)
	}

	ad3, err := Parse(`[
        nested = [a = 1; b = 3];
        list = {1, 2};
        a = 1;
    ]`)
	if err != nil {
		t.Fatalf("parse ad3: %v", err)
	}

	if !ad1.Equal(ad2) {
		t.Fatalf("expected ad1 and ad2 to be equal")
	}
	if ad1.Equal(ad3) {
		t.Fatalf("expected ad1 and ad3 to differ")
	}
}

func TestClassAdEqualAfterJSONRoundTrip(t *testing.T) {
	original, err := Parse(`[
        Z = 1;
        nested = [b = 2; A = 1; c = 3];
        alpha = [y = 20; x = 10];
        outer = [
            inner = [b = 5; a = 4];
            num = 7
        ];
        list = { [z = 9; y = 8; x = 7], 5, 4 }
    ]`)
	if err != nil {
		t.Fatalf("parse original: %v", err)
	}

	bytes1, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal original: %v", err)
	}

	var roundTripped ClassAd
	if err := json.Unmarshal(bytes1, &roundTripped); err != nil {
		t.Fatalf("unmarshal copy: %v", err)
	}

	if !original.Equal(&roundTripped) {
		t.Fatalf("expected original and copy to be equal after round-trip")
	}
}

func TestCaseInsensitiveAttributeReferences(t *testing.T) {
	ad, err := Parse(`[Foo = bar; BAR = 3]`)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	val := ad.EvaluateAttr("Foo")
	if !val.IsInteger() {
		t.Fatalf("expected Foo to evaluate to integer, got %v", val.Type())
	}
	intVal, _ := val.IntValue()
	if intVal != 3 {
		t.Fatalf("expected Foo to evaluate to 3, got %d", intVal)
	}
}

func exprFromAST(e ast.Expr) *Expr {
	return &Expr{expr: e}
}

func TestExprEqualAllCases(t *testing.T) {
	int1 := &ast.IntegerLiteral{Value: 1}
	int2 := &ast.IntegerLiteral{Value: 2}
	realNearA := &ast.RealLiteral{Value: 1.0}
	realNearB := &ast.RealLiteral{Value: 1.0 + 1e-10}
	realFar := &ast.RealLiteral{Value: 1.0 + 1e-6}
	realNaN := &ast.RealLiteral{Value: math.NaN()}
	realPosInf := &ast.RealLiteral{Value: math.Inf(1)}
	realNegInf := &ast.RealLiteral{Value: math.Inf(-1)}
	strFoo := &ast.StringLiteral{Value: "foo"}
	strBar := &ast.StringLiteral{Value: "bar"}
	boolTrue := &ast.BooleanLiteral{Value: true}
	boolFalse := &ast.BooleanLiteral{Value: false}
	undef := &ast.UndefinedLiteral{}
	errLit := &ast.ErrorLiteral{}
	attrFoo := &ast.AttributeReference{Name: "Foo"}
	attrfoo := &ast.AttributeReference{Name: "foo"}
	binAdd1 := &ast.BinaryOp{Op: "+", Left: int1, Right: int2}
	binAdd2 := &ast.BinaryOp{Op: "+", Left: int1, Right: int2}
	binSub := &ast.BinaryOp{Op: "-", Left: int1, Right: int2}
	unaryNeg1 := &ast.UnaryOp{Op: "-", Expr: int1}
	unaryNeg2 := &ast.UnaryOp{Op: "-", Expr: int1}
	cond1 := &ast.ConditionalExpr{Condition: boolTrue, TrueExpr: int1, FalseExpr: int2}
	cond2 := &ast.ConditionalExpr{Condition: boolTrue, TrueExpr: int1, FalseExpr: int2}
	elvis1 := &ast.ElvisExpr{Left: undef, Right: int1}
	elvis2 := &ast.ElvisExpr{Left: undef, Right: int1}
	fn1 := &ast.FunctionCall{Name: "toUpper", Args: []ast.Expr{strFoo}}
	fn2 := &ast.FunctionCall{Name: "TOUPPER", Args: []ast.Expr{strFoo}}
	list12 := &ast.ListLiteral{Elements: []ast.Expr{int1, int2}}
	list21 := &ast.ListLiteral{Elements: []ast.Expr{int2, int1}}
	record1 := &ast.RecordLiteral{ClassAd: &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
		{Name: "x", Value: int1},
		{Name: "y", Value: int2},
	}}}
	record2 := &ast.RecordLiteral{ClassAd: &ast.ClassAd{Attributes: []*ast.AttributeAssignment{
		{Name: "y", Value: int2},
		{Name: "x", Value: int1},
	}}}
	select1 := &ast.SelectExpr{Record: attrFoo, Attr: "Bar"}
	select2 := &ast.SelectExpr{Record: attrfoo, Attr: "bar"}
	subscript1 := &ast.SubscriptExpr{Container: list12, Index: int1}
	subscript2 := &ast.SubscriptExpr{Container: list12, Index: int1}

	cases := []struct {
		name  string
		left  ast.Expr
		right ast.Expr
		equal bool
	}{
		{name: "int equal", left: int1, right: &ast.IntegerLiteral{Value: 1}, equal: true},
		{name: "int not equal", left: int1, right: int2, equal: false},
		{name: "real within tol", left: realNearA, right: realNearB, equal: true},
		{name: "real outside tol", left: realNearA, right: realFar, equal: false},
		{name: "real nan vs nan", left: realNaN, right: &ast.RealLiteral{Value: math.NaN()}, equal: true},
		{name: "real nan vs num", left: realNaN, right: realNearA, equal: false},
		{name: "real inf match", left: realPosInf, right: &ast.RealLiteral{Value: math.Inf(1)}, equal: true},
		{name: "real inf mismatch", left: realPosInf, right: realNegInf, equal: false},
		{name: "string equal", left: strFoo, right: &ast.StringLiteral{Value: "foo"}, equal: true},
		{name: "string diff", left: strFoo, right: strBar, equal: false},
		{name: "bool equal", left: boolTrue, right: &ast.BooleanLiteral{Value: true}, equal: true},
		{name: "bool diff", left: boolTrue, right: boolFalse, equal: false},
		{name: "undefined equal", left: undef, right: &ast.UndefinedLiteral{}, equal: true},
		{name: "undefined vs int", left: undef, right: int1, equal: false},
		{name: "error equal", left: errLit, right: &ast.ErrorLiteral{}, equal: true},
		{name: "attr case-insensitive", left: attrFoo, right: attrfoo, equal: true},
		{name: "binary equal", left: binAdd1, right: binAdd2, equal: true},
		{name: "binary diff op", left: binAdd1, right: binSub, equal: false},
		{name: "unary equal", left: unaryNeg1, right: unaryNeg2, equal: true},
		{name: "unary diff op", left: unaryNeg1, right: &ast.UnaryOp{Op: "!", Expr: int1}, equal: false},
		{name: "conditional equal", left: cond1, right: cond2, equal: true},
		{name: "conditional diff", left: cond1, right: &ast.ConditionalExpr{Condition: boolFalse, TrueExpr: int1, FalseExpr: int2}, equal: false},
		{name: "elvis equal", left: elvis1, right: elvis2, equal: true},
		{name: "elvis diff", left: elvis1, right: &ast.ElvisExpr{Left: int1, Right: int2}, equal: false},
		{name: "func name case-insensitive", left: fn1, right: fn2, equal: true},
		{name: "func args diff", left: fn1, right: &ast.FunctionCall{Name: "toUpper", Args: []ast.Expr{strBar}}, equal: false},
		{name: "list equal", left: list12, right: &ast.ListLiteral{Elements: []ast.Expr{int1, int2}}, equal: true},
		{name: "list order diff", left: list12, right: list21, equal: false},
		{name: "record equal order-insensitive", left: record1, right: record2, equal: true},
		{name: "record diff value", left: record1, right: &ast.RecordLiteral{ClassAd: &ast.ClassAd{Attributes: []*ast.AttributeAssignment{{Name: "x", Value: int2}}}}, equal: false},
		{name: "select attr case-insensitive", left: select1, right: select2, equal: true},
		{name: "subscript equal", left: subscript1, right: subscript2, equal: true},
		{name: "subscript diff", left: subscript1, right: &ast.SubscriptExpr{Container: list12, Index: int2}, equal: false},
	}

	for _, tc := range cases {
		if got := exprFromAST(tc.left).Equal(exprFromAST(tc.right)); got != tc.equal {
			t.Fatalf("%s: expected %v, got %v", tc.name, tc.equal, got)
		}
	}
}

func TestExprEqualNilHandling(t *testing.T) {
	var left *Expr
	var right *Expr
	if !left.Equal(right) {
		t.Fatalf("nil vs nil should be equal")
	}

	nonNil := exprFromAST(&ast.IntegerLiteral{Value: 1})
	if left.Equal(nonNil) {
		t.Fatalf("nil vs non-nil should not be equal")
	}
	if nonNil.Equal(left) {
		t.Fatalf("non-nil vs nil should not be equal")
	}
}

func TestClassAdEqualNilHandling(t *testing.T) {
	var left *ClassAd
	var right *ClassAd
	if !left.Equal(right) {
		t.Fatalf("nil vs nil ClassAd should be equal")
	}

	nonNil := New()
	if left.Equal(nonNil) {
		t.Fatalf("nil vs non-nil ClassAd should not be equal")
	}
	if nonNil.Equal(left) {
		t.Fatalf("non-nil vs nil ClassAd should not be equal")
	}
}
