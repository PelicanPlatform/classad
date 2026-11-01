package vm

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// countOp returns how many instructions of a given opcode a program has.
func countOp(rp *RegProgram, op RegOp) int {
	n := 0
	for _, in := range rp.code {
		if in.Op == op {
			n++
		}
	}
	return n
}

// optAgrees checks the optimizer's law on one expression in one scope: the optimized program must
// produce exactly what the unoptimized one produces.
//
// The unoptimized program is the oracle, and it is itself checked against the stack interpreter
// (FuzzRegisterParity), which is checked against the tree-walking evaluator, which is checked
// against the C++ engine. A pass that breaks anything lands on that chain rather than on a second
// opinion about what the answer should be.
func optAgrees(t *testing.T, scopeSrc, exprSrc string) {
	t.Helper()
	scope, err := classad.Parse(scopeSrc)
	if err != nil || scope == nil {
		return
	}
	expr, err := parser.ParseExpr(exprSrc)
	if err != nil || expr == nil {
		return
	}
	rp := LowerToRegisters(CompileProgram(expr))
	want := RunRegisters(rp, scope)
	got := RunRegisters(Optimize(rp, AllPasses()), scope)
	if !valuesEqual(want, got) {
		t.Errorf("expr=%q scope=%q: unoptimized=%s optimized=%s",
			exprSrc, scopeSrc, describe(want), describe(got))
	}
	// Optimize must not touch its input: the unoptimized program has to stay usable as the oracle,
	// including after the optimized one has run.
	if again := RunRegisters(rp, scope); !valuesEqual(want, again) {
		t.Errorf("expr=%q: the unoptimized program changed under Optimize (%s then %s)",
			exprSrc, describe(want), describe(again))
	}
}

// TestOptimizeAgreesCorpus runs the law over every expression corpus the package maintains.
func TestOptimizeAgreesCorpus(t *testing.T) {
	for _, scopeSrc := range scopeSources {
		for _, exprSrc := range exprSources {
			optAgrees(t, scopeSrc, exprSrc)
		}
	}
	for _, exprSrc := range parityCorpusExprs {
		optAgrees(t, `[Cpus = 4; Owner = "alice"; x = 5]`, exprSrc)
	}
	for _, exprSrc := range vecExprs {
		optAgrees(t, `[A = 10; B = 3; F = true; S = "user3"]`, exprSrc)
		optAgrees(t, `[A = undefined; B = 0]`, exprSrc)
	}
}

// TestOptimizeAgreesOnScripts runs the law over scripts, which are where the copies actually come
// from: every read of a local lowers to one.
func TestOptimizeAgreesOnScripts(t *testing.T) {
	for _, scopeSrc := range reductionScopes {
		scope, err := classad.Parse(scopeSrc)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range reductionOperands {
			for _, tmpl := range reductionTemplates {
				if capturesLocal(x) {
					continue
				}
				xe, err1 := parser.ParseExpr(x)
				re, err2 := parser.ParseExpr(tmpl)
				if err1 != nil || err2 != nil || xe == nil || re == nil {
					continue
				}
				sp, err := NewScript().Let(localA, xe).Let(localB, xe).Result(re).Compile()
				if err != nil {
					t.Fatalf("compile: %v", err)
				}
				want := RunRegisters(sp.RegProgram(), scope)
				got := RunRegisters(Optimize(sp.RegProgram(), AllPasses()), scope)
				if !valuesEqual(want, got) {
					t.Errorf("script{%s = %s; %s} unoptimized=%s optimized=%s",
						localA, x, tmpl, describe(want), describe(got))
				}
			}
		}
	}
}

// TestCopyPropActuallyFires is the guard against a vacuous law. A pass that never fires agrees with
// the unoptimized program on everything, so agreement alone proves nothing; this pins that the
// copies a script generates are in fact removed.
func TestCopyPropActuallyFires(t *testing.T) {
	rhs, err := parser.ParseExpr("Cpus + Memory")
	if err != nil {
		t.Fatal(err)
	}
	res, err := parser.ParseExpr("t + t + t")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := NewScript().Let("t", rhs).Result(res).Compile()
	if err != nil {
		t.Fatal(err)
	}
	before := countOp(sp.RegProgram(), RMove)
	if before == 0 {
		t.Fatal("the script compiled to no copies at all; this test is not testing the pass")
	}
	after := countOp(Optimize(sp.RegProgram(), AllPasses()), RMove)
	if after >= before {
		t.Errorf("copies: %d before, %d after; the pass did not fire", before, after)
	}
	// And the answer is still right.
	scope, err := classad.Parse(`[ Cpus = 4; Memory = 6 ]`)
	if err != nil {
		t.Fatal(err)
	}
	if got := RunRegisters(Optimize(sp.RegProgram(), AllPasses()), scope).String(); got != "30" {
		t.Errorf("optimized result = %s, want 30", got)
	}
}

