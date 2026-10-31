package vm

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// regParity checks one expression in one scope: the register form must produce exactly what the
// stack form produces for the SAME Program. Same pools, same scope, same value.
func regParity(t *testing.T, scopeSrc, exprSrc string) {
	t.Helper()
	scope, err := classad.Parse(scopeSrc)
	if err != nil || scope == nil {
		return
	}
	expr, err := parser.ParseExpr(exprSrc)
	if err != nil || expr == nil {
		return
	}
	p := CompileProgram(expr)
	want := Run(p, scope)
	got := RunRegisters(LowerToRegisters(p), scope)
	if !valuesEqual(want, got) {
		t.Errorf("expr=%q scope=%q: stack=%s register=%s", exprSrc, scopeSrc, describe(want), describe(got))
	}
}

// TestRegisterParityCorpus runs every expression corpus the package already maintains through both
// forms. The corpora are shared with the stack interpreter's own differential test and the vector
// executor's, so this inherits their coverage: short-circuit with a cyclic right operand, Elvis
// chains, delegated subtrees, strict and non-strict built-in calls, member over a constant list, and
// the C++ parity edge cases.
func TestRegisterParityCorpus(t *testing.T) {
	for _, scopeSrc := range scopeSources {
		for _, exprSrc := range exprSources {
			regParity(t, scopeSrc, exprSrc)
		}
	}
	for _, exprSrc := range parityCorpusExprs {
		regParity(t, `[Cpus = 4; Owner = "alice"; x = 5]`, exprSrc)
		regParity(t, `[]`, exprSrc)
	}
	// Nested Elvis. Two joins land on the SAME instruction only when the nesting is to the RIGHT,
	// which needs explicit parentheses: `?:` is left-associative, so a bare chain patches each jump
	// to a different target and the join order never matters. Right-nested with a DEFINED outer left
	// operand is the one shape that distinguishes them -- it jumps past both joins, so the result has
	// to be the outermost destination, where every other shape threads the value through a chain of
	// moves and comes out right either way.
	for _, exprSrc := range []string{
		`Cpus ?: (Missing ?: 7)`, `Missing ?: (Cpus ?: 7)`, `Missing ?: (Missing ?: 7)`,
		`Cpus ?: (Missing ?: (Missing ?: 9))`, `Missing ?: (Missing ?: (Cpus ?: 9))`,
		`Cpus ?: Missing ?: 7`, `Missing ?: Cpus ?: 7`, `Missing ?: Missing ?: 7`,
		`(Missing ?: 3) + (Cpus ?: 4)`, `Missing ?: Cpus > 2`,
		`Cpus ?: (Missing ?: Missing)`, `(Cpus ?: 1) ?: (Missing ?: 2)`,
	} {
		regParity(t, `[Cpus = 4; Owner = "alice"]`, exprSrc)
		regParity(t, `[Owner = "alice"]`, exprSrc) // Cpus absent: the other path
	}
	// A logical short-circuit whose jump target COINCIDES with an Elvis join. This happens whenever
	// the Elvis fallback ends in a logical operator: both the Elvis and the combine finish at the
	// same instruction, so the short-circuit jump lands on the join. It has to run the join rather
	// than skip it -- its value IS what the fallback produced -- and it is the only shape in which
	// that distinction is observable.
	for _, exprSrc := range []string{
		`Missing ?: (Flag && Cpus > 2)`, `Missing ?: (Cpus > 100 && Missing2 > 0)`,
		`Missing ?: (Cpus > 2 || Missing2)`, `Missing ?: (Flag || Cpus > 2)`,
		`Missing ?: (Flag && Missing2)`, `Missing ?: ((Flag || Cpus > 2) && Cpus > 1)`,
		`Cpus ?: (Flag && Cpus > 2)`, // the Elvis takes its own jump instead
		`Missing ?: (Missing2 ?: (Flag && Cpus > 2))`,
		// The same shapes NOT at the end of the program, so the join lands on a real instruction
		// rather than on the trailing landing pad. These exercise a different branch of the lowering.
		`(Missing ?: (Flag && Cpus > 2)) + 1`, `(Missing ?: 5) + (Cpus ?: 6)`,
		`(Missing ?: (Flag || Cpus > 2)) == true`, `(Missing ?: (Flag && Cpus > 2)) ?: 99`,
		`((Missing ?: (Flag && Cpus > 2))) * 2`, `(Cpus ?: (Flag && Cpus > 2)) + 1`,
	} {
		for _, scopeSrc := range []string{
			`[Cpus = 4; Flag = false]`, // the && short-circuits
			`[Cpus = 4; Flag = true]`,  // it does not
			`[Cpus = 1; Flag = false]`,
			`[Flag = false]`, // Cpus absent too
		} {
			regParity(t, scopeSrc, exprSrc)
		}
	}
	// The vector executor's corpus too: its expressions are chosen to stress states (UNDEFINED,
	// ERROR, overflow, mixed types) rather than shapes, which is the complementary axis.
	for _, exprSrc := range vecExprs {
		regParity(t, `[A = 10; B = 3; F = true; S = "user3"]`, exprSrc)
		regParity(t, `[A = undefined; B = 0]`, exprSrc)
	}
}

