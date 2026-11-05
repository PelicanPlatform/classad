package db

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// An archive is newest-first (ReverseScan, "matching condor_history order"). Both spellings of the
// same read must say so, and for a while neither reliably did: reverseScan was documented to force
// serial scans "since fan-out has no cross-segment order", so the CONSTRAINED read gave up its
// fan-out to keep the order, while the unconstrained one carried no such gate and gave up the
// ORDER to keep the fan-out -- silently, and only when the worker budget happened to be free. On a
// 20k archive `true` yielded ClusterId 1376 first where `JobStatus >= 0` yielded 19999.
//
// Both now go through the ordered fan-out (collections.runWireFanout), which recovers the order
// from the segments rather than from the records: the segments are dealt to the workers newest
// first and the consumer drains them in that order, so nothing is compared and nothing is sorted.
// This was an XFAIL that skipped while the defect stood. It is an assertion.
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

	if shortcut != predicate {
		t.Errorf("the two spellings of the same read disagree on order: `true` yielded %d first, "+
			"`JobStatus >= 0` yielded %d", shortcut, predicate)
	}
	if shortcut != 19999 {
		t.Errorf("newest-first: first ClusterId is %d, want 19999", shortcut)
	}
}
