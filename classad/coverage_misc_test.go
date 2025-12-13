package classad

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

func TestClassAdStringEmpty(t *testing.T) {
	var ad ClassAd
	if ad.String() != "[]" {
		t.Fatalf("expected empty ClassAd string to be [] but got %q", ad.String())
	}
}

func TestExprStringUndefined(t *testing.T) {
	var expr Expr
	if expr.String() != "undefined" {
		t.Fatalf("expected undefined Expr string but got %q", expr.String())
	}
}

func TestInsertExprNilNoop(t *testing.T) {
	ad := &ClassAd{}
	ad.InsertExpr("ignored", nil)
	if ad.Size() != 0 {
		t.Fatalf("expected insert of nil Expr to be ignored; size=%d", ad.Size())
	}
}

func TestEvaluateExprWithTargetNil(t *testing.T) {
	ad := &ClassAd{}
	result := ad.EvaluateExprWithTarget(nil, &ClassAd{})
	if !result.IsUndefined() {
		t.Fatalf("expected undefined when evaluating nil expr, got %v", result)
	}
}

func TestFlattenBinaryWithMissingRef(t *testing.T) {
	ad := &ClassAd{}
	ad.InsertAttr("A", 2)

	expr, err := ParseExpr("A + B")
	if err != nil {
		t.Fatalf("failed to parse expression: %v", err)
	}

	flattened := ad.Flatten(expr)
	if flattened == nil {
		t.Fatalf("expected flattened expression, got nil")
	}

	bin, ok := flattened.internal().(*ast.BinaryOp)
	if !ok {
		t.Fatalf("expected binary op, got %T", flattened.internal())
	}

	left, ok := bin.Left.(*ast.IntegerLiteral)
	if !ok || left.Value != 2 {
		t.Fatalf("expected left literal 2, got %T with value %v", bin.Left, bin.Left)
	}

	right, ok := bin.Right.(*ast.AttributeReference)
	if !ok || right.Name != "B" {
		t.Fatalf("expected right attribute reference B, got %T with name %v", bin.Right, right.Name)
	}
}
