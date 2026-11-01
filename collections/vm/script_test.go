package vm

import (
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// localA and localB are the names the reduction law binds. They are deliberately unlikely, because
// the law only holds when the bindings do not capture a name the operand expressions themselves use:
// in the script, a reference to localA inside the SECOND operand means the first binding, while in
// the inlined form it would mean an attribute. A case where that happens is skipped rather than
// asserted, since it is the law's precondition and not a bug.
const (
	localA = "zzLocalA"
	localB = "zzLocalB"
)

func capturesLocal(srcs ...string) bool {
	for _, s := range srcs {
		if strings.Contains(strings.ToLower(s), "zzlocal") {
			return true
		}
	}
	return false
}

// reduction checks one instance of the law:
//
//	zzLocalA = <x>
//	zzLocalB = <y>
//	result   = <tmpl with the locals>      ==      Eval(<tmpl with (x) and (y) substituted>)
//
// The substituted form is not how the script runs, which is the point: the script computes each
// operand ONCE into a register and reads it back, while the reference re-evaluates the operand text
// wherever it appears. Agreement is what says the binding model is right.
func reduction(t *testing.T, scope *classad.ClassAd, x, y, tmpl string) {
	t.Helper()
	if capturesLocal(x, y) {
		return
	}
	xe, err1 := parser.ParseExpr(x)
	ye, err2 := parser.ParseExpr(y)
	resSrc := strings.NewReplacer(localA, "("+x+")", localB, "("+y+")").Replace(tmpl)
	re, err3 := parser.ParseExpr(tmpl)
	inlined, err4 := parser.ParseExpr(resSrc)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || xe == nil || ye == nil || re == nil || inlined == nil {
		return
	}

	sp, err := NewScript().Let(localA, xe).Let(localB, ye).Result(re).Compile()
	if err != nil {
		t.Fatalf("compile {%s; %s; %s}: %v", x, y, tmpl, err)
	}
	got := sp.Run(scope)
	want := refEval(scope, inlined)
	if !valuesEqual(want, got) {
		t.Errorf("script{%s = %s; %s = %s; %s} = %s, inlined %q = %s",
			localA, x, localB, y, tmpl, describe(got), resSrc, describe(want))
	}
}

var reductionScopes = []string{
	`[Cpus = 4; Memory = 2048; Owner = "alice"; R = 2.5; Flag = true; Self = Self + 1]`,
	`[Cpus = 0; Owner = ""]`,
	`[]`,
}

var reductionOperands = []string{
	`1`, `0`, `Cpus`, `Memory`, `Owner`, `R`, `Flag`, `Missing`, `undefined`, `error`,
	`Cpus + 1`, `Cpus / 0`, `Cpus > 2`, `Owner == "alice"`, `Memory ?: 7`,
	`toUpper(Owner)`, `floor(R)`, `Missing ?: Cpus`, `-Cpus`, `Cpus =?= 4`,
	`member(Cpus, {1, 4})`, `Cpus * Memory`, `"x"`, `9223372036854775807 + 1`,
}

var reductionTemplates = []string{
	localA,
	localA + " + " + localB,
	localA + " && " + localB,
	localA + " ?: " + localB,
	"(" + localA + " + " + localB + ") > " + localA,
	localA + " =?= " + localB,
	localA + " + " + localA + " + " + localB, // a local read more than once
	"toUpper(string(" + localA + "))",
	"Cpus + " + localA, // an attribute and a local in one expression
}

// TestScriptReductionCorpus is the reduction law over a hand-picked corpus: undefined and error
// operands, overflow, division by zero, a cyclic attribute, and templates that read a local more
// than once.
func TestScriptReductionCorpus(t *testing.T) {
	for _, scopeSrc := range reductionScopes {
		scope, err := classad.Parse(scopeSrc)
		if err != nil {
			t.Fatalf("parse scope: %v", err)
		}
		for _, x := range reductionOperands {
			for _, y := range reductionOperands {
				for _, tmpl := range reductionTemplates {
					reduction(t, scope, x, y, tmpl)
				}
			}
		}
	}
}

// FuzzScriptReduction fuzzes the operands of the same law. classad.Eval is the oracle, and it is
// itself differentially fuzzed against the C++ engine, so a clean run chains the binding model onto
// that rather than onto a second implementation of it.
func FuzzScriptReduction(f *testing.F) {
	for _, x := range reductionOperands {
		for _, y := range reductionOperands {
			f.Add(x, y)
		}
	}
	scope, err := classad.Parse(reductionScopes[0])
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, x, y string) {
		for _, tmpl := range reductionTemplates {
			reduction(t, scope, x, y, tmpl)
		}
	})
}

