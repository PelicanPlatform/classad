package classad

import "testing"

func TestEvaluateExprWithTargetCustom(t *testing.T) {
	scope := New()
	scope.InsertAttr("A", 1)
	target := New()
	target.InsertAttr("B", 5)

	expr, err := ParseExpr("A + TARGET.B")
	if err != nil {
		t.Fatalf("parse expr failed: %v", err)
	}

	val := scope.EvaluateExprWithTarget(expr, target)
	if val.IsError() || val.IsUndefined() {
		t.Fatalf("expected defined value, got %v", val.Type())
	}
	if num, _ := val.IntValue(); num != 6 {
		t.Fatalf("unexpected evaluation result: %d", num)
	}
}
