package vm

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// Resolving an attribute reference must not allocate per evaluation.
//
// ResolveRef builds a fresh *ast.AttributeReference and folds the name with strings.ToLower on every
// call, and the interpreter called it once per reference per RECORD -- so a scan over 20k records
// paid 20k allocations for a name that was known at compile time. That defeated the precomputed-fold
// optimization resolveAttributeReference documents, and for a custom resolver the fold was pure
// waste, since the resolver receives the original-cased name.
//
// The program now carries one node per reference, built during compilation. This test fails if that
// regresses, which a timing benchmark would not do reliably.
func TestLoadRefDoesNotAllocatePerEval(t *testing.T) {
	q, err := Parse("ProcId >= 5 && ClusterId != ProcId")
	if err != nil {
		t.Fatal(err)
	}
	if !q.Native() {
		t.Fatal("fixture query is not native; it would not use OpLoadRef")
	}
	m := q.Matcher()
	resolver := func(name string, scope ast.AttributeScope) classad.Value {
		return classad.NewIntValue(7)
	}
	// Warm up, so first-call lazy work is not counted.
	for i := 0; i < 100; i++ {
		m.EvalResolved(resolver)
	}
	const n = 2000
	allocs := testing.AllocsPerRun(n, func() {
		m.EvalResolved(resolver)
	})
	// Two references per evaluation; a per-reference node build would show up as >= 2.
	if allocs > 0.5 {
		t.Errorf("EvalResolved allocates %.2f objects per evaluation over a 2-reference query; "+
			"the reference node should be built once at compile time, not per evaluation", allocs)
	}
	t.Logf("%.2f allocs per evaluation (2 references)", allocs)
}
