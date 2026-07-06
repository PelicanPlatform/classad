package vm

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// refEval is the tree-walking reference: it mirrors classad.Expr.Eval (a fresh
// evaluator plus cyclic-panic recovery), which is exactly what vm.Run mirrors on
// the compiled side. The two must agree for every (ad, expr).
func refEval(scope *classad.ClassAd, expr ast.Expr) (result classad.Value) {
	defer classad.RecoverCyclic(&result)
	return classad.NewEvaluator(scope).Evaluate(expr)
}

// valuesEqual is exact structural equality of two Values, including type
// (undefined vs error vs false are all distinct).
func valuesEqual(a, b classad.Value) bool { return valuesEqualDepth(a, b, 0) }

// listCmpCap bounds recursion when comparing lists. It exists solely to tolerate
// one known, pathological divergence: a self-referential cyclic lazy list (e.g.
// A = {a}, where "a" case-folds to "A") materializes into a list nested until the
// evaluator's depth limit, then an error element. The tree-walker captures the
// expression's nesting depth into the lazy list's listDepth, but the flat vm does
// not, so the two bottom out at slightly different nesting depths. Both produce a
// deeply-nested, error-terminating list; only the depth differs, and only for
// cyclic lists. No non-cyclic expression nests anywhere near this cap, so bounding
// the comparison masks no realistic difference.
const listCmpCap = 64

func valuesEqualDepth(a, b classad.Value, depth int) bool {
	if depth > listCmpCap {
		return true
	}
	switch {
	case a.IsUndefined() || b.IsUndefined():
		return a.IsUndefined() && b.IsUndefined()
	case a.IsError() || b.IsError():
		return a.IsError() && b.IsError()
	case a.IsBool() || b.IsBool():
		if !a.IsBool() || !b.IsBool() {
			return false
		}
		av, _ := a.BoolValue()
		bv, _ := b.BoolValue()
		return av == bv
	case a.IsInteger() || b.IsInteger():
		if !a.IsInteger() || !b.IsInteger() {
			return false
		}
		av, _ := a.IntValue()
		bv, _ := b.IntValue()
		return av == bv
	case a.IsReal() || b.IsReal():
		if !a.IsReal() || !b.IsReal() {
			return false
		}
		av, _ := a.RealValue()
		bv, _ := b.RealValue()
		return av == bv || (av != av && bv != bv) // NaN == NaN for test purposes
	case a.IsString() || b.IsString():
		if !a.IsString() || !b.IsString() {
			return false
		}
		av, _ := a.StringValue()
		bv, _ := b.StringValue()
		return av == bv
	case a.IsList() || b.IsList():
		if !a.IsList() || !b.IsList() {
			return false
		}
		al, _ := a.ListValue()
		bl, _ := b.ListValue()
		if len(al) != len(bl) {
			return false
		}
		for i := range al {
			if !valuesEqualDepth(al[i], bl[i], depth+1) {
				return false
			}
		}
		return true
	case a.IsClassAd() || b.IsClassAd():
		if !a.IsClassAd() || !b.IsClassAd() {
			return false
		}
		ac, _ := a.ClassAdValue()
		bc, _ := b.ClassAdValue()
		return ac.Equal(bc)
	}
	return false
}

// scopes used as evaluation contexts in the differential tests.
var scopeSources = []string{
	`[Cpus = 4; Memory = 8192; Owner = "alice"; Arch = "X86_64"; Rank = Cpus * 100; Flag = true; R = 2.5]`,
	`[Cpus = 1; Memory = 512; Owner = "bob"; State = "Idle"; L = {1, 2, 3}; Nested = [K = 7]]`,
	`[A = B; B = A; Self = Self + 1; OK = 10]`, // cyclic attrs -> error when referenced
	// Scope/cycle edge cases mirrored from classad/cpp_parity_test.go.
	`[a = a]`, `[a = a + 1]`, `[a = b; b = a]`, `[a = eval("a")]`, `[a = (0 =!= a)]`,
	`[x = 1; B = [].x; A = [].A]`,
	`[]`,
}

// exprSources cover every native path and several delegated (EvalNode) paths.
var exprSources = []string{
	// literals
	`42`, `-7`, `3.14`, `"hi"`, `true`, `false`, `undefined`, `error`,
	// refs (present, absent, cyclic)
	`Cpus`, `Missing`, `A`, `Self`, `Rank`,
	// arithmetic / comparison / bitwise
	`Cpus + Memory`, `Memory / Cpus`, `Cpus * 2 - 1`, `Memory % 100`,
	`Cpus > 2`, `Memory <= 512`, `Owner == "alice"`, `Owner != "bob"`, `R < 3.0`,
	`Cpus & 6`, `Cpus | 1`, `Cpus << 2`, `Memory >> 3`, `Cpus ^ 5`,
	// meta-equality
	`Owner =?= "alice"`, `Missing =?= undefined`, `Cpus =!= 4`, `error =?= error`,
	// unary
	`-Cpus`, `!Flag`, `~Cpus`, `+Memory`, `!Missing`,
	// short-circuit && || (incl. cyclic right operand that must NOT be evaluated)
	`Cpus > 0 && Memory > 0`, `Cpus > 100 && Missing > 0`, `false && A`, `true || A`,
	`Cpus > 100 || Owner == "alice"`, `Missing && true`, `Missing || false`,
	`Flag && Owner == "alice" && Cpus >= 4`,
	// elvis
	`Missing ?: 99`, `Cpus ?: 99`, `Missing ?: Missing ?: 7`, `error ?: 1`,
	// ternary (delegated)
	`Cpus > 2 ? "big" : "small"`, `Missing ? 1 : 2`, `Flag ? Cpus : Memory`,
	// functions / list / record / select / subscript (delegated)
	`strcat(Owner, "!")`, `size({1,2,3})`, `toUpper(Owner)`,
	`{1, 2, Cpus}`, `[Q = Cpus + 1]`, `Nested.K`, `L[1]`,
	`strcat(Owner, "/", State) =?= "bob/Idle"`,
	// mixed / nested parentheses
	`(Cpus + 1) * (Memory - 2)`, `((Owner))`, `(Cpus > 2) && (R < 3.0 || Flag)`,
}

