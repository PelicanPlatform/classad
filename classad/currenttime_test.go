package classad

import (
	"strings"
	"testing"
	"time"
)

// TestCurrentTimeMagic verifies CurrentTime evaluates to ~now when the ad does not define
// it, works inside an arithmetic expression, and is always "defined".
func TestCurrentTimeMagic(t *testing.T) {
	before := time.Now().Unix()
	ad, err := Parse(`[ EnteredHistoryTime = 1000 ]`)
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().Unix() + 1

	now, err := ad.EvaluateAttr("CurrentTime").IntValue()
	if err != nil {
		t.Fatalf("CurrentTime did not evaluate to an int: %v", err)
	}
	if now < before || now > after {
		t.Errorf("CurrentTime = %d, want within [%d, %d]", now, before, after)
	}

	evalExpr := func(s string) Value {
		e, perr := ParseExpr(s)
		if perr != nil {
			t.Fatalf("parse %q: %v", s, perr)
		}
		return e.Eval(ad)
	}

	// Used in an expression: CurrentTime - EnteredHistoryTime.
	if v, _ := evalExpr("CurrentTime - EnteredHistoryTime").IntValue(); v < before-1000 || v > after-1000 {
		t.Errorf("CurrentTime - EnteredHistoryTime = %d, want ~%d", v, now-1000)
	}

	// isUndefined(CurrentTime) is false -- the magic attribute is always defined.
	if b, _ := evalExpr("isUndefined(CurrentTime)").BoolValue(); b {
		t.Error("isUndefined(CurrentTime) = true, want false")
	}

	// Case-insensitive.
	if _, err := ad.EvaluateAttr("currenttime").IntValue(); err != nil {
		t.Error("lowercase currenttime did not resolve to the magic value")
	}
}

// TestCurrentTimeAdOverride verifies an ad that explicitly defines CurrentTime wins.
func TestCurrentTimeAdOverride(t *testing.T) {
	ad, err := Parse(`[ CurrentTime = 42 ]`)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := ad.EvaluateAttr("CurrentTime").IntValue(); err != nil || v != 42 {
		t.Errorf("ad-defined CurrentTime = %d (err %v), want 42", v, err)
	}
}

// TestFoldConstantsCurrentTime verifies FoldConstants replaces CurrentTime with a literal so
// the query optimizer sees a constant threshold (range-probe extraction).
func TestFoldConstantsCurrentTime(t *testing.T) {
	before := time.Now().Unix()
	e, err := ParseExpr(`CompletionDate > CurrentTime - 86400`)
	if err != nil {
		t.Fatal(err)
	}
	folded := FoldConstants(e.internal())
	if strings.Contains(strings.ToLower(folded.String()), "currenttime") {
		t.Errorf("FoldConstants left CurrentTime unfolded: %q", folded.String())
	}
	// The RHS folds to a literal now-86400.
	rhs, _ := ParseExpr(`CurrentTime - 86400`)
	n, err := New().exprToValue(FoldConstants(rhs.internal())).IntValue()
	if err != nil {
		t.Fatalf("CurrentTime - 86400 did not fold to an int literal: %q", FoldConstants(rhs.internal()).String())
	}
	if n < before-86400 || n > time.Now().Unix()-86400+1 {
		t.Errorf("folded CurrentTime-86400 = %d, out of expected range", n)
	}
}
