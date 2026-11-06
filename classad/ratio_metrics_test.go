//go:build classadmetrics

package classad

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/parser"
)

// Measurement of the construction-to-comparison ratio over the committed pool
// corpus, which is the number the predicate fast path exists to move.
//
// Run with:
//
//	go test ./classad/ -tags classadmetrics -run TestConstructionRatio -v
//
// The two arms are the SAME expression evaluated two ways in one binary: as written,
// where the fast path may fire, and armored (predicate_diff_test.go), where every
// comparison operand is wrapped in parentheses so it provably cannot. The armored arm
// is the before: a ParenExpr constructs no Value of its own and changes no comparison,
// so its counts are what this corpus produced before the fast path existed.
// Measuring both in one process rather than against a second checkout removes the
// usual way this measurement goes wrong -- two builds, two corpora, two
// instrumentations that do not count the same events.

type ratioCounts struct {
	constructions int64
	comparisons   int64
	fast          int64
}

func (c ratioCounts) ratio() float64 {
	if c.comparisons == 0 {
		return 0
	}
	return float64(c.constructions) / float64(c.comparisons)
}

// measure runs fn with the counters reset and returns what it accumulated.
func measure(fn func()) ratioCounts {
	ResetEvalMetrics()
	fn()
	cons, cmps, fast := EvalMetrics()
	return ratioCounts{constructions: cons, comparisons: cmps, fast: fast}
}

func report(t *testing.T, name string, before, after ratioCounts) {
	t.Helper()
	t.Logf("%s", name)
	t.Logf("    before: %8d constructions / %7d comparisons = %.2f per comparison",
		before.constructions, before.comparisons, before.ratio())
	t.Logf("    after:  %8d constructions / %7d comparisons = %.2f per comparison",
		after.constructions, after.comparisons, after.ratio())
	if before.comparisons != after.comparisons {
		t.Errorf("    the two arms decided different numbers of comparisons (%d vs %d); they are not the same work",
			before.comparisons, after.comparisons)
	}
	if after.comparisons > 0 {
		t.Logf("    fast path decided %d of %d comparisons (%.1f%%); %d declined (%.1f%%)",
			after.fast, after.comparisons, 100*float64(after.fast)/float64(after.comparisons),
			after.comparisons-after.fast, 100*float64(after.comparisons-after.fast)/float64(after.comparisons))
	}
	if before.constructions > 0 {
		t.Logf("    Value constructions removed: %d of %d (%.1f%%)",
			before.constructions-after.constructions, before.constructions,
			100*float64(before.constructions-after.constructions)/float64(before.constructions))
	}
}

