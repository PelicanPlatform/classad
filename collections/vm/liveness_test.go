package vm

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// TestLivenessOnAHandBuiltProgram checks liveness against answers worked out by hand, which is the
// only way to know the fixpoint is computing what it claims rather than something self-consistent.
//
//	0: RConst r1 = 1          r1 live from 1 onwards
//	1: RConst r2 = 2          r2 live from 2 onwards
//	2: RBinop r3 = r1 + r2    kills nothing, defines r3; r1 and r2 die here
//	3: RUnop  r4 = -r3        r3 dies here
//	res = r4
func TestLivenessOnAHandBuiltProgram(t *testing.T) {
	p := &Program{
		consts: []classad.Value{classad.NewIntValue(1), classad.NewIntValue(2)},
		ops:    []string{"+", "-"},
	}
	rp := &RegProgram{
		src: p, nregs: 5, res: 4,
		code: []RegInstr{
			{Op: RConst, Dst: 1, Imm: 0},
			{Op: RConst, Dst: 2, Imm: 1},
			{Op: RBinop, Dst: 3, A: 1, B: 2, Imm: 0},
			{Op: RUnop, Dst: 4, A: 3, Imm: 1},
		},
	}
	l := liveness(rp)
	for _, tc := range []struct {
		at   int
		r    int32
		want bool
		why  string
	}{
		{0, 1, false, "r1 is not live before it is defined"},
		{1, 1, true, "r1 is live after its definition, since instruction 2 reads it"},
		{2, 1, true, "r1 is live on entry to the instruction that reads it"},
		{3, 1, false, "r1 died at its last read"},
		{2, 2, true, "r2 is live on entry to its reader"},
		{3, 3, true, "r3 is live on entry to its reader"},
		{4, 3, false, "r3 died at its last read"},
		{4, 4, true, "the result register is live at the exit"},
	} {
		if got := l.liveAt(tc.at, tc.r); got != tc.want {
			t.Errorf("liveAt(%d, r%d) = %v, want %v -- %s", tc.at, tc.r, got, tc.want, tc.why)
		}
	}
	// Two constants plus nothing else is the peak: at instruction 2 both operands are live.
	if got := MaxLive(rp); got != 2 {
		t.Errorf("MaxLive = %d, want 2 (the two operands of the one binary op)", got)
	}
}

// TestLivenessAcrossABranch pins the union over successors.
//
// The register whose liveness is asserted is NOT the one the branch tests, deliberately. An earlier
// version of this test used the condition register, and a liveness that looked at only one successor
// still passed it -- because RBranchCond reads its condition itself, so that register is live at the
// branch no matter what the successors say. Here r1 is live at the branch for one reason only: the
// true arm reads it.
//
//	0: RConst r1 = 1          the value, read in the true arm only
//	1: RConst r2 = 1          the condition
//	2: RBranchCond on r2      -> 3 (true) / 5
//	3: RUnop  r3 = -r1        the only read of r1
//	4: RJump                  -> 6
//	5: RConst r3 = 1
//	6: RMove  res = r3
func TestLivenessAcrossABranch(t *testing.T) {
	p := &Program{
		consts: []classad.Value{classad.NewIntValue(1)},
		ops:    []string{"-"},
		conds:  []condTargets{{onFalse: 5, onUndef: 5, onError: 5, end: 6}},
	}
	rp := &RegProgram{
		src: p, nregs: 7, res: 6,
		conds: []condTargets{{onFalse: 5, onUndef: 5, onError: 5, end: 6}},
		code: []RegInstr{
			{Op: RConst, Dst: 1, Imm: 0},
			{Op: RConst, Dst: 2, Imm: 0},
			{Op: RBranchCond, A: 2, Imm: 0},
			{Op: RUnop, Dst: 3, A: 1, Imm: 0},
			{Op: RJump, T: 6},
			{Op: RConst, Dst: 3, Imm: 0},
			{Op: RMove, Dst: 6, A: 3},
		},
	}
	l := liveness(rp)
	if !l.liveAt(2, 1) {
		t.Error("r1 is not live at the branch, although the true arm reads it; the union missed a successor")
	}
	if !l.liveAt(3, 1) {
		t.Error("r1 is not live in the arm that reads it")
	}
	if l.liveAt(5, 1) {
		t.Error("r1 is live in the arm that does not read it")
	}
	if !l.liveAt(6, 3) {
		t.Error("r3 is not live at the merge, although both arms define it and the merge reads it")
	}
}

// TestLivenessAcrossALoopBackEdge pins why the computation is a fixpoint and not a sweep: a value
// read at the top of a loop body and redefined at the bottom is live across the back edge, which a
// single backward pass over instruction order cannot see.
func TestLivenessAcrossALoopBackEdge(t *testing.T) {
	cond, err := parser.ParseExpr("n < Cpus")
	if err != nil {
		t.Fatal(err)
	}
	zero, err := parser.ParseExpr("0")
	if err != nil {
		t.Fatal(err)
	}
	inc, err := parser.ParseExpr("n + 1")
	if err != nil {
		t.Fatal(err)
	}
	res, err := parser.ParseExpr("n")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := NewScript().Let("n", zero).
		While(cond, func(s *Script) { s.Let("n", inc) }).
		Result(res).Compile()
	if err != nil {
		t.Fatal(err)
	}
	rp := sp.RegProgram()
	if len(rp.localRegs) != 1 {
		t.Fatalf("expected one local, got %d", len(rp.localRegs))
	}
	n := rp.localRegs[0]
	l := liveness(rp)
	// THE BACK EDGE is what makes this a fixpoint rather than a backward sweep. The jump at the
	// bottom of the body targets the loop header, which sits at a LOWER instruction index -- so a
	// single high-to-low pass computes the jump's live-out from a header whose live-in it has not
	// computed yet, and under-approximates. The loop's local is live across that edge, because the
	// header's condition reads it on the next trip.
	backEdge := -1
	for i, in := range rp.code {
		if in.Op == RJump && int(in.T) <= i {
			backEdge = i
			break
		}
	}
	if backEdge < 0 {
		t.Fatal("no back edge in the compiled loop; this test is not testing what it says")
	}
	if !l.liveAt(backEdge, n) {
		t.Errorf("the loop's local is not live at the back edge (instruction %d); a single backward pass cannot see this",
			backEdge)
	}
	// And it is live at the exit, since the result reads it.
	if !l.liveAt(l.n-1, rp.res) {
		t.Error("the result register is not live at the exit")
	}
}

