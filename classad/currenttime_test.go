package classad

import (
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

// TestFoldConstantsCurrentTime verifies FoldConstants leaves CurrentTime alone (an ad may
// define it, so it is not a constant) and FoldConstantsAt replaces it with the given time,
// so the query optimizer sees a constant threshold (range-probe extraction).
func TestFoldConstantsCurrentTime(t *testing.T) {
	e, err := ParseExpr(`CompletionDate > CurrentTime - 86400`)
	if err != nil {
		t.Fatal(err)
	}
	if got := FoldConstants(e.internal()).String(); got != "(CompletionDate > (CurrentTime - 86400))" {
		t.Errorf("FoldConstants = %q, want CurrentTime kept", got)
	}
	const now = 1700000000
	if got := FoldConstantsAt(e.internal(), now).String(); got != "(CompletionDate > 1699913600)" {
		t.Errorf("FoldConstantsAt = %q, want CurrentTime folded to %d", got, now)
	}
	my, _ := ParseExpr(`MY.CurrentTime`)
	if got := FoldConstantsAt(my.internal(), now).String(); got != "1700000000" {
		t.Errorf("FoldConstantsAt(MY.CurrentTime) = %q, want %d", got, now)
	}
	tgt, _ := ParseExpr(`TARGET.CurrentTime`)
	if got := FoldConstantsAt(tgt.internal(), now).String(); got != "TARGET.CurrentTime" {
		t.Errorf("FoldConstantsAt(TARGET.CurrentTime) = %q, want the reference kept", got)
	}
}

// TestPinCurrentTime verifies a pinned evaluator resolves an undefined CurrentTime to the
// pinned time, and an ad's own CurrentTime still wins.
func TestPinCurrentTime(t *testing.T) {
	e, _ := ParseExpr(`CurrentTime + MY.CurrentTime`)
	ev := NewEvaluator(New())
	ev.PinCurrentTime(1000)
	if v, _ := ev.Evaluate(e.internal()).IntValue(); v != 2000 {
		t.Errorf("pinned CurrentTime + MY.CurrentTime = %d, want 2000", v)
	}
	ad, _ := Parse(`[CurrentTime = 7]`)
	ev.SetScope(ad)
	if v, _ := ev.Evaluate(e.internal()).IntValue(); v != 14 {
		t.Errorf("ad-defined CurrentTime + MY.CurrentTime = %d, want 14", v)
	}
}