// TestCopyPropRefusesAJoinDestination pins the condition most likely to be dropped as an
// over-caution. An Elvis join writes one destination from several paths, so it is NOT
// single-definition, and substituting it would take whichever path the pass looked at.
//
// This pins the OBSERVABLE behaviour -- the join's copy survives and both paths stay right -- and
// not which of copyProp's conditions enforces it. Two of them do, independently, for the shapes the
// compiler currently emits, so neither can be removed on its own and have any test notice. What
// notices the unsound combination is TestOptimizeAgreesOnScripts, through a local bound to an Elvis.
//
// The expression deliberately uses the join's value somewhere other than as the program's result: a
// bare `Missing ?: 7` has the join destination AS the result, so the result check refuses it and
// this test would pass whatever else were true.
func TestCopyPropRefusesAJoinDestination(t *testing.T) {
	expr, err := parser.ParseExpr(`(Missing ?: 7) + 1`)
	if err != nil {
		t.Fatal(err)
	}
	rp := LowerToRegisters(CompileProgram(expr))
	before := countOp(rp, RMove)
	if before == 0 {
		t.Fatal("no join move; this test is not testing what it says")
	}
	if after := countOp(Optimize(rp, AllPasses()), RMove); after != before {
		t.Errorf("the join's copy was removed (%d -> %d); its destination has more than one definition",
			before, after)
	}
	for _, tc := range []struct{ scope, want string }{
		{`[]`, "8"},            // fallback path
		{`[Missing = 3]`, "4"}, // taken path: the one a wrong substitution breaks
	} {
		scope, err := classad.Parse(tc.scope)
		if err != nil {
			t.Fatal(err)
		}
		if got := RunRegisters(Optimize(rp, AllPasses()), scope).String(); got != tc.want {
			t.Errorf("%s: (Missing ?: 7) + 1 = %s, want %s", tc.scope, got, tc.want)
		}
	}
}

// TestOptimizePassesAreSwitchable pins that the zero value runs nothing, which is what makes the
// law able to localize a divergence to one pass.
func TestOptimizePassesAreSwitchable(t *testing.T) {
	rhs, _ := parser.ParseExpr("Cpus + 1")
	res, _ := parser.ParseExpr("t + t")
	sp, err := NewScript().Let("t", rhs).Result(res).Compile()
	if err != nil {
		t.Fatal(err)
	}
	none := Optimize(sp.RegProgram(), OptPasses{})
	if countOp(none, RMove) != countOp(sp.RegProgram(), RMove) {
		t.Error("OptPasses{} changed the program; the zero value must run no pass")
	}
	if countOp(Optimize(sp.RegProgram(), OptPasses{CopyProp: true}), RMove) >= countOp(none, RMove) {
		t.Error("enabling CopyProp alone did nothing")
	}
}

// FuzzOptimizeAgrees is the law's fuzzer.
func FuzzOptimizeAgrees(f *testing.F) {
	for _, ss := range scopeSources {
		for _, es := range exprSources {
			f.Add(ss, es)
		}
	}
	for _, es := range parityCorpusExprs {
		f.Add(`[Cpus = 4; Owner = "alice"; x = 5]`, es)
	}
	f.Fuzz(func(t *testing.T, scopeSrc, exprSrc string) {
		optAgrees(t, scopeSrc, exprSrc)
	})
}

// TestOptimizeCarriesEveryField guards the way this broke once. Optimize used to copy RegProgram
// field by field, so when the conditional branch added a target table the optimized program got a
// nil one and indexed an empty slice at run time. A struct copy carries a new field by default;
// this asserts the one that was dropped, and the behaviour that depended on it.
func TestOptimizeCarriesEveryField(t *testing.T) {
	expr, err := parser.ParseExpr(`ifThenElse(Cpus > 2, 1, 2)`)
	if err != nil {
		t.Fatal(err)
	}
	rp := LowerToRegisters(CompileProgram(expr))
	if len(rp.conds) == 0 {
		t.Fatal("the conditional compiled with no target table; this test is not testing what it says")
	}
	opt := Optimize(rp, AllPasses())
	if len(opt.conds) != len(rp.conds) {
		t.Errorf("optimized program has %d condition targets, source has %d", len(opt.conds), len(rp.conds))
	}
	for _, tc := range []struct{ scope, want string }{
		{`[Cpus = 4]`, "1"},
		{`[Cpus = 1]`, "2"},
		{`[]`, "undefined"},
	} {
		scope, err := classad.Parse(tc.scope)
		if err != nil {
			t.Fatal(err)
		}
		if got := RunRegisters(opt, scope).String(); got != tc.want {
			t.Errorf("%s: optimized ifThenElse = %s, want %s", tc.scope, got, tc.want)
		}
	}
}