// TestLivenessAgreesWithDeadCode is the cross-check between two independent implementations. deadCode
// computes its own program-wide liveness to decide what to remove; a register that this liveness says
// is live nowhere must be one deadCode found dead too, on every corpus program.
//
// They are written differently -- deadCode iterates over whole registers, this iterates per
// instruction over the CFG -- so agreement is evidence rather than a tautology.
func TestLivenessAgreesWithDeadCode(t *testing.T) {
	check := func(src string) {
		t.Helper()
		expr, err := parser.ParseExpr(src)
		if err != nil || expr == nil {
			return
		}
		rp := LowerToRegisters(CompileProgram(expr))
		l := liveness(rp)
		liveAnywhere := make([]bool, rp.nregs)
		for i := 0; i < l.n; i++ {
			for r := int32(0); r < int32(rp.nregs); r++ {
				if l.liveAt(i, r) {
					liveAnywhere[r] = true
				}
			}
		}
		opt := Optimize(rp, OptPasses{DeadCode: true})
		for i, in := range rp.code {
			d, ok := regDef(in)
			if !ok || !removable(in) {
				continue
			}
			// A removable instruction defining a register that is live nowhere must have been
			// removed; one whose destination is live somewhere must have been kept.
			removed := opt.code[i].Op == RNop && in.Op != RNop
			if !liveAnywhere[d] && !removed {
				t.Errorf("%s: instruction %d defines r%d, which is live nowhere, yet survived DCE",
					src, i, d)
			}
			if liveAnywhere[d] && removed {
				t.Errorf("%s: instruction %d defines r%d, which is live somewhere, yet DCE removed it",
					src, i, d)
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

// TestPlacesDoNotInflatePeakLive is the measurement that refuted an argument, kept as a test because
// the argument will be made again.
//
// THE ARGUMENT WAS: the lowering gives each script local one register for the whole program (a
// "place" rather than a value), so a local's live range must be the whole program -- and since a
// register is about 200 KiB in the vector form at 8192 records, places must maximize peak live bytes,
// which is the quantity a vector register allocator exists to minimize. That would make converting
// to SSA urgent.
//
// IT IS WRONG, and liveness is what shows it. Liveness is a property of DEFINITION AND USE POSITIONS,
// not of names: `liveIn = uses u (liveOut \ def)` kills a register at its next definition and after
// its last read whether or not the same name is reused later. So eight locals bound in a chain, with
// only the last one read, hold two registers live at a time and not eight.
//
// The generalization, which the numbers below only check: at a join, SSA is weakly WORSE for this
// quantity, because a phi needs both of its operands live simultaneously while a place has already
// merged them into one register. SSA's real benefits here are elsewhere -- naming for CSE, and the
// interference structure a register allocator wants -- and none of them is peak live bytes.
func TestPlacesDoNotInflatePeakLive(t *testing.T) {
	// Eight locals in a chain, only the last read. Places give one register each -- eight registers
	// exist -- but at most two are live at once.
	chained := NewScript()
	for i, nm := range []string{"l1", "l2", "l3", "l4", "l5", "l6", "l7", "l8"} {
		src := "Cpus + 1"
		if i > 0 {
			src = "l" + string(rune('0'+i)) + " + 1"
		}
		ex, err := parser.ParseExpr(src)
		if err != nil {
			t.Fatal(err)
		}
		chained = chained.Let(nm, ex)
	}
	last, err := parser.ParseExpr("l8")
	if err != nil {
		t.Fatal(err)
	}
	spChain, err := chained.Result(last).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(spChain.Locals()); n != 8 {
		t.Fatalf("%d locals, want 8; this test is not testing what it says", n)
	}
	if got := MaxLive(spChain.RegProgram()); got != 2 {
		t.Errorf("chained locals: MaxLive = %d, want 2 -- a place's live range is its uses, not the program", got)
	}

	// Eight locals all read at the end. Nine live at the peak, and that is INHERENT: the sum needs
	// every operand at once, so SSA would need the same nine. It is here to show the previous number
	// is not an artifact of the measurement.
	summed := NewScript()
	for i, nm := range []string{"l1", "l2", "l3", "l4", "l5", "l6", "l7", "l8"} {
		ex, err := parser.ParseExpr("Cpus + " + string(rune('1'+i)))
		if err != nil {
			t.Fatal(err)
		}
		summed = summed.Let(nm, ex)
	}
	sum, err := parser.ParseExpr("l1 + l2 + l3 + l4 + l5 + l6 + l7 + l8")
	if err != nil {
		t.Fatal(err)
	}
	spSum, err := summed.Result(sum).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if got := MaxLive(spSum.RegProgram()); got != 9 {
		t.Errorf("summed locals: MaxLive = %d, want 9 -- every operand is genuinely needed at once", got)
	}
}
