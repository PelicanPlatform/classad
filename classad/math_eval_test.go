package classad

import (
	"fmt"
	"testing"
)

func evalExpr(t *testing.T, expr string) Value {
	t.Helper()
	ad := New()
	wrapped := fmt.Sprintf("[__tmp__ = %s]", expr)
	val, err := ad.EvaluateExprString(wrapped)
	if err != nil {
		t.Fatalf("parse/eval failed for %q: %v", expr, err)
	}
	return val
}

func TestEvaluateExprMathFunctions(t *testing.T) {
	floorVal := evalExpr(t, "floor(3.7)")
	if !floorVal.IsInteger() {
		t.Fatalf("floor result not integer: %v", floorVal.Type())
	}
	if got, _ := floorVal.IntValue(); got != 3 {
		t.Fatalf("floor(3.7) = %d, want 3", got)
	}

	ceilVal := evalExpr(t, "ceiling(3.2)")
	if got, _ := ceilVal.IntValue(); got != 4 {
		t.Fatalf("ceiling(3.2) = %d, want 4", got)
	}

	roundVal := evalExpr(t, "round(3.5)")
	if got, _ := roundVal.IntValue(); got != 4 {
		t.Fatalf("round(3.5) = %d, want 4", got)
	}

	powVal := evalExpr(t, "pow(2, -2)")
	if !powVal.IsReal() {
		t.Fatalf("pow negative exponent should be real")
	}
	if got, _ := powVal.RealValue(); got != 0.25 {
		t.Fatalf("pow(2,-2) = %f, want 0.25", got)
	}

	quantizeVal := evalExpr(t, "quantize(12, {5, 10, 15})")
	if got, _ := quantizeVal.IntValue(); got != 15 {
		t.Fatalf("quantize(12,{5,10,15}) = %d, want 15", got)
	}

	sumVal := evalExpr(t, "sum({1, 2, 3.5})")
	if !sumVal.IsReal() {
		t.Fatalf("sum mixed numeric should be real")
	}
	if got, _ := sumVal.RealValue(); got != 6.5 {
		t.Fatalf("sum({1,2,3.5}) = %f, want 6.5", got)
	}
}

func TestEvaluateExprMathErrorPaths(t *testing.T) {
	errVal := evalExpr(t, "floor(\"x\")")
	if !errVal.IsError() {
		t.Fatalf("expected floor on string to be error, got %v", errVal.Type())
	}

	undefVal := evalExpr(t, "floor(undefined)")
	if !undefVal.IsUndefined() {
		t.Fatalf("expected undefined from floor(undefined), got %v", undefVal.Type())
	}

	powErr := evalExpr(t, "pow(\"x\", 2)")
	if !powErr.IsError() {
		t.Fatalf("expected error from pow with string base")
	}

	quantizeErr := evalExpr(t, "quantize(\"x\", 3)")
	if !quantizeErr.IsError() {
		t.Fatalf("expected error from quantize non-numeric input")
	}

	quantizeUndef := evalExpr(t, "quantize(12, {undefined})")
	if !quantizeUndef.IsUndefined() {
		t.Fatalf("expected undefined from quantize with all undefined list entries")
	}
}

func TestEvaluateExprUnaryAndDivide(t *testing.T) {
	plusVal := evalExpr(t, "+2.5")
	if !plusVal.IsReal() {
		t.Fatalf("expected real from unary plus")
	}
	if got, _ := plusVal.RealValue(); got != 2.5 {
		t.Fatalf("+2.5 = %f, want 2.5", got)
	}

	minusVal := evalExpr(t, "-(4)")
	if got, _ := minusVal.IntValue(); got != -4 {
		t.Fatalf("-(4) = %d, want -4", got)
	}

	notErr := evalExpr(t, "!1")
	if !notErr.IsError() {
		t.Fatalf("expected error from logical not on non-bool")
	}

	plErr := evalExpr(t, "+\"str\"")
	if !plErr.IsError() {
		t.Fatalf("expected error from unary plus on string")
	}

	boolNot := evalExpr(t, "!false")
	if got, _ := boolNot.BoolValue(); !got {
		t.Fatalf("!false expected true, got %v", got)
	}

	cmpVal := evalExpr(t, "\"b\" < \"c\"")
	if got, _ := cmpVal.BoolValue(); !got {
		t.Fatalf("expected string comparison to be true")
	}

	divZero := evalExpr(t, "10 / 0")
	if !divZero.IsError() {
		t.Fatalf("division by zero should be error")
	}
}
