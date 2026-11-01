package vm

import (
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/parser"
)

// loopCorpus gives every record a DIFFERENT trip count, which is the whole point: the executor runs
// the trips in lockstep and the live set has to shrink one record at a time. A corpus where every
// record went round the same number of times would pass against an executor that never narrowed.
//
// It also carries the conditions that are neither true nor false -- a missing attribute, an explicit
// UNDEFINED, a string -- because a loop faults on those exactly as an If does, and a record that
// faults on trip three must not be revived on trip four.
func loopCorpus() *testSource {
	return newTestSource([]map[string]string{
		{"A": "0", "B": "1"},   // leaves before the first trip
		{"A": "1", "B": "2"},   // one trip
		{"A": "3", "B": "0"},   //
		{"A": "5", "B": "7"},   // the last one still going
		{"B": "1"},             // A missing: the condition is UNDEFINED, so this faults
		{"A": "undefined"},     //
		{"A": `"x"`, "B": "1"}, // a string: the condition is ERROR
		{"A": "2", "B": "4"},   //
	})
}

// evalWithDeadline runs a vector evaluation on another goroutine and fails the test if it does not
// return.
//
// The bugs this file is about do not produce wrong answers; they produce a program that never stops.
// A trip bound that does not bound, or a region pushed on every trip so that no trip is ever
// counted, both read as a hang — and a hang takes the whole package down with no indication of which
// test was stuck, which is the least useful report there is. A deadline turns each of them into a
// named failure.
func evalWithDeadline(t *testing.T, sp *ScriptProgram, src ColumnSource, n int, scratch *VecScratch) (*Vec, bool) {
	t.Helper()
	type result struct {
		v  *Vec
		ok bool
	}
	ch := make(chan result, 1)
	go func() {
		v, ok := sp.VecEval(src, n, scratch)
		ch <- result{v, ok}
	}()
	select {
	case r := <-ch:
		return r.v, r.ok
	case <-time.After(20 * time.Second):
		t.Fatal("the vector evaluation did not return: a loop is running with nothing bounding it")
		return nil, false
	}
}

// mustExprB is mustExpr for a benchmark.
func mustExprB(b *testing.B, src string) ast.Expr {
	b.Helper()
	e, err := parser.ParseExpr(src)
	if err != nil {
		b.Fatalf("parse %q: %v", src, err)
	}
	return e
}

type loopShape struct {
	name  string
	build func(t *testing.T, s *Script) *Script
}