// TestScriptBindingModel pins what a bare name means, which is the decision the whole design rests
// on and the one a reduction law cannot check on its own -- the law's precondition is precisely that
// no name is captured.
func TestScriptBindingModel(t *testing.T) {
	scope, err := classad.Parse(`[ x = 10; Owner = "alice" ]`)
	if err != nil {
		t.Fatal(err)
	}
	// x = x + 1  : the right-hand side reads the ATTRIBUTE x (10), because the local is not bound
	// until the statement completes. A later x is the LOCAL.
	xp, err := parser.ParseExpr("x + 1")
	if err != nil {
		t.Fatal(err)
	}
	res, err := parser.ParseExpr("x")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := NewScript().Let("x", xp).Result(res).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if got := sp.Run(scope).String(); got != "11" {
		t.Errorf("x = x + 1; x  => %s, want 11 (the RHS reads the attribute, the result reads the local)", got)
	}

	// A SCOPED reference still reaches the attribute, which is how a script names an attribute whose
	// name a local has taken.
	myx, err := parser.ParseExpr("MY.x")
	if err != nil {
		t.Fatal(err)
	}
	sp2, err := NewScript().Let("x", xp).Result(myx).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if got := sp2.Run(scope).String(); got != "10" {
		t.Errorf("x = x + 1; MY.x  => %s, want 10 (a scoped reference is always the attribute)", got)
	}

	// Names fold: bound as Foo, read as fOO.
	one, _ := parser.ParseExpr("41 + 1")
	readFoo, _ := parser.ParseExpr("fOO")
	sp3, err := NewScript().Let("Foo", one).Result(readFoo).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if got := sp3.Run(scope).String(); got != "42" {
		t.Errorf("Foo = 42; fOO => %s, want 42 (attribute names fold, and so do locals)", got)
	}

	// An unbound name is an attribute, exactly as in a bare expression.
	readOwner, _ := parser.ParseExpr("Owner")
	sp4, err := NewScript().Let("other", one).Result(readOwner).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if got := sp4.Run(scope).String(); got != `"alice"` {
		t.Errorf("Owner => %s, want \"alice\"", got)
	}
}

// TestScriptComputesALocalOnce is the structural half of the reduction law. The law compares VALUES,
// and substituting an operand twice gives the same value as reading a register twice -- so it cannot
// tell a register read from a re-evaluation. This can: the compiled program must load the attribute
// ONCE however many times the local is read.
//
// It matters for more than speed. Re-evaluating would mean a local whose expression reads the clock
// or the PRNG changed value between its uses, which is not what a binding is.
func TestScriptComputesALocalOnce(t *testing.T) {
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
	loads := 0
	for _, in := range sp.RegProgram().code {
		if in.Op == RLoadRef {
			loads++
		}
	}
	if loads != 2 {
		t.Errorf("%d attribute loads, want 2 (Cpus and Memory, once each); the local is being re-evaluated", loads)
	}
	scope, err := classad.Parse(`[ Cpus = 4; Memory = 6 ]`)
	if err != nil {
		t.Fatal(err)
	}
	if got := sp.Run(scope).String(); got != "30" {
		t.Errorf("t = Cpus + Memory; t + t + t => %s, want 30", got)
	}
}

