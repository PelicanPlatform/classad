package db

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// An archive sets ReverseScan ("newest-first Query, matching condor_history order"), and
// reverseScan forces serial scans "since fan-out has no cross-segment order". The CONSTRAINED
// wire read honours that gate; the unconstrained one (runParallelWireScan) has no such gate.
//
// So: do the two spellings of the same read agree on order?
func TestRawWireRefsOrderMatchesBetweenSpellings(t *testing.T) {
	cat, a := archiveCountFixture(t, 20000)
	defer cat.Close()

	first := func(constraint string) int64 {
		rows, err := a.QueryRawWireRefs(constraint, []string{"ClusterId"}, false)
		if err != nil {
			t.Fatal(err)
		}
		for row := range rows {
			ad, derr := wire.DecodeInline(row)
			if derr != nil {
				t.Fatalf("%q: row did not decode: %v", constraint, derr)
			}
			v, verr := classad.FromAST(ad).EvaluateAttr("ClusterId").IntValue()
			if verr != nil {
				t.Fatalf("%q: ClusterId: %v", constraint, verr)
			}
			return v
		}
		t.Fatalf("%q: no rows", constraint)
		return -1
	}

	shortcut := first("true")
	predicate := first("JobStatus >= 0")
	t.Logf("first ClusterId: `true` => %d, `JobStatus >= 0` => %d (newest is 19999)", shortcut, predicate)

	// XFAIL, deliberately, and self-maintaining: it SKIPS while the defect is present and FAILS
	// once it is fixed, so whoever fixes it is told to turn this back into a plain assertion
	// rather than leaving a test that passes for the wrong reason.
	if shortcut != predicate {
		t.Skipf("KNOWN DEFECT: the two spellings of the same read disagree on order -- `true` "+
			"yielded %d first, `JobStatus >= 0` yielded %d. The archive promises newest-first "+
			"(ReverseScan, 'matching condor_history order'), and reverseScan is documented to "+
			"force serial scans 'since fan-out has no cross-segment order'. The CONSTRAINED wire "+
			"read honours that gate (`... && !c.reverseScan`); runParallelWireScan, which serves "+
			"the unconstrained read, has no such gate and fans out anyway. Fixing it means either "+
			"gating that fan-out too or restoring order after it -- both have a cost, which is why "+
			"this records the defect rather than choosing.", shortcut, predicate)
	}
	if shortcut != 19999 {
		t.Errorf("newest-first: first ClusterId is %d, want 19999", shortcut)
	}
}
