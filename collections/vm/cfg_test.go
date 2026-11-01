package vm

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// allInit is the trivially correct initializer set: every local initialized, whatever the analysis
// says. It is the oracle for localsNeedingInit, because initializing a local that did not need it
// changes no answer -- the store that follows overwrites it -- while failing to initialize one that
// did changes the answer from UNDEFINED to whatever the register happened to hold.
func allInit(n int) []bool {
	b := make([]bool, n)
	for i := range b {
		b[i] = true
	}
	return b
}

// agreesWithAllInitialized checks one script both ways.
func agreesWithAllInitialized(t *testing.T, sp *ScriptProgram, scope *classad.ClassAd) {
	t.Helper()
	p := sp.Program()
	want := RunRegisters(lowerWithInit(p, allInit(len(p.locals))), scope)
	got := RunRegisters(sp.RegProgram(), scope)
	if !valuesEqual(want, got) {
		t.Errorf("analysis disagrees with all-initialized: all-init=%s analyzed=%s",
			describe(want), describe(got))
	}
}

// TestLocalsNeedingInitAgreesWithInitializingEverything is the safety net for the dominance
// analysis, over every script shape the package can build.
func TestLocalsNeedingInitAgreesWithInitializingEverything(t *testing.T) {
	for _, scopeSrc := range reductionScopes {
		scope, err := classad.Parse(scopeSrc)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range reductionOperands {
			if capturesLocal(x) {
				continue
			}
			xe, err := parser.ParseExpr(x)
			if err != nil || xe == nil {
				continue
			}
			for _, tmpl := range reductionTemplates {
				re, err := parser.ParseExpr(tmpl)
				if err != nil || re == nil {
					continue
				}
				// Straight line: two bindings and a result.
				sp, err := NewScript().Let(localA, xe).Let(localB, xe).Result(re).Compile()
				if err != nil {
					t.Fatalf("compile: %v", err)
				}
				agreesWithAllInitialized(t, sp, scope)

				// A binding inside a branch, read after the merge: the shape whose initializer the
				// analysis must KEEP.
				cond, _ := parser.ParseExpr("Cpus > 2")
				sp2, err := NewScript().
					If(cond, func(s *Script) { s.Let(localA, xe) }, func(s *Script) { s.Let(localB, xe) }).
					Result(re).Compile()
				if err != nil {
					t.Fatalf("compile branch: %v", err)
				}
				agreesWithAllInitialized(t, sp2, scope)

				// A binding before a branch, read after it: the shape whose initializer the analysis
				// now REMOVES.
				sp3, err := NewScript().
					Let(localA, xe).Let(localB, xe).
					If(cond, func(s *Script) {}, nil).
					Result(re).Compile()
				if err != nil {
					t.Fatalf("compile post-branch: %v", err)
				}
				agreesWithAllInitialized(t, sp3, scope)
			}
		}
	}
}

// countRegOp counts instructions of one register opcode.
func countRegOp(rp *RegProgram, op RegOp) int {
	n := 0
	for _, in := range rp.code {
		if in.Op == op {
			n++
		}
	}
	return n
}

// TestDominatingStoreNeedsNoInitializer is the win, stated as the structural fact rather than as a
// result that would be identical either way.
//
// `x = Cpus + 1` then a branch then a read of x: the store is on every path to the read, so the read
// cannot see an uninitialized register. The old approximation inserted an initializer anyway, because
// a jump target (the branch's merge) sat between the store and the read.
func TestDominatingStoreNeedsNoInitializer(t *testing.T) {
	rhs, err := parser.ParseExpr("Cpus + 1")
	if err != nil {
		t.Fatal(err)
	}
	cond, err := parser.ParseExpr("Cpus > 2")
	if err != nil {
		t.Fatal(err)
	}
	res, err := parser.ParseExpr("x")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := NewScript().Let("x", rhs).If(cond, func(s *Script) {}, nil).Result(res).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if n := countRegOp(sp.RegProgram(), RUndef); n != 0 {
		t.Errorf("%d initializers, want 0: the store dominates the read", n)
	}
	// And the consequence that made it worth fixing: with one definition instead of two, the read's
	// move is now propagated away.
	rp := sp.RegProgram()
	before := countRegOp(rp, RMove)
	if before == 0 {
		t.Fatal("the read compiled to no move; this test is not testing what it says")
	}
	if after := countRegOp(Optimize(rp, AllPasses()), RMove); after >= before {
		t.Errorf("moves %d -> %d: copy propagation still refuses the local's register", before, after)
	}
	for _, tc := range []struct{ scope, want string }{
		{`[Cpus = 4]`, "5"},
		{`[Cpus = 1]`, "2"},
	} {
		scope, err := classad.Parse(tc.scope)
		if err != nil {
			t.Fatal(err)
		}
		if got := RunRegisters(Optimize(rp, AllPasses()), scope).String(); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.scope, got, tc.want)
		}
	}
}

