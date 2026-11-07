package vm

import (
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// THE TERNARY'S PARITY TESTS.
//
// `cond ? t : f` is compiled natively (OpTernBranch/OpTernTrue/OpTernFalse), and the only thing that
// makes that safe is agreement with the tree-walking evaluator -- which does something a two-way
// branch cannot reproduce. An UNDEFINED condition evaluates BOTH arms, discards both values and
// yields UNDEFINED, so an arm that cannot be evaluated at all (a cyclic self-reference) turns the
// whole expression into ERROR while an arm that merely evaluates TO an error is absorbed.
//
// EVERY CASE HERE ASSERTS THE NATIVE PATH RAN. Without that the table passes by delegating, which is
// exactly what it is meant to stop being true, and the suite would stay green through a change that
// quietly put the ternary back on the tree-walker.

// ternaryAd is the scope every case below is evaluated against. Cyc is self-referential: evaluating
// it PANICS inside the evaluator, which both engines recover as ERROR, so it is how an arm that must
// not be reached makes itself heard.
const ternaryAd = `[ Cpus = 4; Zero = 0; Flag = true; Off = false; Name = "ap"; L = {1, 2};
	Nested = [ x = 1 ]; Cyc = Cyc + 1 ]`

var ternaryCases = []string{
	// Plain true and false conditions.
	`Flag ? 1 : 2`, `Off ? 1 : 2`, `true ? "a" : "b"`, `false ? "a" : "b"`,
	`Cpus > 2 ? 1 : 2`, `Cpus > 9 ? 1 : 2`,
	// NUMERIC TRUTHINESS: a nonzero number is true and a zero is false, the same rule && and ||
	// use. `0.0 ? 1 : 2` is 2, not an error.
	`1 ? 1 : 2`, `0 ? 1 : 2`, `2.5 ? 1 : 2`, `0.0 ? 1 : 2`, `-1 ? 1 : 2`,
	`Cpus ? 1 : 2`, `Zero ? 1 : 2`,
	// AN UNDEFINED CONDITION, with each arm shape. The value is UNDEFINED in every one of these
	// except the last two, where an arm cannot be evaluated at all.
	`Missing ? 1 : 2`, `undefined ? 1 : 2`,
	`Missing ? 1 : error`, `Missing ? error : 1`, `Missing ? error : error`,
	`Missing ? Name : L`, `Missing ? Nested : 1`, `Missing ? {} : 1`,
	`Missing ? 1 : Cyc`, `Missing ? Cyc : 1`, `Missing ? Cyc : Cyc`,
	`Missing ? 1 : (1 / 0)`,
	// A CONDITION WITH NO TRUTHINESS: a string, a list, an ad, an error. Each is ERROR, and
	// NEITHER arm runs -- which is why a cyclic arm does not change the answer here.
	`Name ? 1 : 2`, `"x" ? 1 : 2`, `L ? 1 : 2`, `{} ? 1 : 2`, `Nested ? 1 : 2`,
	`error ? 1 : 2`, `(1 / 0) ? 1 : 2`, `Name ? Cyc : Cyc`, `error ? Cyc : Cyc`,
	// A CYCLIC CONDITION, which neither engine can read at all.
	`Cyc ? 1 : 2`,
	// ONLY THE SELECTED ARM RUNS when the condition is true or false, so a cyclic arm on the other
	// side must not be reached.
	`Flag ? 1 : Cyc`, `Off ? Cyc : 2`, `Cpus > 2 ? 1 : Cyc`, `Cpus > 9 ? Cyc : 2`,
	// NESTING, in the condition and in each arm, including a chain -- the shape a ClassAd lookup
	// table takes.
	`(Flag ? Off : Flag) ? 1 : 2`,
	`Flag ? (Off ? 1 : 2) : 3`, `Off ? 1 : (Flag ? 2 : 3)`,
	`Flag ? (Off ? 1 : 2) : (Flag ? 3 : 4)`,
	`Cpus == 1 ? "a" : (Cpus == 2 ? "b" : (Cpus == 4 ? "c" : "d"))`,
	`Missing ? (Missing ? 1 : 2) : (Missing ? 3 : 4)`,
	`Missing ? (Missing ? Cyc : 2) : 3`,
	// Attributes present and absent, on both sides.
	`Flag ? Cpus : Missing`, `Off ? Cpus : Missing`, `Missing ? Cpus : Missing`,
	`Cpus > 2 ? Name : Missing`, `Missing2 ? Missing3 : Missing4`,
	// The ternary as an operand, so its value flows on rather than ending the expression.
	`(Flag ? 1 : 2) + 1`, `-(Flag ? 1 : 2)`, `(Flag ? 1 : 2) == 1`,
	`toUpper(Cpus > 2 ? "a" : "b")`, `(Missing ? 1 : 2) + 1`, `(Name ? 1 : 2) + 1`,
	// Mixed with the other branching constructs, which share its joins.
	`Missing ?: (Flag ? 1 : 2)`, `(Missing ? 1 : 2) ?: 9`,
	`ifThenElse(Flag, Off ? 1 : 2, 3)`, `Flag ? ifThenElse(Off, 1, 2) : 3`,
	`Flag && (Off ? true : false)`, `(Missing ? true : false) || Flag`,
}

// assertTernaryNative is the guard against a vacuous pass. With the ternary delegated, the "native"
// side of every comparison below IS the tree-walker and the whole table compares the evaluator to
// itself -- a green suite that proves nothing, which this package has produced before.
//
// It asks two things, because neither alone is enough. The three opcodes must be there, so something
// was lowered; and no delegated subtree may be a conditional, so nothing was lowered AROUND one. A
// delegated subtree of another kind is expected and fine: `Missing ? {} : 1` has a native ternary
// over a list literal, which stays delegated because its elements are lazy.
func assertTernaryNative(t *testing.T, p *Program, src string) {
	t.Helper()
	for _, op := range []Opcode{OpTernBranch, OpTernTrue, OpTernFalse} {
		if !usesOp(p, op) {
			t.Fatalf("%s: opcode %d missing; the ternary did not compile natively", src, op)
		}
	}
	for _, n := range p.nodes {
		if _, isTernary := n.(*ast.ConditionalExpr); isTernary {
			t.Fatalf("%s: %s was delegated; this case proves nothing about the native path", src, n)
		}
	}
}

// TestTernaryMatchesTheEvaluator is the differential table: every shape above, compiled natively and
// evaluated by the tree-walker, must agree exactly.
func TestTernaryMatchesTheEvaluator(t *testing.T) {
	scope, err := classad.Parse(ternaryAd)
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range ternaryCases {
		t.Run(src, func(t *testing.T) {
			expr, perr := parser.ParseExpr(src)
			if perr != nil {
				t.Fatalf("parse: %v", perr)
			}
			p := CompileProgram(expr)
			assertTernaryNative(t, p, src)
			want := refEval(scope, expr)
			if got := Run(p, scope); !valuesEqual(got, want) {
				t.Errorf("stack vm = %s, evaluator = %s", describe(got), describe(want))
			}
			// The register form is lowered from the same Program and must agree too, unoptimized
			// and optimized: the ternary writes one destination from three instructions, which is a
			// join the optimizer has to leave alone.
			rp := LowerToRegisters(p)
			if got := RunRegisters(rp, scope); !valuesEqual(got, want) {
				t.Errorf("register vm = %s, evaluator = %s", describe(got), describe(want))
			}
			if got := RunRegisters(Optimize(rp, AllPasses()), scope); !valuesEqual(got, want) {
				t.Errorf("optimized register vm = %s, evaluator = %s", describe(got), describe(want))
			}
		})
	}
}

// TestTernaryUndefinedEvaluatesBothArms is the rule the whole lowering is shaped around, stated on
// its own so a change that drops it fails with a message that says what was lost.
//
// `undefined ? 1 : error` is UNDEFINED: an error VALUE in an arm is absorbed. `undefined ? {} : Cyc`
// is ERROR: a cyclic self-reference is a hard failure that propagates. The second is only reachable
// by an engine that EVALUATES the arm it is about to discard.
func TestTernaryUndefinedEvaluatesBothArms(t *testing.T) {
	scope, err := classad.Parse(ternaryAd)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ src, want string }{
		{`Missing ? 1 : error`, "undefined"},
		{`Missing ? error : 1`, "undefined"},
		{`Missing ? 1 : 2`, "undefined"},
		{`Missing ? {} : Cyc`, "error"},
		{`Missing ? Cyc : {}`, "error"},
	}
	for _, tc := range cases {
		expr, perr := parser.ParseExpr(tc.src)
		if perr != nil {
			t.Fatalf("parse %q: %v", tc.src, perr)
		}
		p := CompileProgram(expr)
		assertTernaryNative(t, p, tc.src)
		got := Run(p, scope)
		if got.String() != tc.want {
			t.Errorf("%s = %s, want %s", tc.src, describe(got), tc.want)
		}
		if ref := refEval(scope, expr); !valuesEqual(got, ref) {
			t.Errorf("%s: vm %s, evaluator %s", tc.src, describe(got), describe(ref))
		}
	}
}