// TestConstructionRatio measures the ratio for the two constraint shapes real
// matchmaking evaluates: the JOB's Requirements against each machine ad in the pool
// corpus, and each MACHINE's own Requirements against the job.
//
// The two are not alike and the difference is the headline caveat for this work. A
// job's Requirements is written against literals (`TARGET.Arch == "X86_64"`,
// `TARGET.Memory >= RequestMemory`), which is exactly the shape the fast path claims.
// A real OSPool machine's Requirements is `Start && WithinResourceLimits`, two
// references to other attributes whose own expressions are large compositions -- a
// shape with almost no `attribute OP literal` comparisons at the top at all, and
// whose comparisons deeper down are mostly between two computed subexpressions.
func TestConstructionRatio(t *testing.T) {
	slots := poolSlots(t)
	if len(slots) == 0 {
		t.Skip("pool corpus unavailable")
	}
	t.Logf("corpus: %d machine ads with a Requirements expression", len(slots))

	job, err := Parse(benchJobAd)
	if err != nil {
		t.Fatal(err)
	}
	jobReq, ok := job.Lookup("Requirements")
	if !ok {
		t.Fatal("benchJobAd has no Requirements")
	}

	runJobSide := func(expr ast.Expr) func() {
		return func() {
			for _, slot := range slots {
				_ = job.EvaluateExprAgainst(expr, slot)
			}
		}
	}
	report(t, "job Requirements against every machine (the matchmaking read path)",
		measure(runJobSide(armor(jobReq.expr))),
		measure(runJobSide(jobReq.expr)))

	// Machine-side START policy. Each machine's own Requirements is evaluated with
	// the job as TARGET, which is what the negotiator does from the other side.
	//
	// Armoring the EXPRESSION is not enough here, and discovering that is the first
	// real result of this measurement. A real OSPool machine's Requirements is
	// `Start && WithinResourceLimits` -- two bare references -- so the comparisons it
	// decides are not in the expression at all; they are in the expressions those
	// attributes are bound to, inside the ad. An expression-level armor left both arms
	// identical while 81% of the comparisons took the fast path in BOTH. The control
	// arm has to armor the ADS too.
	machineReqs := make([]ast.Expr, 0, len(slots))
	for _, slot := range slots {
		if r, ok := slot.Lookup("Requirements"); ok {
			machineReqs = append(machineReqs, r.expr)
		}
	}
	armoredSlots := make([]*ClassAd, len(slots))
	for i, slot := range slots {
		armoredSlots[i] = armoredAd(t, slot)
	}
	armoredJob := armoredAd(t, job)
	armoredReqs := make([]ast.Expr, len(machineReqs))
	for i, e := range machineReqs {
		armoredReqs[i] = armor(e)
	}
	// The control arm must be the same work, not merely similar work. FromAST
	// collapses duplicate attribute names and canonicalizes order, so an armored copy
	// is not guaranteed to be a twin of the ad it came from; checking that both arms
	// produce identical values for every machine is what makes the counts comparable.
	for i, slot := range slots {
		got := slot.EvaluateExprAgainst(machineReqs[i], job)
		want := armoredSlots[i].EvaluateExprAgainst(armoredReqs[i], armoredJob)
		if !valuesIdentical(got, want) {
			t.Fatalf("armored machine ad %d is not a twin: %s vs %s", i, describeValue(got), describeValue(want))
		}
	}

	report(t, "machine Requirements against the job (the START-policy read path)",
		measure(func() {
			for i, slot := range armoredSlots {
				_ = slot.EvaluateExprAgainst(armoredReqs[i], armoredJob)
			}
		}),
		measure(func() {
			for i, slot := range slots {
				_ = slot.EvaluateExprAgainst(machineReqs[i], job)
			}
		}))

	// A constraint of the shape a query tool sends -- condor_q / condor_status
	// -constraint, which is overwhelmingly `attribute OP literal` conjunctions,
	// because that is what a person types and what a query planner emits.
	q, err := parser.ParseExpr(`Cpus >= 1 && Memory > 1024 && (Arch == "X86_64" || Arch == "x86_64") && State != "Owner"`)
	if err != nil {
		t.Fatal(err)
	}
	runQuery := func(expr ast.Expr) func() {
		return func() {
			for _, slot := range slots {
				_ = NewEvaluator(slot).Evaluate(expr)
			}
		}
	}
	report(t, "a condor_status -constraint over every machine ad",
		measure(runQuery(armor(q))),
		measure(runQuery(q)))
}

// armoredAd returns a copy of ad whose every attribute expression is armored, so the
// predicate fast path cannot fire anywhere reachable from it -- including through an
// attribute reference, which an expression-level armor cannot reach.
//
// Parentheses around a comparison operand change no value, and armor leaves a bare
// literal alone, so evalAttrExpr's own literal fast path still applies to the copy
// exactly as it does to the original: the only thing suppressed is this work.
func armoredAd(t *testing.T, ad *ClassAd) *ClassAd {
	t.Helper()
	src := ad.AST()
	if src == nil {
		return ad
	}
	attrs := make([]*ast.AttributeAssignment, 0, len(src.Attributes))
	for _, a := range src.Attributes {
		attrs = append(attrs, &ast.AttributeAssignment{Name: a.Name, Value: armor(a.Value)})
	}
	out := FromAST(&ast.ClassAd{Attributes: attrs})
	if len(out.AST().Attributes) != len(src.Attributes) {
		t.Fatalf("armored ad lost attributes: %d of %d", len(out.AST().Attributes), len(src.Attributes))
	}
	return out
}