// TestBranchBoundLocalKeepsItsInitializer is the other half, and the one a too-clever analysis
// breaks. A local bound ONLY inside a branch and read after the merge must read UNDEFINED on the
// path that did not bind it -- which is a semantic test, not a count.
func TestBranchBoundLocalKeepsItsInitializer(t *testing.T) {
	one, err := parser.ParseExpr("1")
	if err != nil {
		t.Fatal(err)
	}
	cond, err := parser.ParseExpr("Cpus > 2")
	if err != nil {
		t.Fatal(err)
	}
	res, err := parser.ParseExpr("x")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := NewScript().If(cond, func(s *Script) { s.Let("x", one) }, nil).Result(res).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if n := countRegOp(sp.RegProgram(), RUndef); n == 0 {
		t.Error("no initializer: a local bound only in the taken branch would read a register nothing wrote")
	}
	for _, tc := range []struct{ scope, want string }{
		{`[Cpus = 4]`, "1"},         // branch taken, the store ran
		{`[Cpus = 1]`, "undefined"}, // branch not taken, nothing stored
	} {
		scope, err := classad.Parse(tc.scope)
		if err != nil {
			t.Fatal(err)
		}
		if got := sp.Run(scope).String(); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.scope, got, tc.want)
		}
	}
}

// TestLoopCarriedReadKeepsItsInitializer covers the back edge, where a store inside the body does
// not dominate a read earlier in the same body: the first iteration reaches the read first.
func TestLoopCarriedReadKeepsItsInitializer(t *testing.T) {
	cond, err := parser.ParseExpr("Cpus > 0")
	if err != nil {
		t.Fatal(err)
	}
	// while Cpus > 0:  n = n + 1      -- reads n before any store on the first trip
	inc, err := parser.ParseExpr("n + 1")
	if err != nil {
		t.Fatal(err)
	}
	res, err := parser.ParseExpr("n")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := NewScript().While(cond, func(s *Script) { s.Let("n", inc) }).Result(res).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if n := countRegOp(sp.RegProgram(), RUndef); n == 0 {
		t.Error("no initializer: the store inside the body does not dominate the read inside it")
	}
}

// --- dominance itself, on shapes built by hand ---

// TestDominanceOnADiamond pins the property every caller relies on: the instruction before a branch
// dominates everything after the merge, and neither arm dominates the other.
func TestDominanceOnADiamond(t *testing.T) {
	// 0: entry
	// 1: branch -> 2 (true) / 4 (false)
	// 2: then
	// 3: jump -> 5
	// 4: else
	// 5: merge
	succs := func(ip int, buf []int) []int {
		buf = buf[:0]
		switch ip {
		case 1:
			return append(buf, 2, 4)
		case 3:
			return append(buf, 5)
		}
		return append(buf, ip+1)
	}
	c := buildCFG(6, succs)
	for _, tc := range []struct {
		a, b int
		want bool
		why  string
	}{
		{0, 5, true, "the entry dominates everything reachable"},
		{1, 5, true, "the branch dominates the merge"},
		{2, 5, false, "the then arm does not dominate the merge"},
		{4, 5, false, "the else arm does not dominate the merge"},
		{2, 4, false, "neither arm dominates the other"},
		{5, 5, true, "a node dominates itself"},
		{5, 1, false, "dominance is not symmetric"},
	} {
		if got := c.dominates(tc.a, tc.b); got != tc.want {
			t.Errorf("dominates(%d, %d) = %v, want %v -- %s", tc.a, tc.b, got, tc.want, tc.why)
		}
	}
}

// TestDominanceIgnoresUnreachableCode pins the decision in markReachable. An unreachable node must
// not be reported as dominating anything, because the conventional "dominated by everything" answer
// would make a store in dead code look like it reaches a live read.
func TestDominanceIgnoresUnreachableCode(t *testing.T) {
	// 0 -> 2 (1 is unreachable, and would otherwise look like a dominator of 2 by falling through)
	succs := func(ip int, buf []int) []int {
		buf = buf[:0]
		if ip == 0 {
			return append(buf, 2)
		}
		return append(buf, ip+1)
	}
	c := buildCFG(3, succs)
	if c.reach[1] {
		t.Fatal("node 1 is reachable; this test is not testing what it says")
	}
	if c.dominates(1, 2) {
		t.Error("an unreachable store was reported as dominating a reachable read")
	}
	if !c.dominates(0, 2) {
		t.Error("the entry does not dominate node 2")
	}
}

// TestLoopHeaderDominatesItsBody pins the back edge, which the fixpoint has to converge on rather
// than walk.
func TestLoopHeaderDominatesItsBody(t *testing.T) {
	// 0: entry  1: header -> 2 (body) / 4 (exit)  2: body  3: back edge -> 1  4: after
	succs := func(ip int, buf []int) []int {
		buf = buf[:0]
		switch ip {
		case 1:
			return append(buf, 2, 4)
		case 3:
			return append(buf, 1)
		}
		return append(buf, ip+1)
	}
	c := buildCFG(5, succs)
	if !c.dominates(1, 2) {
		t.Error("the loop header does not dominate its body")
	}
	if c.dominates(2, 1) {
		t.Error("the body dominates the header; the back edge was treated as the only entry")
	}
	if !c.dominates(1, 4) {
		t.Error("the header does not dominate the code after the loop")
	}
}