// TestTernaryNonCoercibleRunsNeitherArm is the other half: a condition with no truthiness is ERROR
// and the arms are never reached, so a cyclic arm is harmless there.
func TestTernaryNonCoercibleRunsNeitherArm(t *testing.T) {
	scope, err := classad.Parse(ternaryAd)
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{`Name ? Cyc : Cyc`, `L ? Cyc : Cyc`, `Nested ? Cyc : Cyc`,
		`error ? Cyc : Cyc`} {
		expr, perr := parser.ParseExpr(src)
		if perr != nil {
			t.Fatalf("parse %q: %v", src, perr)
		}
		p := CompileProgram(expr)
		assertTernaryNative(t, p, src)
		got := Run(p, scope)
		if !got.IsError() {
			t.Errorf("%s = %s, want error", src, describe(got))
		}
		if ref := refEval(scope, expr); !valuesEqual(got, ref) {
			t.Errorf("%s: vm %s, evaluator %s", src, describe(got), describe(ref))
		}
	}
}

// TestTernaryIsNotExponentialInNesting pins the property the three-opcode shape buys: each arm is
// emitted once, so a chain of ternaries grows linearly. A lowering with one block per condition
// state would emit each arm twice and double with every level, which at depth 12 is a program of
// thousands of instructions rather than tens.
func TestTernaryIsNotExponentialInNesting(t *testing.T) {
	src := "0"
	for i := 0; i < 12; i++ {
		src = "Flag ? 1 : (" + src + ")"
	}
	expr, err := parser.ParseExpr(src)
	if err != nil {
		t.Fatal(err)
	}
	p := CompileProgram(expr)
	// Six instructions per level is the whole construct: the condition load, the branch, the true
	// arm, its closer, the false arm and its closer. Twice that is slack, and still far below the
	// 2^12 a duplicating lowering would reach.
	if limit := 12 * 12; p.Len() > limit {
		t.Errorf("%d instructions for 12 nested ternaries, want at most %d; the arms are being duplicated",
			p.Len(), limit)
	}
}