func loopShapes() []loopShape {
	return []loopShape{
		// The plain counting loop: trips differ per record and the result is the count.
		{"count to A", func(t *testing.T, s *Script) *Script {
			return s.Let("x", mustExpr(t, "0")).
				While(mustExpr(t, "x < A"), func(b *Script) { b.Let("x", mustExpr(t, "x + 1")) }).
				Result(mustExpr(t, "x"))
		}},
		// A second local written in the body, so more than the counter has to survive the trips.
		{"accumulate", func(t *testing.T, s *Script) *Script {
			return s.Let("x", mustExpr(t, "0")).Let("acc", mustExpr(t, "0")).
				While(mustExpr(t, "x < A"), func(b *Script) {
					b.Let("acc", mustExpr(t, "acc + x"))
					b.Let("x", mustExpr(t, "x + 1"))
				}).
				Result(mustExpr(t, "acc"))
		}},
		// A loop nobody enters.
		{"never runs", func(t *testing.T, s *Script) *Script {
			return s.Let("x", mustExpr(t, "0")).
				While(mustExpr(t, "false"), func(b *Script) { b.Let("x", mustExpr(t, "99")) }).
				Result(mustExpr(t, "x"))
		}},
		// An If INSIDE the body, so a region opens and closes within every trip.
		{"branch in body", func(t *testing.T, s *Script) *Script {
			return s.Let("x", mustExpr(t, "0")).Let("acc", mustExpr(t, "0")).
				While(mustExpr(t, "x < A"), func(b *Script) {
					b.If(mustExpr(t, "B > 1"),
						func(c *Script) { c.Let("acc", mustExpr(t, "acc + 10")) },
						func(c *Script) { c.Let("acc", mustExpr(t, "acc + 1")) })
					b.Let("x", mustExpr(t, "x + 1"))
				}).
				Result(mustExpr(t, "acc"))
		}},
		// A branch in the body whose RESULT is the counter the loop advances. "branch in body" above
		// reads an accumulator the arms write, so a store that wrongly wrote every lane would corrupt
		// only the counter -- which nothing reads. Here the counter is the answer, which is what makes
		// a live set mishandled inside the body a wrong value rather than a wrong charge.
		{"branch then counter is the result", func(t *testing.T, s *Script) *Script {
			return s.Let("x", mustExpr(t, "0")).Let("acc", mustExpr(t, "0")).
				While(mustExpr(t, "x < A"), func(b *Script) {
					b.If(mustExpr(t, "B > 1"),
						func(c *Script) { c.Let("acc", mustExpr(t, "acc + 10")) },
						func(c *Script) { c.Let("acc", mustExpr(t, "acc + 1")) })
					b.Let("x", mustExpr(t, "x + 1"))
				}).
				Result(mustExpr(t, "x"))
		}},
		// A short-circuit in the CONDITION, so a path region opens and closes at the header.
		{"short-circuit condition", func(t *testing.T, s *Script) *Script {
			return s.Let("x", mustExpr(t, "0")).
				While(mustExpr(t, "B > 0 && x < A"), func(b *Script) { b.Let("x", mustExpr(t, "x + 1")) }).
				Result(mustExpr(t, "x"))
		}},
		// A loop AFTER a loop, so the live set has to be restored and narrowed again.
		{"two loops", func(t *testing.T, s *Script) *Script {
			s.Let("x", mustExpr(t, "0")).
				While(mustExpr(t, "x < A"), func(b *Script) { b.Let("x", mustExpr(t, "x + 1")) })
			return s.Let("y", mustExpr(t, "0")).
				While(mustExpr(t, "y < B"), func(b *Script) { b.Let("y", mustExpr(t, "y + 1")) }).
				Result(mustExpr(t, "x * 100 + y"))
		}},
		// A loop INSIDE a loop.
		{"nested loops", func(t *testing.T, s *Script) *Script {
			return s.Let("x", mustExpr(t, "0")).Let("acc", mustExpr(t, "0")).
				While(mustExpr(t, "x < A"), func(b *Script) {
					b.Let("y", mustExpr(t, "0"))
					b.While(mustExpr(t, "y < B"), func(c *Script) {
						c.Let("acc", mustExpr(t, "acc + 1"))
						c.Let("y", mustExpr(t, "y + 1"))
					})
					b.Let("x", mustExpr(t, "x + 1"))
				}).
				Result(mustExpr(t, "acc"))
		}},
		// A loop inside an If arm: the loop's outer live set is the arm's mask, not the batch.
		{"loop inside a branch", func(t *testing.T, s *Script) *Script {
			return s.Let("x", mustExpr(t, "0")).
				If(mustExpr(t, "B > 0"), func(c *Script) {
					c.While(mustExpr(t, "x < A"), func(b *Script) { b.Let("x", mustExpr(t, "x + 1")) })
				}, nil).
				Result(mustExpr(t, "x"))
		}},
	}
}

// TestVecLoopMatchesScalarPerRecord is the law, for loops.
func TestVecLoopMatchesScalarPerRecord(t *testing.T) {
	src := loopCorpus()
	n := len(src.recs)
	var scratch VecScratch
	for _, sh := range loopShapes() {
		sp, err := sh.build(t, NewScript()).Compile()
		if err != nil {
			t.Fatalf("%s: compile: %v", sh.name, err)
		}
		got, ok := evalWithDeadline(t, sp, src, n, &scratch)
		if !ok {
			t.Errorf("%s declined; a terminating loop is meant to be servable", sh.name)
			continue
		}
		for i := 0; i < n; i++ {
			want := sp.Run(src.ad(i))
			if !sameValue(got.value(i), want) {
				t.Errorf("%s record %d: vector %s (state %d) != scalar %s",
					sh.name, i, got.value(i), got.St[i], want)
			}
		}
	}
	t.Logf("%d loop shapes x %d records verified against the register executor", len(loopShapes()), n)
}