// TestOptimizerKnowsEveryOpcode is the tripwire for the other half of the same hazard: a pass that
// treats a new instruction as reading no registers will happily delete what it reads. regUses lists
// every opcode and panics on anything else, so adding one without teaching the optimizer fails here
// rather than silently.
func TestOptimizerKnowsEveryOpcode(t *testing.T) {
	rp := &RegProgram{src: &Program{}, res: -1, nregs: 1}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("regUses accepted an unknown opcode; a new one could be analyzed as reading nothing")
		}
	}()
	regUses(rp, RegInstr{Op: RegOp(250)}, nil)
}

// TestDeadCodeKeepsWhatCanPanic is the condition that makes this pass correct rather than plausible,
// and it is not a general property of dead-code elimination -- it is specific to this evaluator.
//
// Resolving an attribute reference can be a CYCLIC reference, which panics, and RunRegisters turns
// that panic into an error value. So a load nothing reads still decides the result: binding a local
// to a cyclic attribute and never using it yields ERROR, not the result expression's value.
// Removing the load would turn ERROR into 5.
func TestDeadCodeKeepsWhatCanPanic(t *testing.T) {
	scope, err := classad.Parse(`[ Cyc = Cyc + 1; Ok = 5 ]`)
	if err != nil {
		t.Fatal(err)
	}
	rhs, err := parser.ParseExpr("Cyc")
	if err != nil {
		t.Fatal(err)
	}
	res, err := parser.ParseExpr("5")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := NewScript().Let("unused", rhs).Result(res).Compile()
	if err != nil {
		t.Fatal(err)
	}
	before := RunRegisters(sp.RegProgram(), scope)
	if before.String() != "error" {
		t.Fatalf("unoptimized = %s, want error; this test is not set up to test what it says", before)
	}
	after := RunRegisters(Optimize(sp.RegProgram(), AllPasses()), scope)
	if !valuesEqual(before, after) {
		t.Errorf("optimized = %s, unoptimized = %s; a dead attribute load was removed, and resolving it is observable",
			describe(after), describe(before))
	}
	// A DELEGATED SUBTREE is the same hazard by a different route: this one compiles to a single
	// REvalNode, which hands the expression to the tree-walker, which hits the cycle.
	cond, err := parser.ParseExpr("Cyc ? 1 : 2")
	if err != nil {
		t.Fatal(err)
	}
	spc, err := NewScript().Let("unused", cond).Result(res).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if countOp(spc.RegProgram(), REvalNode) == 0 {
		t.Fatal("the conditional did not compile to REvalNode; this case is not testing what it says")
	}
	wantC := RunRegisters(spc.RegProgram(), scope)
	if wantC.String() != "error" {
		t.Fatalf("unoptimized conditional = %s, want error", wantC)
	}
	if gotC := RunRegisters(Optimize(spc.RegProgram(), AllPasses()), scope); !valuesEqual(wantC, gotC) {
		t.Errorf("optimized = %s, unoptimized = %s; a dead delegated subtree was removed",
			describe(gotC), describe(wantC))
	}

	// A dead LITERAL, by contrast, is removable: nothing about evaluating it can be observed.
	lit, err := parser.ParseExpr("42")
	if err != nil {
		t.Fatal(err)
	}
	sp2, err := NewScript().Let("alsoUnused", lit).Result(res).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if countOp(Optimize(sp2.RegProgram(), AllPasses()), RConst) >= countOp(sp2.RegProgram(), RConst) {
		t.Error("a dead literal was not removed; the pass is doing nothing here")
	}
}

// TestDeadCodeActuallyFires guards against the vacuous law: a pass that never fires agrees with the
// unoptimized program on everything.
func TestDeadCodeActuallyFires(t *testing.T) {
	rhs, _ := parser.ParseExpr("1 + 2")
	res, _ := parser.ParseExpr("t + t")
	sp, err := NewScript().Let("t", rhs).Result(res).Compile()
	if err != nil {
		t.Fatal(err)
	}
	live := func(rp *RegProgram) int {
		n := 0
		for _, in := range rp.code {
			if in.Op != RNop {
				n++
			}
		}
		return n
	}
	before := live(sp.RegProgram())
	after := live(Optimize(sp.RegProgram(), AllPasses()))
	if after >= before {
		t.Errorf("live instructions: %d before, %d after; the passes did not fire", before, after)
	}
	scope, _ := classad.Parse(`[]`)
	if got := RunRegisters(Optimize(sp.RegProgram(), AllPasses()), scope).String(); got != "6" {
		t.Errorf("optimized result = %s, want 6", got)
	}
}