// TestTernaryInAScriptReadsLocals pins that the arms are ordinary compiled expressions, so a script
// local is readable from either of them -- which a delegated ternary could only do through the
// resolver overlay, and could not do at all inside a list or ad literal.
func TestTernaryInAScriptReadsLocals(t *testing.T) {
	scope, err := classad.Parse(`[ Cpus = 4 ]`)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := NewScript().
		Let("n", pe(t, "Cpus + 1")).
		Let("big", pe(t, "n > 3")).
		Result(pe(t, "big ? n * 2 : n")).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if nodes := len(sp.Program().nodes); nodes != 0 {
		t.Fatalf("%d delegated node(s); the ternary should compile natively", nodes)
	}
	got := sp.Run(scope)
	if n, nerr := got.IntValue(); nerr != nil || n != 10 {
		t.Errorf("result = %s, want 10", describe(got))
	}
}

// TestTernaryRegistersAsAUDF is the point of making the ternary native, stated as the thing an
// author can now do: a function whose body uses `? :` passes the purity gate and registers.
//
// The gate refuses any program with a delegated node (rule 4), so this fails outright for as long as
// the ternary is delegated -- there is no partial credit and no way for it to pass vacuously.
func TestTernaryRegistersAsAUDF(t *testing.T) {
	m, err := ParseScriptModule("def pick(x, y):\n    return (x > y ? x : y)\n")
	if err != nil {
		t.Fatal(err)
	}
	f, err := m.Compile("pick")
	if err != nil {
		t.Fatalf("Compile: %v; a ternary over parameters reaches no ad", err)
	}
	if f.Arity() != 2 {
		t.Fatalf("arity = %d, want 2", f.Arity())
	}
	fn := f.Func()
	cases := []struct {
		a, b classad.Value
		want string
	}{
		{classad.NewIntValue(3), classad.NewIntValue(7), "7"},
		{classad.NewIntValue(9), classad.NewIntValue(7), "9"},
		// An undefined argument makes the COMPARISON undefined, so both arms are evaluated and the
		// result is undefined -- the same answer the evaluator gives for the same body.
		{classad.NewUndefinedValue(), classad.NewIntValue(7), "undefined"},
		// A string against a number compares to error, which has no truthiness: neither arm runs.
		{classad.NewStringValue("x"), classad.NewIntValue(7), "error"},
	}
	for _, tc := range cases {
		if got := fn([]classad.Value{tc.a, tc.b}); got.String() != tc.want {
			t.Errorf("pick(%s, %s) = %s, want %s", tc.a, tc.b, describe(got), tc.want)
		}
	}
}