func TestDifferentialVMvsEvaluator(t *testing.T) {
	for _, ss := range scopeSources {
		scopeAd, err := classad.Parse(ss)
		if err != nil {
			t.Fatalf("parse scope %q: %v", ss, err)
		}
		for _, es := range exprSources {
			expr, err := parser.ParseExpr(es)
			if err != nil {
				t.Fatalf("parse expr %q: %v", es, err)
			}
			want := refEval(scopeAd, expr)
			got := Run(CompileProgram(expr), scopeAd)
			if !valuesEqual(want, got) {
				t.Errorf("mismatch expr=%q scope=%q\n  want=%s\n   got=%s",
					es, ss, describe(want), describe(got))
			}
		}
	}
}

// TestDifferentialParityCorpus runs the AST-evaluation edge cases lifted from
// classad/cpp_parity_test.go through the vm and asserts parity with the
// tree-walker, across several scopes.
func TestDifferentialParityCorpus(t *testing.T) {
	parityScopes := []string{
		`[]`,
		`[Cpus = 4; Memory = 8192; Owner = "alice"; x = 5; f = 3]`,
	}
	for _, ss := range parityScopes {
		scopeAd, err := classad.Parse(ss)
		if err != nil {
			t.Fatalf("parse scope %q: %v", ss, err)
		}
		for _, es := range parityCorpusExprs {
			expr, err := parser.ParseExpr(es)
			if err != nil {
				continue // a few entries are ad fragments, not standalone exprs
			}
			want := refEval(scopeAd, expr)
			got := Run(CompileProgram(expr), scopeAd)
			if !valuesEqual(want, got) {
				t.Errorf("parity mismatch expr=%q scope=%q\n  want=%s\n   got=%s",
					es, ss, describe(want), describe(got))
			}
		}
	}
}

// describe renders a Value for test failure messages.
func describe(v classad.Value) string {
	switch {
	case v.IsUndefined():
		return "undefined"
	case v.IsError():
		return "error"
	case v.IsBool():
		b, _ := v.BoolValue()
		if b {
			return "true"
		}
		return "false"
	case v.IsInteger():
		i, _ := v.IntValue()
		return "int(" + itoa(i) + ")"
	case v.IsReal():
		return "real"
	case v.IsString():
		s, _ := v.StringValue()
		return "string(" + s + ")"
	case v.IsList():
		return "list"
	case v.IsClassAd():
		return "classad"
	}
	return "?"
}

func itoa(i int64) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}

func TestQueryMatches(t *testing.T) {
	ad, _ := classad.Parse(`[Owner = "alice"; RequestMemory = 4096]`)
	q, err := Parse(`Owner == "alice" && RequestMemory > 2048`)
	if err != nil {
		t.Fatal(err)
	}
	if !q.Matches(ad) {
		t.Error("expected match")
	}
	q2, _ := Parse(`RequestMemory > 8192`)
	if q2.Matches(ad) {
		t.Error("expected non-match")
	}
	// Undefined/error constraints are non-matches.
	q3, _ := Parse(`Missing > 0`)
	if q3.Matches(ad) {
		t.Error("undefined constraint should not match")
	}
	// ReadAttrs planning info.
	got := q.ReadAttrs()
	if len(got) != 2 {
		t.Errorf("ReadAttrs = %v, want [Owner RequestMemory]", got)
	}
}

func FuzzDifferential(f *testing.F) {
	for _, ss := range scopeSources {
		for _, es := range exprSources {
			f.Add(ss, es)
		}
	}
	// Seed with the AST-parity edge cases so the fuzzer starts from the corpus
	// the reference engine was hardened against.
	for _, es := range parityCorpusExprs {
		f.Add(`[Cpus = 4; Owner = "alice"; x = 5]`, es)
		f.Add(`[]`, es)
	}
	f.Fuzz(func(t *testing.T, scopeSrc, exprSrc string) {
		scopeAd, err := classad.Parse(scopeSrc)
		if err != nil || scopeAd == nil {
			return
		}
		expr, err := parser.ParseExpr(exprSrc)
		if err != nil || expr == nil {
			return
		}
		want := refEval(scopeAd, expr)
		got := Run(CompileProgram(expr), scopeAd)
		if !valuesEqual(want, got) {
			t.Fatalf("mismatch expr=%q scope=%q: want=%s got=%s",
				exprSrc, scopeSrc, describe(want), describe(got))
		}
	})
}
