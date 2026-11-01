package vm

import (
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/parser"
)

// chargeShapes are the shapes the shared corpora do not contain, each added because a mutation
// survived without it. They are about WHERE a conditionally-executed region sits rather than about
// what it computes.
var chargeShapes = []string{
	// Work AFTER a short-circuit region. Without it, a region that failed to restore the live count
	// on the way out would never be noticed: in every corpus expression the short-circuit is the last
	// thing that happens, so the stale count is never used to bill anything.
	`(A > 4 && B < 4) + 1`,
	`(A > 4 || B < 4) == F`,
	`ifThenElse(A > 4, 1, 2) + B`,
	`(A ?: B) + 1`,
	// An Elvis whose LEFT OPERAND IS IN MASK FORM, which only happens when it comes from a logical
	// operator. It is the case that distinguishes "is undefined" from "is not true" -- an ERROR left
	// operand is perfectly well defined, so the Elvis takes it and the fallback is not needed.
	`(A && F) ?: 9`,
	`(A > 4 && B < 4) ?: 7`,
	`(A || F) ?: S`,
	// Regions nested inside regions, in both orders.
	`A > 4 && (B < 4 || A == 0)`,
	`(A ?: B) && (B ?: A)`,
	`ifThenElse(A > 4 && F, B ?: 1, B ?: 2)`,
}

// TestVecChargeMatchesScalarPerRecord is the point of the charge: a record must be billed for what
// it would have run scalar, whoever else is in its batch.
//
// Checked on batches of ONE record, which is what makes it exact rather than aggregate. The vector
// executor keeps a single total for the batch, so over a batch of many records the test could only
// compare sums -- and a sum can match while every lane is wrong. With one record the total IS that
// record's charge.
//
// The scalar side is the stack interpreter rather than the register executor, because the stack form
// is the same instruction stream the vector executor runs. (That the register form agrees too is
// TestChargeIsTheSameInBothForms, and it is what makes either a valid oracle.)
func TestVecChargeMatchesScalarPerRecord(t *testing.T) {
	full := vecCorpus()
	var scratch VecScratch
	checked, declined := 0, 0
	var worst string
	all := append(append([]string{}, vecExprs...), parityCorpusExprs...)
	all = append(all, chargeShapes...)
	for _, exprSrc := range all {
		expr, err := parser.ParseExpr(exprSrc)
		if err != nil || expr == nil {
			continue
		}
		q, err := Parse(exprSrc)
		if err != nil {
			continue
		}
		for i := range full.recs {
			one := newTestSource(full.recs[i : i+1])
			if _, ok := q.VecEval(one, 1, &scratch); !ok {
				declined++
				continue
			}
			want := stackSteps(q.prog, full.ad(i))
			checked++
			if int64(want) != scratch.charge {
				if worst == "" {
					worst = exprSrc
				}
				t.Errorf("%q record %d: vector charged %d, scalar charged %d",
					exprSrc, i, scratch.charge, want)
			}
		}
	}
	if checked == 0 {
		t.Fatal("nothing was checked")
	}
	t.Logf("%d record-expression pairs checked, %d declined", checked, declined)
}

// countingSource records which columns were asked for, so a test can assert that work was not merely
// made cheaper but NOT DONE.
type countingSource struct {
	*testSource
	loads map[string]int
}

func newCountingSource(recs []map[string]string) *countingSource {
	return &countingSource{testSource: newTestSource(recs), loads: map[string]int{}}
}

func (s *countingSource) LoadColumn(name string, scope ast.AttributeScope, dst *Vec) bool {
	s.loads[strings.ToLower(name)]++
	return s.testSource.LoadColumn(name, scope, dst)
}