// TestTernaryVectorCoversEveryConditionState compares the vector executor with the scalar one over a
// batch whose lanes take all four outcomes, and checks that the batch really does contain all four
// -- a lane distribution that happened to miss one would leave the arm-mask arithmetic untested.
func TestTernaryVectorCoversEveryConditionState(t *testing.T) {
	src := newTestSource([]map[string]string{
		{"C": "true", "A": "1", "B": "2"},       // true
		{"C": "false", "A": "3", "B": "4"},      // false
		{"C": "7", "A": "5", "B": "6"},          // a nonzero number: true
		{"C": "0", "A": "7", "B": "8"},          // zero: false
		{"C": "undefined", "A": "9", "B": "10"}, // undefined: both arms run, result undefined
		{"A": "11", "B": "12"},                  // C absent: undefined as well
		{"C": `"s"`, "A": "13", "B": "14"},      // a string: no truthiness, neither arm runs
		{"C": "error", "A": "15", "B": "16"},    // an error: likewise
	})
	n := len(src.recs)

	// The lane distribution itself, so a corpus that stopped covering a state fails here rather
	// than silently narrowing what the comparison below exercises.
	seen := map[classad.CondState]bool{}
	cq, err := Parse("C")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		seen[classad.ConditionState(cq.Eval(src.ad(i)))] = true
	}
	for _, st := range []classad.CondState{classad.CondTrue, classad.CondFalse, classad.CondUndef,
		classad.CondError} {
		if !seen[st] {
			t.Fatalf("no lane with condition state %v; the batch does not cover every outcome", st)
		}
	}

	var scratch VecScratch
	for _, expr := range []string{
		`C ? A : B`, `C ? A + 1 : B * 2`, `C ? "yes" : "no"`, `C ? A : Missing`,
		`(C ? A : B) + 1`, `C ? (A > 2 ? 1 : 2) : 3`, `C ? A : (C ? B : 0)`,
		`C ?: (C ? A : B)`, `(C ? A : B) ?: 99`, `ifThenElse(C, C ? A : B, 0)`,
	} {
		q, perr := Parse(expr)
		if perr != nil {
			t.Fatalf("parse %q: %v", expr, perr)
		}
		if !q.Native() {
			t.Fatalf("%q is not native; the vector executor would decline for that reason alone", expr)
		}
		got, ok := q.VecEval(src, n, &scratch)
		if !ok {
			t.Fatalf("%q: the vector executor declined", expr)
		}
		for i := 0; i < n; i++ {
			want := q.Eval(src.ad(i))
			if !sameValue(got.value(i), want) {
				t.Errorf("%q record %d: vector %s != scalar %s", expr, i, got.value(i), want)
			}
		}
	}
}