// TestDeadCodeIsSwitchable pins that DeadCode runs only when selected, which is what lets the law
// localize a divergence to one pass.
func TestDeadCodeIsSwitchable(t *testing.T) {
	lit, _ := parser.ParseExpr("42")
	res, _ := parser.ParseExpr("1")
	sp, err := NewScript().Let("unused", lit).Result(res).Compile()
	if err != nil {
		t.Fatal(err)
	}
	base := countOp(sp.RegProgram(), RConst)
	if n := countOp(Optimize(sp.RegProgram(), OptPasses{CopyProp: true}), RConst); n != base {
		t.Errorf("CopyProp alone changed the literal count (%d -> %d)", base, n)
	}
	if n := countOp(Optimize(sp.RegProgram(), OptPasses{DeadCode: true}), RConst); n >= base {
		t.Errorf("DeadCode alone did nothing (%d -> %d)", base, n)
	}
}

// TestOptimizerKnowsTheOpcodesWeEmit is the complement of TestOptimizerKnowsEveryOpcode, and the one
// that actually catches a missed opcode.
//
// That test proves the tripwire fires for an opcode nobody emits. This one walks every expression
// corpus the package maintains, lowers each, and asks regUses about every instruction -- so an
// opcode the compiler DOES emit and the optimizer has not been taught fails here.
//
// Without it the gap is latent rather than loud: regUses is reached from copy propagation only when
// there is a copy to consider, so a program without one can carry an unknown opcode straight through
// a pass that would have deleted what it reads. A merged stringListMember specialization did exactly
// that, and every test still passed.
func TestOptimizerKnowsTheOpcodesWeEmit(t *testing.T) {
	var buf []int32
	check := func(src string) {
		t.Helper()
		expr, err := parser.ParseExpr(src)
		if err != nil || expr == nil {
			return
		}
		rp := LowerToRegisters(CompileProgram(expr))
		for i, in := range rp.code {
			// A panic here is the failure; the message names the opcode.
			buf = regUses(rp, in, buf)
			if _, ok := regDef(in); !ok && in.Op != RNop {
				t.Errorf("%s instruction %d (op %d) defines nothing and is not a nop", src, i, in.Op)
			}
		}
	}
	for _, src := range exprSources {
		check(src)
	}
	for _, src := range vecExprs {
		check(src)
	}
	for _, src := range parityCorpusExprs {
		check(src)
	}
}

// TestOptimizeAgreesOnBranchingScripts extends the law to scripts with control flow, which the
// passes only started seeing in statement position when If arrived.
//
// It matters because both passes reason about reachability. Copy propagation refuses to substitute
// across a point control can arrive at, and dead code elimination must not delete a store that only
// one path makes. A straight-line corpus exercises neither.
func TestOptimizeAgreesOnBranchingScripts(t *testing.T) {
	parse := func(src string) ast.Expr {
		e, err := parser.ParseExpr(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		return e
	}
	for _, cond := range []string{"Cpus > 2", "Cpus > 100", "Missing", "Flag", "Cpus"} {
		for _, shape := range []struct {
			name      string
			then, els string
			result    string
		}{
			{"both branches bind", "1", "2", "x"},
			{"only then binds", "1", "", "x"},
			{"only else binds", "", "2", "x"},
			{"branch reads the local", "x + 1", "x - 1", "x"},
			{"result mixes local and attribute", "1", "2", "x + Cpus"},
			{"local read twice after merge", "1", "2", "x + x"},
		} {
			build := func() *ScriptProgram {
				s := NewScript().Let("x", parse("0"))
				s = s.If(parse(cond),
					func(b *Script) {
						if shape.then != "" {
							b.Let("x", parse(shape.then))
						}
					},
					func(b *Script) {
						if shape.els != "" {
							b.Let("x", parse(shape.els))
						}
					})
				sp, err := s.Result(parse(shape.result)).Compile()
				if err != nil {
					t.Fatalf("compile: %v", err)
				}
				return sp
			}
			for _, scopeSrc := range []string{`[Cpus = 4; Flag = true]`, `[Cpus = 1; Flag = false]`, `[]`} {
				scope, err := classad.Parse(scopeSrc)
				if err != nil {
					t.Fatal(err)
				}
				sp := build()
				want := RunRegisters(sp.RegProgram(), scope)
				got := RunRegisters(Optimize(sp.RegProgram(), AllPasses()), scope)
				if !valuesEqual(want, got) {
					t.Errorf("%s / cond=%q / scope=%s: unoptimized=%s optimized=%s",
						shape.name, cond, scopeSrc, describe(want), describe(got))
				}
			}
		}
	}
}