// TestVecSkipAvoidsTheColumnLoad is the behavioural proof that a skipped path is not executed.
//
// A charge that comes out right could still be a path that ran and was not billed. This asks the
// question the optimization is actually about: a column the batch does not need must never be
// LOADED, because a load is a decompression and that is where the time goes.
//
// Both directions are asserted. A batch where no record needs the right operand must not touch it;
// a batch where one record does must touch it, or the test would pass against an executor that had
// simply stopped evaluating right operands.
func TestVecSkipAvoidsTheColumnLoad(t *testing.T) {
	for _, tc := range []struct {
		name  string
		expr  string
		recs  []map[string]string
		wantB int
	}{
		{"&& short-circuits away", "A > 100 && B > 0",
			[]map[string]string{{"A": "1", "B": "5"}, {"A": "2", "B": "6"}}, 0},
		{"&& needs it for one record", "A > 100 && B > 0",
			[]map[string]string{{"A": "1", "B": "5"}, {"A": "200", "B": "6"}}, 1},
		{"|| short-circuits away", "A > 0 || B > 0",
			[]map[string]string{{"A": "1", "B": "5"}, {"A": "2", "B": "6"}}, 0},
		{"elvis fallback unused", "A ?: B",
			[]map[string]string{{"A": "1", "B": "5"}, {"A": "2", "B": "6"}}, 0},
		{"elvis fallback needed", "A ?: B",
			[]map[string]string{{"A": "1", "B": "5"}, {"B": "6"}}, 1},
		{"conditional arm unused", "ifThenElse(A > 0, 7, B)",
			[]map[string]string{{"A": "1", "B": "5"}, {"A": "2", "B": "6"}}, 0},
		{"conditional arm needed", "ifThenElse(A > 0, 7, B)",
			[]map[string]string{{"A": "1", "B": "5"}, {"A": "-2", "B": "6"}}, 1},
	} {
		src := newCountingSource(tc.recs)
		q, err := Parse(tc.expr)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := q.VecEval(src, len(tc.recs), nil)
		if !ok {
			t.Errorf("%s: declined", tc.name)
			continue
		}
		if n := src.loads["b"]; n != tc.wantB {
			t.Errorf("%s: B was loaded %d times, want %d", tc.name, n, tc.wantB)
		}
		// And the answer is still right, because a skip that changed the result would be a far worse
		// bug than a skip that never happened.
		for i := range tc.recs {
			if want := q.Eval(src.ad(i)); !sameValue(got.value(i), want) {
				t.Errorf("%s record %d: vector %s != scalar %s", tc.name, i, got.value(i), want)
			}
		}
	}
}

// TestVecChargeOverABatchIsTheSumOfItsRecords is the complement of the per-record test above, and it
// exists because that test cannot see a whole class of bug.
//
// A one-record batch makes the charge exact per lane, but it also makes every live count either zero
// or one -- and a region whose live count is one is indistinguishable from the whole batch. So a
// region that failed to restore the count on the way out, or narrowed to the wrong number, is
// invisible there. Over a mixed batch the counts are genuinely intermediate and the total is the sum
// of what each record would have been charged alone.
//
// Neither test subsumes the other: this one compares sums, which can agree while every lane is
// wrong, and that one compares lanes but only in a batch where counts are trivial.
func TestVecChargeOverABatchIsTheSumOfItsRecords(t *testing.T) {
	full := vecCorpus()
	n := len(full.recs)
	var scratch VecScratch
	checked := 0
	for _, exprSrc := range append(append([]string{}, vecExprs...), chargeShapes...) {
		q, err := Parse(exprSrc)
		if err != nil {
			continue
		}
		if _, ok := q.VecEval(full, n, &scratch); !ok {
			continue
		}
		var want int64
		for i := 0; i < n; i++ {
			want += int64(stackSteps(q.prog, full.ad(i)))
		}
		checked++
		if scratch.charge != want {
			t.Errorf("%q: the batch was charged %d, its records charge %d alone",
				exprSrc, scratch.charge, want)
		}
	}
	if checked == 0 {
		t.Fatal("nothing was checked")
	}
	t.Logf("%d expressions checked over a batch of %d", checked, n)
}