// TestFaultCauseNamesWhatTheConditionHeld pins the three causes apart. The class says a condition
// could not be read; the cause says what it held, and "undefined" versus "an error value" versus "a
// string" are three different mistakes with three different fixes.
func TestFaultCauseNamesWhatTheConditionHeld(t *testing.T) {
	scope, err := classad.Parse(`[ Name = "ap"; Cpus = 4 ]`)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		cond  string
		cause FaultCause
		words string
	}{
		{"undefined", "Missing", CauseUndefined, "undefined"},
		{"error value", "1 / 0", CauseError, "error value"},
		{"string", "Name", CauseNotBoolean, "no truthiness"},
		{"list", "{1, 2}", CauseNotBoolean, "no truthiness"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp, cerr := NewScript().
				If(pe(t, tc.cond), func(s *Script) { s.Let("a", pe(t, "1")) }, nil).
				Result(pe(t, "2")).Compile()
			if cerr != nil {
				t.Fatal(cerr)
			}
			_, _, fault := sp.RunWithFault(scope)
			if fault == nil {
				t.Fatalf("condition %q did not fault", tc.cond)
			}
			if fault.Class != FaultCondition {
				t.Errorf("class = %v, want %v", fault.Class, FaultCondition)
			}
			if fault.Cause != tc.cause {
				t.Errorf("cause = %v, want %v", fault.Cause, tc.cause)
			}
			if !strings.Contains(fault.Error(), tc.words) {
				t.Errorf("%q does not say %q", fault.Error(), tc.words)
			}
			if !strings.Contains(fault.Error(), "if #1 condition") {
				t.Errorf("%q no longer names the construct", fault.Error())
			}
		})
	}
}

// TestTernaryControlFlowGraphKnowsEveryPath pins the successor tables for the ternary's opcodes.
//
// Both tables are a second statement of the executors' dispatch, and cfg.go's rule is that a
// successor they MISS is a path dominance does not know about -- which makes a dominance answer
// wrong rather than merely conservative, and every pass that trusts dominance moves work across it.
// Nothing the compiler emits today depends on the ternary's join edges (the outcome that runs no arm
// already reaches the join without passing through either), so no result would change if one were
// dropped; this is what keeps that from being discovered by a pass that assumed otherwise.
func TestTernaryControlFlowGraphKnowsEveryPath(t *testing.T) {
	expr, err := parser.ParseExpr(`(Flag ? Cpus : Zero) + 1`)
	if err != nil {
		t.Fatal(err)
	}
	p := CompileProgram(expr)

	var buf []int
	found := 0
	for ip, in := range p.code {
		switch in.Op {
		case OpTernBranch:
			found++
			ct := p.conds[in.A]
			// The true arm (and the undefined outcome with it), the false arm, and the join for a
			// condition with no truthiness, which runs no arm at all.
			want := []int{ip + 1, int(ct.onFalse), int(ct.end)}
			if got := append([]int(nil), stackSuccs(p, ip, buf)...); !sameSuccs(got, want) {
				t.Errorf("stackSuccs(OpTernBranch) = %v, want %v", got, want)
			}
		case OpTernTrue:
			found++
			ct := p.conds[in.A]
			// The join on the true outcome, the false arm on the undefined one.
			want := []int{ip + 1, int(ct.end)}
			if got := append([]int(nil), stackSuccs(p, ip, buf)...); !sameSuccs(got, want) {
				t.Errorf("stackSuccs(OpTernTrue) = %v, want %v", got, want)
			}
		}
	}
	if found != 2 {
		t.Fatalf("found %d ternary control-flow instructions, want 2", found)
	}

	rp := LowerToRegisters(p)
	found = 0
	var rbuf []int
	for ip, in := range rp.code {
		switch in.Op {
		case RTernBranch:
			found++
			want := []int{ip + 1, int(rp.conds[in.Imm].onFalse), int(in.T)}
			if got := append([]int(nil), regSuccs(rp, ip, rbuf)...); !sameSuccs(got, want) {
				t.Errorf("regSuccs(RTernBranch) = %v, want %v", got, want)
			}
		case RTernTrue:
			found++
			want := []int{ip + 1, int(in.T)}
			if got := append([]int(nil), regSuccs(rp, ip, rbuf)...); !sameSuccs(got, want) {
				t.Errorf("regSuccs(RTernTrue) = %v, want %v", got, want)
			}
		}
	}
	if found != 2 {
		t.Fatalf("found %d register-form ternary control-flow instructions, want 2", found)
	}

	// And no instruction is stranded: a target the lowering got wrong shows up here as code the
	// graph cannot reach.
	c := regCFG(rp)
	for ip := range rp.code {
		if !c.reach[ip] {
			t.Errorf("register instruction %d is unreachable", ip)
		}
	}
}

// sameSuccs compares two successor lists as SETS, because the order they are appended in is an
// implementation detail of the successor function and not something a caller may rely on.
func sameSuccs(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[int]int{}
	for _, v := range got {
		seen[v]++
	}
	for _, v := range want {
		seen[v]--
		if seen[v] < 0 {
			return false
		}
	}
	return true
}
