package classad

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

func TestClassAdAPIAndMathEvaluation(t *testing.T) {
	ad := New()
	ad.InsertAttr("i", 2)
	ad.InsertAttrFloat("f", 2.5)
	ad.InsertAttrString("s", "hi")
	ad.InsertAttrBool("b", true)
	InsertAttrList(ad, "lst", []int64{1, 2, 3})
	child := New()
	child.InsertAttr("c", 5)
	ad.InsertAttrClassAd("child", child)

	if ad.Size() != 6 {
		t.Fatalf("expected 6 attributes, got %d", ad.Size())
	}

	if v := GetOr[int64](ad, "missing", 9); v != 9 {
		t.Fatalf("GetOr default expected 9, got %d", v)
	}
	if v := GetOr[int64](ad, "i", 0); v != 2 {
		t.Fatalf("GetOr existing expected 2, got %d", v)
	}

	if _, ok := ad.Lookup("s"); !ok {
		t.Fatalf("expected to find attribute s")
	}
	if str, ok := ad.EvaluateAttrString("s"); !ok || str != "hi" {
		t.Fatalf("expected EvaluateAttrString hi, got %q ok=%v", str, ok)
	}
	if b, ok := ad.EvaluateAttrBool("b"); !ok || !b {
		t.Fatalf("expected EvaluateAttrBool true, got %v ok=%v", b, ok)
	}
	if num, ok := ad.EvaluateAttrNumber("f"); !ok || num != 2.5 {
		t.Fatalf("expected EvaluateAttrNumber 2.5, got %v ok=%v", num, ok)
	}

	expr := &ast.BinaryOp{
		Op:    "+",
		Left:  &ast.AttributeReference{Name: "i"},
		Right: &ast.AttributeReference{Name: "f"},
	}
	res := ad.EvaluateExpr(expr)
	if !res.IsReal() {
		t.Fatalf("expected real result from i+f")
	}
	if val, _ := res.RealValue(); val != 4.5 {
		t.Fatalf("expected 4.5, got %f", val)
	}

	expr2, err := ParseExpr("MY.i + TARGET.j + round(MY.f)")
	if err != nil {
		t.Fatalf("ParseExpr failed: %v", err)
	}
	target := New()
	target.InsertAttr("j", 3)
	res2 := ad.EvaluateExprWithTarget(expr2, target)
	if !res2.IsInteger() {
		t.Fatalf("expected integer from EvaluateExprWithTarget")
	}
	if val, _ := res2.IntValue(); val != 8 {
		t.Fatalf("expected 8 from MY/TARGET math, got %d", val)
	}

	bin := ad.evaluateBinaryOp("-", NewIntValue(10), NewRealValue(3.5))
	if !bin.IsReal() {
		t.Fatalf("binary op should yield real")
	}
	if val, _ := bin.RealValue(); val != 6.5 {
		t.Fatalf("expected 6.5, got %f", val)
	}

	unary := ad.evaluateUnaryOp("-", NewRealValue(1.25))
	if val, _ := unary.RealValue(); val != -1.25 {
		t.Fatalf("expected -1.25 from unary op, got %f", val)
	}

	if !ad.Delete("s") {
		t.Fatalf("expected to delete attribute s")
	}
	ad.Clear()
	if ad.Size() != 0 {
		t.Fatalf("expected clear to remove all attributes")
	}
	if len(ad.GetAttributes()) != 0 {
		t.Fatalf("expected no attribute names after clear")
	}
}