// TestRegisterJoinShapes pins the two places the conversion is more than a transcription, by
// checking the instruction mix rather than just the value.
//
// A value test alone would not notice if a join were implemented by evaluating both sides and
// discarding one -- that produces the right answer for every total expression and the wrong one only
// where short-circuiting is load-bearing. These assertions say the structure is what it claims.
func TestRegisterJoinShapes(t *testing.T) {
	count := func(rp *RegProgram, op RegOp) int {
		n := 0
		for _, in := range rp.code {
			if in.Op == op {
				n++
			}
		}
		return n
	}
	lower := func(src string) *RegProgram {
		expr, err := parser.ParseExpr(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		return LowerToRegisters(CompileProgram(expr))
	}

	// A logical operator joins through a shared destination, so it needs no move.
	and := lower(`A > 1 && B > 2`)
	if got := count(and, RShortAnd); got != 1 {
		t.Errorf("A > 1 && B > 2: RShortAnd = %d, want 1", got)
	}
	if got := count(and, RCombineAnd); got != 1 {
		t.Errorf("A > 1 && B > 2: RCombineAnd = %d, want 1", got)
	}
	if got := count(and, RMove); got != 0 {
		t.Errorf("A > 1 && B > 2: RMove = %d, want 0 (a shared destination is the join)", got)
	}
	// The short-circuit and the combine must write the SAME register, or the two paths disagree.
	var shortDst, combineDst int32 = -1, -2
	for _, in := range and.code {
		switch in.Op {
		case RShortAnd:
			shortDst = in.Dst
		case RCombineAnd:
			combineDst = in.Dst
		}
	}
	if shortDst != combineDst {
		t.Errorf("short destination %d != combine destination %d; the join is broken", shortDst, combineDst)
	}

	// Elvis has no combine, so the fall-through path needs exactly one move.
	elvis := lower(`A ?: 5`)
	if got := count(elvis, RJmpIfDefined); got != 1 {
		t.Errorf("A ?: 5: RJmpIfDefined = %d, want 1", got)
	}
	if got := count(elvis, RMove); got != 1 {
		t.Errorf("A ?: 5: RMove = %d, want 1 (the fallback has to reach the destination)", got)
	}

	// Right-nested Elvis: both jumps target the same instruction, so both joins are applied there
	// and their order is observable. Written with parentheses on purpose -- `?:` is left-associative,
	// so `A ?: B ?: 7` gives each jump its own target and does not exercise this at all.
	nested := lower(`A ?: (B ?: 7)`)
	if got := count(nested, RJmpIfDefined); got != 2 {
		t.Errorf("A ?: (B ?: 7): RJmpIfDefined = %d, want 2", got)
	}
	if got := count(nested, RMove); got != 2 {
		t.Errorf("A ?: (B ?: 7): RMove = %d, want 2", got)
	}

	// The result must be the OUTERMOST destination, which for right-nesting is the first jump
	// emitted: its path skips both joins, so nothing else can be holding the value. Joins applied in
	// the wrong order leave the result in the inner destination, which on the outer jump path was
	// never written at all.
	var outerElvisDst int32 = -1
	for _, in := range nested.code {
		if in.Op == RJmpIfDefined {
			outerElvisDst = in.Dst
			break
		}
	}
	if nested.res != outerElvisDst {
		t.Errorf("A ?: (B ?: 7): result register %d, want the outermost Elvis destination %d",
			nested.res, outerElvisDst)
	}

	// Every jump target has to be a real instruction index.
	for _, src := range []string{`A ?: 5`, `A ?: B ?: 7`, `A > 1 && B > 2`, `A > 1 || B > 2`,
		`A > 1 && B > 2 && C > 3`, `(A ?: 1) + (B ?: 2)`, `A > 1 && (B ?: 2) > 1`} {
		rp := lower(src)
		for i, in := range rp.code {
			switch in.Op {
			case RShortAnd, RShortOr, RJmpIfDefined:
				if in.T < 0 || int(in.T) >= len(rp.code) {
					t.Errorf("%s: instruction %d jumps to %d, out of range [0,%d)", src, i, in.T, len(rp.code))
				}
			}
		}
	}
}

// TestRegisterShortCircuitIsReal is the claim a value-equality test cannot make on its own: the
// register form must NOT evaluate a right operand that short-circuiting skips.
//
// A is self-referential, so evaluating it is a cyclic reference. `false && A` is false only if A is
// never reached; an implementation that evaluated both operands would return error instead.
func TestRegisterShortCircuitIsReal(t *testing.T) {
	ad, err := classad.Parse(`[ A = A + 1; R1 = false && A; R2 = true || A; R3 = undefined ?: 7 ]`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, tc := range []struct{ attr, want string }{
		{"R1", "false"},
		{"R2", "true"},
		{"R3", "7"},
	} {
		expr, err := parser.ParseExpr(tc.attr)
		if err != nil {
			t.Fatalf("parse %s: %v", tc.attr, err)
		}
		p := CompileProgram(expr)
		stack := Run(p, ad)
		reg := RunRegisters(LowerToRegisters(p), ad)
		if !valuesEqual(stack, reg) {
			t.Errorf("%s: stack=%s register=%s", tc.attr, describe(stack), describe(reg))
		}
		if got := reg.String(); got != tc.want {
			t.Errorf("%s = %s, want %s (the skipped operand must never be evaluated)", tc.attr, got, tc.want)
		}
	}
}

// TestRegisterReusesSourcePools is the structural guarantee behind "an expression is lowered in
// exactly one place": the register form indexes the stack form's pools and owns none of its own, so
// the two cannot disagree about what a constant, an operator or a reference is.
func TestRegisterReusesSourcePools(t *testing.T) {
	expr, err := parser.ParseExpr(`Owner == "alice" && Cpus * 2 > 7 && member(X, {1,2}) && toUpper(S) == "Q"`)
	if err != nil {
		t.Fatal(err)
	}
	p := CompileProgram(expr)
	opsBefore := len(p.ops)
	rp := LowerToRegisters(p)
	if rp.Source() != p {
		t.Error("RegProgram does not point at the Program it was lowered from")
	}
	if len(p.ops) != opsBefore {
		t.Errorf("lowering appended to the source operator pool (%d -> %d); a Program is read-only after compilation",
			opsBefore, len(p.ops))
	}
	// Lowering twice must be side-effect free on the source, so a cached Program stays usable.
	rp2 := LowerToRegisters(p)
	if len(rp.code) != len(rp2.code) || rp.NumRegs() != rp2.NumRegs() {
		t.Errorf("lowering is not idempotent: %d instrs/%d regs then %d/%d",
			len(rp.code), rp.NumRegs(), len(rp2.code), rp2.NumRegs())
	}
}

// FuzzRegisterParity is the real gate on the conversion. The stack interpreter is the oracle, and it
// is itself differentially fuzzed against the tree-walking evaluator (FuzzDifferential), which is in
// turn checked against the C++ engine -- so a clean run here chains the register form onto that.
func FuzzRegisterParity(f *testing.F) {
	for _, ss := range scopeSources {
		for _, es := range exprSources {
			f.Add(ss, es)
		}
	}
	for _, es := range parityCorpusExprs {
		f.Add(`[Cpus = 4; Owner = "alice"; x = 5]`, es)
	}
	for _, es := range vecExprs {
		f.Add(`[A = 10; B = 3; F = true; S = "user3"]`, es)
	}
	for _, es := range []string{`Cpus ?: (Missing ?: 7)`, `Missing ?: (Cpus ?: 7)`,
		`Cpus ?: (Missing ?: (Missing ?: 9))`, `Cpus ?: Missing ?: 7`,
		`Missing ?: (Flag && Cpus > 2)`, `Missing ?: (Cpus > 2 || Missing2)`} {
		f.Add(`[Cpus = 4; Owner = "alice"]`, es)
		f.Add(`[Owner = "alice"]`, es)
	}
	f.Fuzz(func(t *testing.T, scopeSrc, exprSrc string) {
		scope, err := classad.Parse(scopeSrc)
		if err != nil || scope == nil {
			return
		}
		expr, err := parser.ParseExpr(exprSrc)
		if err != nil || expr == nil {
			return
		}
		p := CompileProgram(expr)
		want := Run(p, scope)
		got := RunRegisters(LowerToRegisters(p), scope)
		if !valuesEqual(want, got) {
			t.Fatalf("mismatch expr=%q scope=%q: stack=%s register=%s",
				exprSrc, scopeSrc, describe(want), describe(got))
		}
	})
}