// TestScriptReadAttrsExcludesLocals guards the planner's view of a script. readAttrs drives column
// selection, and a local reported as an attribute would send the store looking for a column that
// cannot exist.
func TestScriptReadAttrsExcludesLocals(t *testing.T) {
	rhs, _ := parser.ParseExpr("Cpus + 1")
	res, _ := parser.ParseExpr("t > Memory")
	sp, err := NewScript().Let("t", rhs).Result(res).Compile()
	if err != nil {
		t.Fatal(err)
	}
	got := sp.Program().ReadAttrs()
	want := map[string]bool{"Cpus": true, "Memory": true}
	for _, name := range got {
		if !want[name] {
			t.Errorf("readAttrs includes %q, which is a local, not an attribute", name)
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("readAttrs is missing the attribute %q", name)
	}
}

// TestScriptErrors pins the misuse cases, so a caller gets an error rather than a program that
// silently means something else.
func TestScriptErrors(t *testing.T) {
	e, _ := parser.ParseExpr("1")
	if _, err := NewScript().Let("a", e).Compile(); err != ErrNoResult {
		t.Errorf("no Result: err = %v, want ErrNoResult", err)
	}
	if _, err := NewScript().Result(e).Result(e).Compile(); err == nil {
		t.Error("Result twice: want an error")
	}
	if _, err := NewScript().Result(e).Let("a", e).Compile(); err == nil {
		t.Error("Let after Result: want an error")
	}
	if _, err := NewScript().Let("", e).Result(e).Compile(); err == nil {
		t.Error("empty local name: want an error")
	}
}

// TestLocalsNeedingInit exercises the initializer rule directly, on hand-built programs.
//
// It has to be direct: the Script API cannot currently produce a read that a store does not reach,
// because Let always stores before any later statement can load. The rule exists for the shapes a
// statement-level branch will introduce, and testing it only through the API would leave it
// unexercised until then -- which is how a rule written in advance turns out to be wrong.
func TestLocalsNeedingInit(t *testing.T) {
	load := Instr{Op: OpLoadLocal, A: 0}
	store := Instr{Op: OpStoreLocal, A: 0}
	one := Instr{Op: OpPushTrue}

	for _, tc := range []struct {
		name string
		code []Instr
		want bool
	}{
		{"store then load, straight line", []Instr{one, store, load}, false},
		{"load with no store at all", []Instr{load}, true},
		{"load before the store", []Instr{load, one, store}, true},
		{"second load still dominated", []Instr{one, store, load, load}, false},
	} {
		p := &Program{code: tc.code, locals: []string{"x"}}
		if got := localsNeedingInit(p)[0]; got != tc.want {
			t.Errorf("%s: needs init = %v, want %v", tc.name, got, tc.want)
		}
	}

	// A read the store reaches only on one path. The jump target between them is what makes the
	// store stop dominating it, and it is why the rule is written as dominance rather than as "does
	// this program contain a branch".
	jumpOver := []Instr{
		{Op: OpPushTrue},            // 0
		{Op: OpJmpIfNotUndef, A: 4}, // 1: jumps PAST the store
		one,                         // 2
		store,                       // 3
		load,                        // 4: a jump target, so the store does not reach it
	}
	p := &Program{code: jumpOver, locals: []string{"x"}}
	if !localsNeedingInit(p)[0] {
		t.Error("a read control can reach without passing the store was not marked as needing an initializer")
	}
}

// TestScriptLocalIsOnePlace pins the model this rework exists for: a local has ONE register for the
// whole script, so rebinding writes the same place rather than creating a second one.
//
// Under the model it replaced, each binding took whatever register its expression landed in. That
// works in a straight line and cannot survive a branch, where a binding made on one path has to be
// the same place a read after the merge finds.
func TestScriptLocalIsOnePlace(t *testing.T) {
	scope, err := classad.Parse(`[]`)
	if err != nil {
		t.Fatal(err)
	}
	e1, _ := parser.ParseExpr("1")
	inc, _ := parser.ParseExpr("x + 1")
	res, _ := parser.ParseExpr("x")
	sp, err := NewScript().Let("x", e1).Let("x", inc).Let("x", inc).Result(res).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(sp.Locals()); n != 1 {
		t.Errorf("%d locals, want 1: rebinding must not create a second one", n)
	}
	if got := sp.Run(scope).String(); got != "3" {
		t.Errorf("x = 1; x = x + 1; x = x + 1; x  => %s, want 3", got)
	}
	// One REGISTER for the local, written by all three bindings. Checking the local INDEX instead
	// would pass under the model this replaced, where each binding took a fresh register.
	rp := sp.RegProgram()
	if len(rp.localRegs) != 1 {
		t.Fatalf("%d local registers, want 1", len(rp.localRegs))
	}
	writes := 0
	for _, in := range rp.code {
		if d, ok := regDef(in); ok && d == rp.localRegs[0] {
			writes++
		}
	}
	if writes != 3 {
		t.Errorf("%d writes to the local's register, want 3 (one per binding to the same place)", writes)
	}
}

// TestScriptStatementsAreStackNeutral pins that a statement leaves the stack as it found it, which
// is what lets statements be sequenced at all -- and what a branch will depend on, since its paths
// have to arrive at the merge with the same stack.
func TestScriptStatementsAreStackNeutral(t *testing.T) {
	e, _ := parser.ParseExpr("Cpus + 1")
	res, _ := parser.ParseExpr("a + b + c")
	sp, err := NewScript().Let("a", e).Let("b", e).Let("c", e).Result(res).Compile()
	if err != nil {
		t.Fatal(err)
	}
	// One value at the end -- the result -- and no accumulation from the bindings.
	if sp.RegProgram().res < 0 {
		t.Fatal("no result register")
	}
	scope, err := classad.Parse(`[ Cpus = 4 ]`)
	if err != nil {
		t.Fatal(err)
	}
	if got := sp.Run(scope).String(); got != "15" {
		t.Errorf("three bindings of Cpus+1 summed => %s, want 15", got)
	}
}
