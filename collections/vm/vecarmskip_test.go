package vm

import (
	"testing"
)

// armScript builds `if <cond>: u = <thenRead>  else: v = <elseRead>` and returns it compiled, with
// a nil arm where the read is empty.
func armScript(t *testing.T, cond, thenRead, elseRead string) *ScriptProgram {
	t.Helper()
	s := NewScript().Let("u", mustExpr(t, "0")).Let("v", mustExpr(t, "0"))
	var thenB, elseB func(*Script)
	if thenRead != "" {
		thenB = func(s *Script) { s.Let("u", mustExpr(t, thenRead)) }
	}
	if elseRead != "" {
		elseB = func(s *Script) { s.Let("v", mustExpr(t, elseRead)) }
	}
	sp, err := s.If(mustExpr(t, cond), thenB, elseB).Result(mustExpr(t, "u + v")).Compile()
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

// TestVecArmSkipAvoidsTheColumnLoad is the statement counterpart to TestVecSkipAvoidsTheColumnLoad.
//
// The operators already skip a path no record takes, so `A > 100 && B > 0` never touches B over a
// batch where nothing passes A. An `if` STATEMENT did not: it narrowed the live mask to an empty set
// and then ran the arm anyway, loading every column the body reads for nobody. A column load is the
// expensive thing an arm can contain, so an arm no record enters should cost its condition and
// nothing else.
//
// Both directions are asserted for each arm. A test that only checked the empty case would pass
// against an executor that had simply stopped running arms at all.
func TestVecArmSkipAvoidsTheColumnLoad(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cond         string
		recs         []map[string]string
		wantB, wantC int
	}{
		// Nothing passes, so the THEN arm is for nobody and B must not be read. The else arm runs,
		// so C must be.
		{"then arm empty", "A > 100",
			[]map[string]string{{"A": "1", "B": "5", "C": "9"}, {"A": "2", "B": "6", "C": "9"}}, 0, 1},
		// One record passes, so the then arm is needed and B must be read.
		{"then arm needed", "A > 100",
			[]map[string]string{{"A": "1", "B": "5", "C": "9"}, {"A": "200", "B": "6", "C": "9"}}, 1, 1},
		// EVERYTHING passes, so the ELSE arm is for nobody and C must not be read.
		{"else arm empty", "A > 0",
			[]map[string]string{{"A": "1", "B": "5", "C": "9"}, {"A": "2", "B": "6", "C": "9"}}, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := newCountingSource(tc.recs)
			sp := armScript(t, tc.cond, "B", "C")
			var scratch VecScratch
			if _, ok := sp.VecEval(src, len(tc.recs), &scratch); !ok {
				t.Fatal("the vector executor declined this script")
			}
			if got := src.loads["b"]; got != tc.wantB {
				t.Errorf("loaded B %d time(s), want %d (loads: %v)", got, tc.wantB, src.loads)
			}
			if got := src.loads["c"]; got != tc.wantC {
				t.Errorf("loaded C %d time(s), want %d (loads: %v)", got, tc.wantC, src.loads)
			}
			// The condition is always read; without this the "empty" cases would also pass against
			// an executor that loaded nothing at all.
			if src.loads["a"] == 0 {
				t.Errorf("the condition's own column was never loaded (loads: %v)", src.loads)
			}
		})
	}
}

// TestVecBothArmsEmptySkipsToTheJoin covers the shape where the condition is neither true nor false
// for any record: both arms are empty, so the region opens and closes having run neither, and the
// records it faulted must still be the ones excluded at the join.
func TestVecBothArmsEmptySkipsToTheJoin(t *testing.T) {
	// A is absent, so `A > 4` is UNDEFINED for every record and every record faults on the branch.
	recs := []map[string]string{{"B": "5", "C": "9"}, {"B": "6", "C": "9"}}
	src := newCountingSource(recs)
	sp := armScript(t, "A > 4", "B", "C")
	var scratch VecScratch
	if _, ok := sp.VecEval(src, len(recs), &scratch); !ok {
		t.Fatal("the vector executor declined this script")
	}
	if src.loads["b"] != 0 || src.loads["c"] != 0 {
		t.Errorf("an arm no record takes read a column: %v", src.loads)
	}
}
