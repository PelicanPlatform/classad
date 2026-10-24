package classad

import (
	"fmt"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/ast"
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

// ctNow is the time the CurrentTime tables pin, so a value that reads the clock is exact.
const ctNow = 1_700_000_000

// ctStates are the ways an ad can hold CurrentTime: absent, defined as undefined, a
// literal, an expression over the ad's own Base, and an expression that is undefined.
// want is the ad's CurrentTime at ctNow: an int, or -1 for undefined.
var ctStates = []struct {
	name string
	attr string // the ad's CurrentTime assignment, or "" for none
	want int64
}{
	{"absent", "", ctNow},
	{"undefined", "CurrentTime = undefined;", -1},
	{"literal", "CurrentTime = 7;", 7},
	{"expression", "CurrentTime = Base + 1;", -2}, // Base + 1: the ad's own Base
	{"undefined expression", "CurrentTime = NoSuchAttr;", -1},
}

func ctWant(t *testing.T, got Value, want int64) {
	t.Helper()
	if want == -1 {
		if !got.IsUndefined() {
			t.Errorf("got %v, want undefined", got)
		}
		return
	}
	if v, err := got.IntValue(); err != nil || v != want {
		t.Errorf("got %v, want %d", got, want)
	}
}

func ctEval(t *testing.T, scope *ClassAd, src string) Value {
	t.Helper()
	e, err := ParseExpr(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	ev := NewEvaluator(scope)
	ev.PinCurrentTime(ctNow)
	return ev.Evaluate(e.internal())
}

// TestCurrentTimeScopes is libclassad's CurrentTime under HTCondor's old ClassAd
// semantics (ClassAd::LookupInScope): the ad a reference names supplies its own
// CurrentTime whatever its value, and one that lacks it gives the time. An unscoped or
// MY.CurrentTime never reads the match target's; TARGET.CurrentTime is the target's, the
// time when the target lacks it, and undefined with no target.
func TestCurrentTimeScopes(t *testing.T) {
	adFor := func(st string, base int) *ClassAd {
		ad, err := Parse(fmt.Sprintf("[Base = %d; %s N = 1]", base, st))
		if err != nil {
			t.Fatal(err)
		}
		return ad
	}
	want := func(w int64, base int) int64 {
		if w == -2 {
			return int64(base + 1)
		}
		return w
	}
	for _, st := range ctStates {
		t.Run("standalone/"+st.name, func(t *testing.T) {
			ad := adFor(st.attr, 10)
			ctWant(t, ctEval(t, ad, "CurrentTime"), want(st.want, 10))
			ctWant(t, ctEval(t, ad, "MY.CurrentTime"), want(st.want, 10))
			ctWant(t, ctEval(t, ad, "TARGET.CurrentTime"), -1)
		})
		for _, sst := range ctStates {
			t.Run("match/job "+st.name+"/slot "+sst.name, func(t *testing.T) {
				job, slot := adFor(st.attr, 10), adFor(sst.attr, 20)
				NewMatchClassAd(job, slot)
				// The job's references: its own CurrentTime, never the slot's.
				ctWant(t, ctEval(t, job, "CurrentTime"), want(st.want, 10))
				ctWant(t, ctEval(t, job, "MY.CurrentTime"), want(st.want, 10))
				// TARGET.CurrentTime: the slot's, evaluated in the slot.
				ctWant(t, ctEval(t, job, "TARGET.CurrentTime"), want(sst.want, 20))
				// The slot's references, the other way round.
				ctWant(t, ctEval(t, slot, "CurrentTime"), want(sst.want, 20))
				ctWant(t, ctEval(t, slot, "TARGET.CurrentTime"), want(st.want, 10))
			})
		}
	}
}

// TestCurrentTimeMatchClassAd checks the same through MatchClassAd's own evaluation,
// which reads the clock: a value above 1e9 is the time.
func TestCurrentTimeMatchClassAd(t *testing.T) {
	job, _ := Parse(`[r1 = CurrentTime > 1000000000; r2 = TARGET.CurrentTime > 1000000000;
		r3 = MY.CurrentTime > 1000000000; Requirements = TARGET.N > 2 || CurrentTime < 1000]`)
	for _, tc := range []struct {
		slot           string
		r1, r2, r3, ok string
	}{
		{`[N = 1; CurrentTime = 5]`, "true", "false", "true", "false"},
		{`[N = 1]`, "true", "true", "true", "false"},
		{`[N = 1; CurrentTime = undefined]`, "true", "undefined", "true", "false"},
		{`[N = 3; CurrentTime = 5; Requirements = CurrentTime > 1000000000]`, "true", "false", "true", "true"},
	} {
		slot, _ := Parse(tc.slot)
		m := NewMatchClassAd(job, slot)
		for attr, want := range map[string]string{"r1": tc.r1, "r2": tc.r2, "r3": tc.r3, "Requirements": tc.ok} {
			if got := m.EvaluateAttrLeft(attr).String(); got != want {
				t.Errorf("slot %s: %s = %s, want %s", tc.slot, attr, got, want)
			}
		}
	}
	// The slot's own Requirements reads its own CurrentTime, not the job's.
	slot, _ := Parse(`[Requirements = CurrentTime > 1000000000]`)
	jobWith, _ := Parse(`[CurrentTime = 5]`)
	if got := NewMatchClassAd(jobWith, slot).EvaluateAttrRight("Requirements").String(); got != "true" {
		t.Errorf("slot Requirements over a job with CurrentTime = 5: %s, want true", got)
	}
}

// TestCurrentTimeNestedScopes covers a select and a nested record: the record is the ad
// the lookup starts in, and one without CurrentTime gives the time even when an enclosing
// ad has it. A chained parent (SetParent) is read as part of the ad.
func TestCurrentTimeNestedScopes(t *testing.T) {
	for src, want := range map[string]int64{
		`[CurrentTime = undefined; r = CurrentTime].r`:               -1,
		`[CurrentTime = NoSuchAttr; r = CurrentTime].r`:              -1,
		`[CurrentTime = 5; a = [x = 1]; r = a.CurrentTime].r`:        ctNow,
		`[CurrentTime = 5; a = [r = CurrentTime]; z = a.r].z`:        ctNow,
		`[CurrentTime = 5; a = [r = MY.CurrentTime]; z = a.r].z`:     ctNow,
		`[a = [CurrentTime = 7]; r = a.CurrentTime].r`:               7,
		`[a = [CurrentTime = undefined]; r = a.CurrentTime].r`:       -1,
		`[CurrentTime = 5; a = [r = PARENT.CurrentTime]; z = a.r].z`: 5,
		`[a = [r = PARENT.CurrentTime]; z = a.r].z`:                  ctNow,
		`{[x = 1], [CurrentTime = 7]}.CurrentTime[0]`:                ctNow,
		`{[x = 1], [CurrentTime = 7]}.CurrentTime[1]`:                7,
		`PARENT.CurrentTime`:                                         -1,
	} {
		t.Run(src, func(t *testing.T) { ctWant(t, ctEval(t, New(), src), want) })
	}

	parent, _ := Parse(`[CurrentTime = 7]`)
	child, _ := Parse(`[x = 1]`)
	child.SetParent(parent)
	ctWant(t, ctEval(t, child, "CurrentTime"), 7)
	ctWant(t, ctEval(t, child, "MY.CurrentTime"), 7)
	if v, _ := child.EvaluateAttr("CurrentTime").IntValue(); v != 7 {
		t.Errorf("EvaluateAttr(CurrentTime) on a chained ad = %d, want the parent's 7", v)
	}
	if got := child.Flatten(mustExpr(t, "CurrentTime")).String(); got != "CurrentTime" {
		t.Errorf("Flatten on a chained ad = %s, want the reference kept", got)
	}
}

func mustExpr(t *testing.T, src string) *Expr {
	t.Helper()
	e, err := ParseExpr(src)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// TestCurrentTimeResolver covers the resolver contract: undefined from the resolver for
// an unscoped or MY CurrentTime is "absent" (the time); for TARGET it is returned as is.
func TestCurrentTimeResolver(t *testing.T) {
	ev := NewEvaluator(nil)
	ev.PinCurrentTime(ctNow)
	ev.SetResolver(func(string, ast.AttributeScope) Value { return NewUndefinedValue() })
	for src, want := range map[string]int64{"CurrentTime": ctNow, "MY.CurrentTime": ctNow, "TARGET.CurrentTime": -1} {
		ctWant(t, ev.Evaluate(mustExpr(t, src).internal()), want)
	}
	ev.SetResolver(func(string, ast.AttributeScope) Value { return NewIntValue(3) })
	ctWant(t, ev.Evaluate(mustExpr(t, "CurrentTime").internal()), 3)
}
