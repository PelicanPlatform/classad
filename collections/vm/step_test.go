package vm

import (
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// chargeCorpus is every expression corpus the package maintains, against every scope it maintains.
// The claim being tested is about the LOWERING rather than about any one expression, so the widest
// available input is the right one.
func chargeCorpus(t testing.TB) (exprs []string, ads []*classad.ClassAd) {
	t.Helper()
	exprs = append(exprs, vecExprs...)
	exprs = append(exprs, exprSources...)
	exprs = append(exprs, parityCorpusExprs...)
	exprs = append(exprs, dictExprs...)
	exprs = append(exprs, reductionOperands...)
	for _, src := range scopeSources {
		if ad, err := classad.Parse(src); err == nil && ad != nil {
			ads = append(ads, ad)
		}
	}
	vc := vecCorpus()
	for i := range vc.recs {
		ads = append(ads, vc.ad(i))
	}
	dc := dictCorpus()
	for i := range dc.recs {
		ads = append(ads, dc.ad(i))
	}
	return exprs, ads
}

// stackSteps and regSteps run one program and report what it was charged.
func stackSteps(p *Program, ad *classad.ClassAd) int {
	ev := classad.NewEvaluator(ad)
	ev.PinCurrentTime(p.now)
	var n int
	func() {
		var ignored classad.Value
		defer classad.RecoverCyclic(&ignored)
		_, _, n, _ = exec(p, ev, make([]classad.Value, 0, 16), DefaultStepBudget)
	}()
	return n
}

func regSteps(rp *RegProgram, ad *classad.ClassAd) int {
	ev := classad.NewEvaluator(ad)
	var n int
	func() {
		var ignored classad.Value
		defer classad.RecoverCyclic(&ignored)
		_, _, _, n = execReg(rp, ev, make([]classad.Value, rp.nregs), DefaultStepBudget)
	}()
	return n
}

// TestChargeIsTheSameInBothForms is the claim the whole definition rests on: a record's charge is a
// property of the EXPRESSION, not of the instruction set it was lowered to.
//
// It matters because the three executors span two instruction sets -- the stack interpreter and the
// vector executor share one, the register executor has its own -- and the charge is the quantity they
// are meant to be compared on. If the lowering changed it, a divergence between two executors could
// never be attributed: it would always be arguable that the forms simply count differently.
//
// This is measured over every corpus rather than argued from the lowering, because the lowering is
// exactly the thing that would drift.
func TestChargeIsTheSameInBothForms(t *testing.T) {
	exprs, ads := chargeCorpus(t)
	pairs, varied := 0, map[int]bool{}
	for _, src := range exprs {
		expr, err := parser.ParseExpr(src)
		if err != nil || expr == nil {
			continue
		}
		p := CompileProgram(expr)
		rp := LowerToRegisters(p)
		for _, ad := range ads {
			s, r := stackSteps(p, ad), regSteps(rp, ad)
			pairs++
			varied[s] = true
			if s != r {
				t.Errorf("%q: stack charged %d, register charged %d", src, s, r)
			}
		}
	}
	if pairs == 0 {
		t.Fatal("no pairs compared")
	}
	// Guard against a vacuous pass: if every program charged the same amount -- zero, say, because
	// nothing was classified as a value operation -- agreement would prove nothing.
	if len(varied) < 5 {
		t.Errorf("only %d distinct charges over %d pairs; the counter is not tracking anything",
			len(varied), pairs)
	}
	t.Logf("%d record-expression pairs agree, %d distinct charges", pairs, len(varied))
}

// TestEveryOpcodeIsClassified is the tripwire. An opcode in neither mask is UNCHARGED, and an
// uncharged instruction inside a loop body is an execution that does not terminate -- so the failure
// mode of forgetting one is a hang, which is the worst kind to debug.
//
// It walks the opcodes the compiler actually emits, in both forms, the way the optimizer's own
// tripwire does, because asserting over a hand-written list would be asserting over the list.
func TestEveryOpcodeIsClassified(t *testing.T) {
	exprs, _ := chargeCorpus(t)
	seen, seenReg := map[Opcode]bool{}, map[RegOp]bool{}
	check := func(p *Program) {
		rp := LowerToRegisters(p)
		for _, in := range p.code {
			seen[in.Op] = true
			if !isClassified(in.Op) {
				t.Errorf("stack opcode %d is in neither mask, so it is silently uncharged", in.Op)
			}
		}
		for _, in := range rp.code {
			seenReg[in.Op] = true
			if !isRegClassified(in.Op) {
				t.Errorf("register opcode %d is in neither mask, so it is silently uncharged", in.Op)
			}
		}
	}
	for _, src := range exprs {
		if expr, err := parser.ParseExpr(src); err == nil && expr != nil {
			check(CompileProgram(expr))
		}
	}
	// Scripts, which are where the statement opcodes come from; an expression corpus reaches none of
	// them, and they are the ones a loop's termination depends on.
	one, _ := parser.ParseExpr("1")
	cond, _ := parser.ParseExpr("Cpus > 2")
	res, _ := parser.ParseExpr("t")
	sp, err := NewScript().Let("t", one).
		If(cond, func(s *Script) { s.Let("t", one) }, func(s *Script) { s.Let("t", one) }).
		While(cond, func(s *Script) { s.Let("t", one) }).
		Result(res).Compile()
	if err != nil {
		t.Fatal(err)
	}
	check(sp.Program())
	t.Logf("%d stack opcodes and %d register opcodes classified", len(seen), len(seenReg))
}

// TestEveryLoopIterationIsCharged pins termination at its real source, which is the COMPILER and not
// the interpreter.
//
// Charging only value operations means a loop whose body and condition contained none would run
// forever. That cannot happen because every back edge passes through a condition expression and the
// smallest expression is a literal, which is charged. This asserts it against the smallest loop the
// builder can make rather than trusting the argument.
func TestEveryLoopIterationIsCharged(t *testing.T) {
	alwaysTrue, err := parser.ParseExpr("true")
	if err != nil {
		t.Fatal(err)
	}
	res, err := parser.ParseExpr("1")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := NewScript().While(alwaysTrue, nil).Result(res).Compile()
	if err != nil {
		t.Fatal(err)
	}
	scope, err := classad.Parse(`[]`)
	if err != nil {
		t.Fatal(err)
	}
	// An empty-bodied infinite loop: the condition alone has to be enough to exhaust the budget.
	//
	// Run on another goroutine with a deadline, because the failure being guarded against is that the
	// loop NEVER RETURNS. Calling it directly would hang the test binary until the CI job timed out,
	// with no indication of which test was stuck -- the worst possible report for the worst possible
	// bug. The goroutine is leaked if the budget is broken; that is acceptable in a test that has
	// already failed, and far better than no message.
	const budget = 5000
	type outcome struct {
		val classad.Value
		ok  bool
	}
	done := make(chan outcome, 1)
	go func() {
		v, ok := sp.RunLimited(scope, budget)
		done <- outcome{v, ok}
	}()
	select {
	case got := <-done:
		if got.ok {
			t.Fatalf("`while true` with an empty body finished, returning %s", got.val)
		}
		if !got.val.IsError() {
			t.Errorf("budget exhaustion gave %s, want error", got.val)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("`while true` with an empty body did not return: an iteration is being charged nothing, " +
			"so the budget can never be reached")
	}
	// And the charge really does grow with the iteration count, rather than the loop being stopped by
	// something else that happens to look like a budget.
	var prev int
	for _, limit := range []string{"2", "20"} {
		x0, _ := parser.ParseExpr("0")
		inc, _ := parser.ParseExpr("x + 1")
		lt, _ := parser.ParseExpr("x < " + limit)
		rx, _ := parser.ParseExpr("x")
		loop, err := NewScript().Let("x", x0).
			While(lt, func(s *Script) { s.Let("x", inc) }).
			Result(rx).Compile()
		if err != nil {
			t.Fatal(err)
		}
		if got := loop.Run(scope).String(); got != limit {
			t.Fatalf("loop to %s returned %s", limit, got)
		}
		n := regSteps(loop.RegProgram(), scope)
		if n <= prev {
			t.Errorf("loop to %s charged %d, not more than the shorter loop's %d", limit, n, prev)
		}
		prev = n
	}
}

// TestBudgetStopsBothFormsTogether pins that the budget means the same thing in both instruction
// sets, which follows from the charge agreeing but is the part a caller actually depends on.
func TestBudgetStopsBothFormsTogether(t *testing.T) {
	expr, err := parser.ParseExpr("Cpus + Memory * 2 - Cpus / 2 + Memory")
	if err != nil {
		t.Fatal(err)
	}
	scope, err := classad.Parse(`[Cpus = 4; Memory = 8]`)
	if err != nil {
		t.Fatal(err)
	}
	p := CompileProgram(expr)
	rp := LowerToRegisters(p)
	full := stackSteps(p, scope)
	if full < 4 {
		t.Fatalf("the expression charges only %d; it is too small to test a budget", full)
	}
	for budget := 1; budget <= full+1; budget++ {
		_, okStack := RunLimited(p, scope, budget)
		_, okReg := RunRegistersLimited(rp, scope, budget)
		if okStack != okReg {
			t.Errorf("budget %d: stack finished=%v, register finished=%v", budget, okStack, okReg)
		}
		if want := budget >= full; okStack != want {
			t.Errorf("budget %d of %d charged: finished=%v, want %v", budget, full, okStack, want)
		}
	}
}