// TestVecLoopTripsDifferPerRecord is the guard against a vacuous law. Every shape above would agree
// with the scalar executor if the loop ran the same number of trips for everybody and the result
// happened to match -- so this pins that the corpus really does make records leave at different
// times, which is the thing the live mask exists to handle.
func TestVecLoopTripsDifferPerRecord(t *testing.T) {
	src := loopCorpus()
	n := len(src.recs)
	sp, err := NewScript().Let("x", mustExpr(t, "0")).
		While(mustExpr(t, "x < A"), func(b *Script) { b.Let("x", mustExpr(t, "x + 1")) }).
		Result(mustExpr(t, "x")).Compile()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := evalWithDeadline(t, sp, src, n, nil)
	if !ok {
		t.Fatal("declined")
	}
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		seen[got.value(i).String()] = true
	}
	if len(seen) < 4 {
		t.Errorf("only %d distinct results (%v); the records are not taking different numbers of trips",
			len(seen), seen)
	}
	// And the faults are reached, since a loop that never faults leaves that path untested.
	faults := 0
	for i := 0; i < n; i++ {
		if got.value(i).IsError() {
			faults++
		}
	}
	if faults == 0 {
		t.Error("no record faulted; the corpus is meant to contain conditions that are neither true nor false")
	}
	t.Logf("%d distinct trip counts, %d faults", len(seen), faults)
}

// TestVecLoopGivesUpWhenItStopsPayingOff pins the work guard.
//
// A trip costs a full-width pass whatever the live count is, because the kernels do not compact. So
// a loop where one record goes round thousands of times while the rest left on trip one is doing
// thousands of full-width passes for one record's benefit. The executor abandons the batch, and the
// caller runs it per record -- which is exact, so the only thing at stake is time.
//
// Without the guard this test does not fail, it HANGS: the trip bound alone is the per-record budget,
// and reaching it takes hundreds of thousands of full-width passes.
func TestVecLoopGivesUpWhenItStopsPayingOff(t *testing.T) {
	recs := make([]map[string]string, 512)
	for i := range recs {
		recs[i] = map[string]string{"A": "1"}
	}
	recs[0]["A"] = "1000000" // one record goes round a very long time
	src := newTestSource(recs)
	sp, err := NewScript().Let("x", mustExpr(t, "0")).
		While(mustExpr(t, "x < A"), func(b *Script) { b.Let("x", mustExpr(t, "x + 1")) }).
		Result(mustExpr(t, "x")).Compile()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() {
		_, ok := sp.VecEval(src, len(recs), nil)
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok {
			t.Error("the batch was served; one record taking a million trips should have abandoned it")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the loop did not give up: it is doing full-width passes for one record and the " +
			"only bound is the per-record budget")
	}
}

// TestVecLoopChargeMatchesScalarPerRecord extends the charge law to loops, which is where it has
// the most to say: a record that went round twice must be billed for two trips and a record that
// went round nine times for nine, out of the same lockstep execution.
//
// The scalar side is the REGISTER executor rather than the stack interpreter, because the stack
// interpreter has no implementation of a script's locals or branches — it runs expressions only.
// That the two forms charge identically is what makes either a valid oracle, and it is checked over
// the expression corpora by TestChargeIsTheSameInBothForms.
//
// Batches of one, for the same reason as the expression version: the executor keeps one total for
// the batch, so only a batch of one makes that total a single record's charge.
func TestVecLoopChargeMatchesScalarPerRecord(t *testing.T) {
	full := loopCorpus()
	var scratch VecScratch
	checked := 0
	for _, sh := range loopShapes() {
		sp, err := sh.build(t, NewScript()).Compile()
		if err != nil {
			t.Fatal(err)
		}
		for i := range full.recs {
			one := newTestSource(full.recs[i : i+1])
			if _, ok := evalWithDeadline(t, sp, one, 1, &scratch); !ok {
				continue
			}
			want := regSteps(sp.RegProgram(), full.ad(i))
			checked++
			if int64(want) != scratch.charge {
				t.Errorf("%s record %d: vector charged %d, scalar charged %d",
					sh.name, i, scratch.charge, want)
			}
		}
	}
	if checked == 0 {
		t.Fatal("nothing was checked")
	}
	t.Logf("%d record-shape pairs checked", checked)
}
