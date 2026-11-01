package db

import (
	"fmt"
	"testing"
)

// TestMatchFoldRequiresStaticReads pins the condition that makes the match-all and match-none folds
// sound, and the one that was missing.
//
// Both folds decide a constraint against an EMPTY ad and conclude that the answer is the same for
// every record. That is valid only when the constraint reads nothing -- and "reads nothing" was
// checked by looking for attribute references in the SYNTAX. eval() parses its argument at run time,
// so `eval("JobStatus") == 1` has no reference to find and still reads an attribute of every record.
//
// Both directions were wrong, and both are silent: a wrong fold answers from no data at all.
//
//	eval("JobStatus") == 1    folded to match-NONE, so COUNT(*) was 0 where a scan found 400
//	eval("JobStatus") =!= 2   folds to TRUE against an empty ad, so it folded to match-ALL,
//	                          though it is FALSE for every record whose JobStatus is 2
func TestMatchFoldRequiresStaticReads(t *testing.T) {
	for _, c := range []string{
		`eval("JobStatus") == 1`,
		`eval("JobStatus") =!= 2`,
		`eval("JobStatus")`,
		`eval("1") == 1`, // sound to fold, but refused: see the note below
		`eval("x") && JobStatus == 1`,
	} {
		if IsMatchAll(c) {
			t.Errorf("IsMatchAll(%s) = true; its read set is not statically visible", c)
		}
		if IsMatchNone(c) {
			t.Errorf("IsMatchNone(%s) = true; its read set is not statically visible", c)
		}
	}

	// `eval("1") == 1` above really is match-all, so refusing it loses a fold. That is the right
	// trade in this direction: refusing a sound fold costs a scan, while accepting an unsound one
	// answers wrongly from no data. The predicate is "could this read something we cannot see",
	// not "does it".

	// What must still fold, or the fix has thrown away what the folds are for.
	for _, c := range []string{``, `true`, `TRUE`, `1 == 1`, `true && true`} {
		if !IsMatchAll(c) {
			t.Errorf("IsMatchAll(%q) = false; a constant tautology must still fold", c)
		}
	}
	for _, c := range []string{`false`, `1 == 2`, `undefined`, `error`, `false && true`} {
		if !IsMatchNone(c) {
			t.Errorf("IsMatchNone(%q) = false; a constant contradiction must still fold", c)
		}
	}
	// And a record-dependent constraint folds neither way, which is the check that was already
	// there and must not regress.
	for _, c := range []string{`JobStatus == 1`, `JobStatus =!= 2`, `Owner == "x"`} {
		if IsMatchAll(c) || IsMatchNone(c) {
			t.Errorf("%s folded; it depends on the record", c)
		}
	}
}

// TestEvalConstraintCountsCorrectly is the end-to-end consequence: the aggregate ladder must not
// answer an eval() constraint from a fold. It is the case that exposed the bug -- COUNT(*) returned
// 0 where the row scan found 400 -- and it is checked against the scan rather than against a
// constant, so the test says "these agree" rather than hard-coding what either should be.
func TestEvalConstraintCountsCorrectly(t *testing.T) {
	cat, a := archiveCountFixture(t, 2000)
	defer cat.Close()
	for _, c := range []string{
		`eval("JobStatus") == 1`,
		`eval("JobStatus") =!= 2`,
		`eval("JobStatus") == 1 || JobStatus == 4`,
	} {
		rows, err := a.AggregateCols(c, nil, []AggSpec{{Func: AggCount, Arg: "*"}})
		if err != nil {
			t.Fatalf("%s: %v", c, err)
		}
		var got int
		if _, err := fmt.Sscanf(rows[0].Values[0], "%d", &got); err != nil {
			t.Fatalf("%s: unparsable count %q", c, rows[0].Values[0])
		}
		if want := scanCount(t, a, c); got != want {
			t.Errorf("%s: COUNT(*) = %d, scan = %d", c, got, want)
		}
	}
}