// TestElseArmReadsALocalBoundBeforeTheBranch is the shape that proves the CFG has the conditional's
// FALSE edge. Dropping that edge makes the else arm look unreachable, and then every analysis here
// quietly ignores it: the read inside it is skipped, so the local's copy is deleted without the read
// being rewritten, and the else path returns whatever the dead register held.
//
//	x = Cpus + 1
//	if Cpus > 2: y = 10
//	else:        y = x        <- the read the false edge is needed to see
//	result = y
func TestElseArmReadsALocalBoundBeforeTheBranch(t *testing.T) {
	rhs, err := parser.ParseExpr("Cpus + 1")
	if err != nil {
		t.Fatal(err)
	}
	cond, err := parser.ParseExpr("Cpus > 2")
	if err != nil {
		t.Fatal(err)
	}
	ten, err := parser.ParseExpr("10")
	if err != nil {
		t.Fatal(err)
	}
	readX, err := parser.ParseExpr("x")
	if err != nil {
		t.Fatal(err)
	}
	readY, err := parser.ParseExpr("y")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := NewScript().
		Let("x", rhs).
		If(cond,
			func(s *Script) { s.Let("y", ten) },
			func(s *Script) { s.Let("y", readX) }).
		Result(readY).Compile()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ scope, want string }{
		{`[Cpus = 4]`, "10"}, // true path
		{`[Cpus = 1]`, "2"},  // false path: reads x, which was bound before the branch
	} {
		scope, err := classad.Parse(tc.scope)
		if err != nil {
			t.Fatal(err)
		}
		if got := sp.Run(scope).String(); got != tc.want {
			t.Errorf("%s: unoptimized = %s, want %s", tc.scope, got, tc.want)
		}
		if got := RunRegisters(Optimize(sp.RegProgram(), AllPasses()), scope).String(); got != tc.want {
			t.Errorf("%s: optimized = %s, want %s", tc.scope, got, tc.want)
		}
	}
}

// TestCopyPropRefusesANonDominatingCopy exercises copyProp's dominance condition directly, with a
// program built by hand.
//
// It is built by hand because THE COMPILER CANNOT PRODUCE THIS SHAPE, and that is worth stating
// rather than leaving as a gap. If a register has exactly one definition and some read of it is not
// dominated by that definition, then a path reaches the read with the register unwritten -- and the
// lowering never emits that: localsNeedingInit puts an initializer on exactly those locals, which
// gives the register a second definition and makes the earlier condition refuse first. So the
// dominance check is unreachable through the compiler today and guards a future lowering, which is
// the same standing as the join-destination condition it replaced. Without this test, deleting it
// would break nothing.
//
//	0: RConst  r1 = 1
//	1: RBranchCond on r1           (false/undef/error -> 4)
//	2: RMove    r2 = r1            <- the copy, inside the taken arm only
//	3: RJump                -> 4
//	4: RUnop    r3 = -r2           <- reads r2, NOT dominated by the copy
func TestCopyPropRefusesANonDominatingCopy(t *testing.T) {
	p := &Program{
		consts: []classad.Value{classad.NewIntValue(1)},
		ops:    []string{"-"},
		conds:  []condTargets{{onFalse: 4, onUndef: 4, onError: 4, end: 4}},
	}
	rp := &RegProgram{
		src:   p,
		nregs: 4,
		res:   3,
		conds: []condTargets{{onFalse: 4, onUndef: 4, onError: 4, end: 4}},
		code: []RegInstr{
			{Op: RConst, Dst: 1, Imm: 0},
			{Op: RBranchCond, A: 1, Imm: 0},
			{Op: RMove, Dst: 2, A: 1},
			{Op: RJump, T: 4},
			{Op: RUnop, Dst: 3, A: 2, Imm: 0},
		},
	}
	c := regCFG(rp)
	if c.dominates(2, 4) {
		t.Fatal("the copy dominates the read; this test is not testing what it says")
	}
	defs := defCounts(rp)
	if defs[2] != 1 || defs[1] != 1 {
		t.Fatalf("defs = %v, want exactly one definition of r1 and r2; another condition would refuse first", defs)
	}
	out := Optimize(rp, OptPasses{CopyProp: true})
	if countRegOp(out, RMove) != 1 {
		t.Error("the copy was removed although it does not dominate the read of its destination")
	}
	if out.code[4].A != 2 {
		t.Errorf("the read was rewritten to r%d; control can reach it without passing the copy", out.code[4].A)
	}
}

// A MUTATION THIS FILE DELIBERATELY DOES NOT CATCH, recorded rather than chased: making regSuccs
// treat RFault as falling through to the next instruction. An EXTRA edge can only add paths, and
// dominance is monotone decreasing in edges while reachability only grows -- so a spurious edge makes
// every caller here refuse more work, never accept unsound work. A missing edge is the dangerous
// direction, and that is what TestElseArmReadsALocalBoundBeforeTheBranch and the stack-form half of
// the same shape cover.
