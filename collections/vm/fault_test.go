package vm

import (
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

func pe(t *testing.T, src string) ast.Expr {
	t.Helper()
	e, err := parser.ParseExpr(src)
	if err != nil || e == nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return e
}

// TestFaultIsDistinguishableFromAnErrorValue is the reason the report exists. Both runs below return
// ERROR, and nothing about the value says which is which.
func TestFaultIsDistinguishableFromAnErrorValue(t *testing.T) {
	scope, err := classad.Parse(`[ Cpus = 4 ]`)
	if err != nil {
		t.Fatal(err)
	}

	// A script whose RESULT EXPRESSION evaluates to ERROR. Nothing faulted: the script ran to the
	// end and that is the answer it computed.
	computed, err := NewScript().Result(pe(t, `"a" + 1`)).Compile()
	if err != nil {
		t.Fatal(err)
	}
	v, ok, fault := computed.RunWithFault(scope)
	if !ok {
		t.Error("the script did not finish, but it has no loop and no budget pressure")
	}
	if v.String() != "error" {
		t.Errorf("value = %s, want error", v.String())
	}
	if fault != nil {
		t.Errorf("fault = %v, want none: an expression producing ERROR is not a fault", fault.Error())
	}

	// A script whose IF CONDITION is undefined. Same value, different kind of thing.
	faulted, err := NewScript().
		If(pe(t, "Missing"), func(s *Script) { s.Let("x", pe(t, "1")) }, nil).
		Result(pe(t, "2")).Compile()
	if err != nil {
		t.Fatal(err)
	}
	v2, ok2, fault2 := faulted.RunWithFault(scope)
	if !ok2 {
		t.Error("ok=false, but the budget was not exhausted; a fault is not an abort")
	}
	if v2.String() != "error" {
		t.Errorf("value = %s, want error", v2.String())
	}
	if fault2 == nil {
		t.Fatal("no fault reported for an undefined if condition")
	}
	if fault2.Class != FaultCondition {
		t.Errorf("class = %v, want %v", fault2.Class, FaultCondition)
	}
	if !strings.Contains(fault2.Where, "if") {
		t.Errorf("Where = %q, want it to name the construct", fault2.Where)
	}
}

// TestFaultNamesWhichConstruct pins that the site identifies one construct among several, which is
// the whole point of a site table -- a report that said only "a condition faulted" would be no help
// in a script with five of them.
func TestFaultNamesWhichConstruct(t *testing.T) {
	scope, err := classad.Parse(`[ Cpus = 4 ]`)
	if err != nil {
		t.Fatal(err)
	}
	// Three ifs; the SECOND one's condition is the undefined one.
	sp, err := NewScript().
		If(pe(t, "Cpus > 2"), func(s *Script) { s.Let("a", pe(t, "1")) }, nil).
		If(pe(t, "Missing"), func(s *Script) { s.Let("b", pe(t, "2")) }, nil).
		If(pe(t, "Cpus > 1"), func(s *Script) { s.Let("c", pe(t, "3")) }, nil).
		Result(pe(t, "4")).Compile()
	if err != nil {
		t.Fatal(err)
	}
	_, _, fault := sp.RunWithFault(scope)
	if fault == nil {
		t.Fatal("no fault reported")
	}
	if !strings.Contains(fault.Where, "#2") {
		t.Errorf("Where = %q, want it to name the second if", fault.Where)
	}
	if strings.Contains(fault.Where, "while") {
		t.Errorf("Where = %q names a while; the site table confused the two kinds", fault.Where)
	}
}

// TestWhileFaultIsItsOwnKind pins that the two constructs are counted separately, so "while #1" in a
// script that also has ifs means the first while and not the first fault site.
func TestWhileFaultIsItsOwnKind(t *testing.T) {
	scope, err := classad.Parse(`[ Cpus = 4 ]`)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := NewScript().
		If(pe(t, "Cpus > 2"), func(s *Script) { s.Let("a", pe(t, "1")) }, nil).
		While(pe(t, "Owner"), func(s *Script) { s.Let("b", pe(t, "2")) }).
		Result(pe(t, "3")).Compile()
	if err != nil {
		t.Fatal(err)
	}
	_, _, fault := sp.RunWithFault(scope)
	if fault == nil {
		t.Fatal("no fault reported for a string while condition")
	}
	if !strings.Contains(fault.Where, "while #1") {
		t.Errorf("Where = %q, want `while #1`: the kinds are counted separately", fault.Where)
	}
}

// TestBudgetExhaustionIsNotAFault pins the design's distinction. A runaway loop did not finish, so
// there is no construct to blame -- and reporting one would invite a caller to treat an operational
// limit as a property of the script's logic.
func TestBudgetExhaustionIsNotAFault(t *testing.T) {
	scope, err := classad.Parse(`[ Cpus = 4 ]`)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := NewScript().
		While(pe(t, "true"), func(s *Script) { s.Let("x", pe(t, "x + 1")) }).
		Result(pe(t, "x")).Compile()
	if err != nil {
		t.Fatal(err)
	}
	_, ok, fault := RunRegistersFault(sp.RegProgram(), scope, 500)
	if ok {
		t.Error("ok=true for an unbounded loop under a 500-step budget")
	}
	if fault != nil {
		t.Errorf("fault = %v, want none: exhausting the budget is an abort, not a fault", fault.Error())
	}
}

// TestFaultSiteSurvivesTheOptimizer guards the way this would break silently: the optimizer rewrites
// the instruction stream, and an RFault whose operand it dropped would report the wrong construct --
// or, with the operand zeroed, always the first one.
func TestFaultSiteSurvivesTheOptimizer(t *testing.T) {
	scope, err := classad.Parse(`[ Cpus = 4 ]`)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := NewScript().
		If(pe(t, "Cpus > 2"), func(s *Script) { s.Let("a", pe(t, "1")) }, nil).
		If(pe(t, "Missing"), func(s *Script) { s.Let("b", pe(t, "2")) }, nil).
		Result(pe(t, "3")).Compile()
	if err != nil {
		t.Fatal(err)
	}
	_, _, want := sp.RunWithFault(scope)
	if want == nil {
		t.Fatal("no fault before optimization")
	}
	_, _, got := RunRegistersFault(Optimize(sp.RegProgram(), AllPasses()), scope, DefaultStepBudget)
	if got == nil {
		t.Fatal("no fault after optimization")
	}
	if got.Where != want.Where || got.Class != want.Class {
		t.Errorf("optimized fault = %q/%v, unoptimized = %q/%v", got.Where, got.Class, want.Where, want.Class)
	}
}

// TestExpressionsHaveNoFaultSites pins the layer boundary: an ordinary expression cannot fault, so a
// program compiled from one carries no site table at all.
func TestExpressionsHaveNoFaultSites(t *testing.T) {
	for _, src := range []string{`Missing + 1`, `"a" + 1`, `Missing ?: 7`, `ifThenElse(Missing, 1, 2)`} {
		p := CompileProgram(pe(t, src))
		if len(p.faultSites) != 0 {
			t.Errorf("%s: %d fault sites, want 0: expressions are total", src, len(p.faultSites))
		}
		scope, err := classad.Parse(`[]`)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, fault := RunRegistersFault(LowerToRegisters(p), scope, DefaultStepBudget); fault != nil {
			t.Errorf("%s: reported a fault (%v); an expression cannot fault", src, fault.Error())
		}
	}
}

// TestFaultAtRejectsAnUnknownSite covers the defensive branch directly, because its whole value is
// in a case no program produces. Returning nil there would turn a fault that DID happen into a clean
// result, which is the one outcome the report exists to prevent.
func TestFaultAtRejectsAnUnknownSite(t *testing.T) {
	p := &Program{}
	for _, site := range []int32{-1, 0, 7} {
		f := p.faultAt(site)
		if f == nil {
			t.Errorf("faultAt(%d) = nil on an empty site table; a fault would be reported as success", site)
			continue
		}
		if f.Class != FaultCondition {
			t.Errorf("faultAt(%d) class = %v, want a real class", site, f.Class)
		}
	}
	// And a recorded site still reads back correctly, so the guard did not swallow the normal path.
	q := &Program{}
	if idx := q.addFaultSite(FaultCondition, "if"); idx != 0 {
		t.Fatalf("first site index = %d, want 0", idx)
	}
	if got := q.faultAt(0).Where; got != "if #1 condition" {
		t.Errorf("Where = %q, want %q", got, "if #1 condition")
	}
}
