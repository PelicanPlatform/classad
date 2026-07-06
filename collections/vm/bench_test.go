package vm

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// benchConstraint is a representative machine-matching constraint: attribute
// references, comparisons, and short-circuiting && / || — the native-compiled
// hot path.
const benchConstraint = `Cpus >= 2 && Memory > 4096 && Arch == "X86_64" && (State == "Unclaimed" || State == "Idle") && Rank > 0`

var benchScope = `[Cpus = 8; Memory = 16384; Arch = "X86_64"; State = "Unclaimed"; Rank = Cpus * 10]`

// Benchmark strategy. The VM is a compile-once / evaluate-many design: a query
// is compiled a single time and then run against every ad in a scan (potentially
// millions). So the meaningful comparisons are:
//
//   - BenchmarkEvalVM      : evaluation only (compile happens before ResetTimer)
//   - BenchmarkEvalTreeWalk: the tree-walk baseline, per evaluation
//   - BenchmarkCompile     : the one-time compile cost, amortized over a scan
//   - BenchmarkCompileEvalVM: compile + a single evaluation, the worst case where
//     a query is used exactly once (no amortization)
//
// Net cost over N ads: VM = compile + N*evalVM ; TreeWalk = N*evalTreeWalk.
// The VM wins only once compile is amortized AND evalVM < evalTreeWalk. On a
// *classad.ClassAd scope evalVM ~= evalTreeWalk (attribute resolution and value
// ops are shared); the decisive evaluation speedup comes in M3, where LOAD_ATTR
// resolves interned ids directly against the dense encoded ad instead of the
// string-keyed attribute map + per-reference child evaluator.

// BenchmarkEvalVM measures the compiled interpreter (compile once, run many).
func BenchmarkEvalVM(b *testing.B) {
	ad, err := classad.Parse(benchScope)
	if err != nil {
		b.Fatal(err)
	}
	q, err := Parse(benchConstraint)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !q.Matches(ad) {
			b.Fatal("expected match")
		}
	}
}

// BenchmarkEvalTreeWalk measures the reference tree-walking evaluator on the same
// expression and ad, as the baseline the VM must beat.
func BenchmarkEvalTreeWalk(b *testing.B) {
	ad, err := classad.Parse(benchScope)
	if err != nil {
		b.Fatal(err)
	}
	expr, err := parser.ParseExpr(benchConstraint)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v := classad.NewEvaluator(ad).Evaluate(expr)
		if bv, err := v.BoolValue(); err != nil || !bv {
			b.Fatal("expected true")
		}
	}
}

// BenchmarkCompileEvalVM measures compile + one evaluation: the worst case for
// the VM (a query used exactly once, so compilation is not amortized).
func BenchmarkCompileEvalVM(b *testing.B) {
	ad, err := classad.Parse(benchScope)
	if err != nil {
		b.Fatal(err)
	}
	expr, err := parser.ParseExpr(benchConstraint)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !Run(CompileProgram(expr), ad).IsBool() {
			b.Fatal("expected bool")
		}
	}
}

// BenchmarkCompile measures one-time compilation cost (amortized across a scan).
func BenchmarkCompile(b *testing.B) {
	expr, err := parser.ParseExpr(benchConstraint)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = CompileProgram(expr)
	}
}
