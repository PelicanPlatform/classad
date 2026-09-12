package classad

import (
	"strings"
	"testing"
)

// TestRecoverCyclicStopsThePanic pins the one property RecoverCyclic exists
// for: used as a deferred call, it must turn a cyclic/over-deep evaluation into
// an error value rather than let the panic escape.
//
// It regressed silently. recover() only stops a panic when the deferred
// function calls it directly, and RecoverCyclic used to delegate to
// recoverCyclic -- one frame too deep. Go <=1.25 tolerated that; 1.26 does not,
// so every vm entry point (matcher, wirematch, interp) went from returning
// error to killing the process. Nothing caught it because the only coverage ran
// on a Go that still forgave the mistake.
func TestRecoverCyclicStopsThePanic(t *testing.T) {
	// Deeper than maxEvalDepth, so Evaluate panics with cyclicEvalError.
	src := strings.Repeat("!", maxEvalDepth+1) + "Missing"
	expr, err := ParseExpr(src)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	ad, err := Parse(`[x = 1]`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	got := func() (result Value) {
		defer RecoverCyclic(&result)
		return NewEvaluator(ad).Evaluate(expr.expr)
	}()

	if !got.IsError() {
		t.Fatalf("RecoverCyclic left %v, want an error value", got)
	}
}

// TestRecoverCyclicRepanicsOthers keeps the fix from swallowing unrelated
// panics: only cyclicEvalError becomes an error value.
func TestRecoverCyclicRepanicsOthers(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("RecoverCyclic swallowed a non-cyclic panic")
		}
		if s, ok := r.(string); !ok || s != "unrelated" {
			t.Fatalf("got %#v, want the original panic", r)
		}
	}()
	func() (result Value) {
		defer RecoverCyclic(&result)
		panic("unrelated")
	}()
}
