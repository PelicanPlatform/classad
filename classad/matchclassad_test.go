package classad

import "testing"

func TestMatchClassAdBasic(t *testing.T) {
	left, err := Parse(`[Requirements = TARGET.Memory >= 1024; Rank = 5; Memory = 2048; Name = "job"]`)
	if err != nil {
		t.Fatalf("parse left failed: %v", err)
	}
	right, err := Parse(`[Requirements = TARGET.Memory >= 2048; Rank = 3.5; Memory = 1024; Name = "machine"]`)
	if err != nil {
		t.Fatalf("parse right failed: %v", err)
	}

	match := NewMatchClassAd(left, right)

	if l := match.GetLeftAd(); l == nil {
		t.Fatalf("left ad should be set")
	}
	if r := match.GetRightAd(); r == nil {
		t.Fatalf("right ad should be set")
	}

	if !match.Match() {
		t.Fatalf("expected match to succeed when both requirements true")
	}

	if rank, ok := match.EvaluateRankLeft(); !ok || rank != 5 {
		t.Fatalf("unexpected left rank: %v (ok=%v)", rank, ok)
	}
	if rank, ok := match.EvaluateRankRight(); !ok || rank != 3.5 {
		t.Fatalf("unexpected right rank: %v (ok=%v)", rank, ok)
	}

	expr, err := ParseExpr(`Memory + TARGET.Memory`)
	if err != nil {
		t.Fatalf("parse expr failed: %v", err)
	}
	if val := match.EvaluateExprLeft(expr.internal()); !val.IsNumber() {
		t.Fatalf("expected numeric expression result, got %v", val.Type())
	} else if num, _ := val.NumberValue(); num != 3072 {
		t.Fatalf("unexpected expression result: %g", num)
	}

	exprRight, err := ParseExpr(`TARGET.Memory - Memory`)
	if err != nil {
		t.Fatalf("parse expr right failed: %v", err)
	}
	if val := match.EvaluateExprRight(exprRight.internal()); !val.IsNumber() {
		t.Fatalf("expected numeric result from right expr")
	} else if num, _ := val.NumberValue(); num != 1024 {
		t.Fatalf("unexpected right expr result: %g", num)
	}

	// Replace right with a stricter ad that fails symmetry.
	newRight, err := Parse(`[Requirements = false; Rank = 0; Memory = 512]`)
	if err != nil {
		t.Fatalf("parse new right failed: %v", err)
	}
	match.ReplaceRightAd(newRight)
	if match.Match() {
		t.Fatalf("expected match to fail after replacing right")
	}

	// Replace left with nil and ensure evaluations become undefined.
	match.ReplaceLeftAd(nil)
	if val := match.EvaluateAttrLeft("Memory"); !val.IsUndefined() {
		t.Fatalf("expected undefined when left ad is nil")
	}
	if val := match.EvaluateAttrRight("Memory"); val.IsUndefined() {
		t.Fatalf("right ad should remain accessible")
	}
}

func TestMatchClassAdRankFallback(t *testing.T) {
	left, err := Parse(`[Requirements = true; Rank = "high"]`)
	if err != nil {
		t.Fatalf("parse left failed: %v", err)
	}
	right, err := Parse(`[Requirements = true; Rank = 1]`)
	if err != nil {
		t.Fatalf("parse right failed: %v", err)
	}

	match := NewMatchClassAd(left, right)
	if _, ok := match.EvaluateRankLeft(); ok {
		t.Fatalf("expected non-numeric left rank to return ok=false")
	}
	if rank, ok := match.EvaluateRankRight(); !ok || rank != 1 {
		t.Fatalf("expected numeric right rank, got %v (ok=%v)", rank, ok)
	}
}
