package db

import (
	"testing"

	"github.com/PelicanPlatform/classad/collections"
)

// TestArchiveAggregateTier is the contract the tier exists for: the SAME aggregate shape, over
// fixtures that differ only in whether the columnar accelerator is built, reports which strategy
// answered rather than leaving a caller to infer it from zeroed counters. The two archives here
// are the same 4000 records with the same segment sizing; the only difference is the accelerator,
// so a difference in the reported tier is a difference in behaviour, not in the fixture.
func TestArchiveAggregateTier(t *testing.T) {
	fast, _ := buildTopKArchive(t, 4000, true)  // columnarized: ClusterId is a covered numeric field
	slow, _ := buildTopKArchive(t, 4000, false) // not columnarized: every aggregate reassembles

	maxCid := []AggSpec{{Func: AggMax, Arg: "ClusterId"}}

	// Same aggregate, same data, two strategies. The columnar answer reads the ClusterId blocks;
	// the fall-through walks records. Before the tier these were told apart only by the scan
	// counts happening to be zero on the first.
	var cs collections.ScanStats
	if _, err := fast.AggregateColsStats("true", nil, maxCid, &cs); err != nil {
		t.Fatalf("columnar MAX: %v", err)
	}
	if cs.Tier != collections.TierColumnar {
		t.Errorf("columnar MAX reported tier %v, want %v (stats %+v)", cs.Tier, collections.TierColumnar, cs)
	}

	var ss collections.ScanStats
	if _, err := slow.AggregateColsStats("true", nil, maxCid, &ss); err != nil {
		t.Fatalf("fall-through MAX: %v", err)
	}
	if ss.Tier != collections.TierScan {
		t.Errorf("fall-through MAX reported tier %v, want %v (stats %+v)", ss.Tier, collections.TierScan, ss)
	}
	if ss.RecordsVisited == 0 {
		t.Errorf("a scan tier with no records visited: %+v", ss)
	}

	// An unconstrained COUNT(*) is answered from the archive's retained record count -- no
	// segment opened at all, which is a different kind of cheap from the columnar read above and
	// used to be indistinguishable from it.
	var is collections.ScanStats
	rows, err := slow.AggregateColsStats("true", nil, []AggSpec{{Func: AggCount, Arg: "*"}}, &is)
	if err != nil {
		t.Fatalf("COUNT(*): %v", err)
	}
	if len(rows) != 1 || rows[0].Values[0] != "4000" {
		t.Fatalf("COUNT(*) = %+v, want 4000", rows)
	}
	if is.Tier != collections.TierIndex {
		t.Errorf("unconstrained COUNT(*) reported tier %v, want %v (stats %+v)", is.Tier, collections.TierIndex, is)
	}

	// A constraint that folds to false reads nothing. It reports its own tier rather than the
	// all-zero trailer it shares with every fast path, which is the residual ambiguity the
	// inference could never resolve.
	var ns collections.ScanStats
	rows, err = fast.AggregateColsStats("false", nil, []AggSpec{{Func: AggCount, Arg: "*"}}, &ns)
	if err != nil {
		t.Fatalf("match-none COUNT(*): %v", err)
	}
	if len(rows) != 1 || rows[0].Values[0] != "0" {
		t.Fatalf("match-none COUNT(*) = %+v, want 0", rows)
	}
	if ns.Tier != collections.TierMatchNone {
		t.Errorf("match-none reported tier %v, want %v (stats %+v)", ns.Tier, collections.TierMatchNone, ns)
	}
	if ns.RecordsVisited != 0 {
		t.Errorf("match-none visited records: %+v", ns)
	}
}

// TestArchiveAggregateTierAgreesWithCounts checks the tier against the evidence in the rest of the
// trailer across a spread of shapes: a scan tier must have visited records, and any other tier
// must not have. A path tagged with the wrong tier -- or a new path left untagged -- shows up here
// even for shapes whose strategy this test does not pin down.
func TestArchiveAggregateTierAgreesWithCounts(t *testing.T) {
	a, _ := buildTopKArchive(t, 4000, true)

	for _, tc := range []struct {
		name       string
		constraint string
		groups     []GroupCol
		aggs       []AggSpec
	}{
		{"unconstrained count", "true", nil, []AggSpec{{Func: AggCount, Arg: "*"}}},
		{"constrained count", "ClusterId > 500000", nil, []AggSpec{{Func: AggCount, Arg: "*"}}},
		{"grouped count", "true", []GroupCol{{Attr: "Owner"}}, []AggSpec{{Func: AggCount, Arg: "*"}}},
		{"max numeric", "true", nil, []AggSpec{{Func: AggMax, Arg: "ClusterId"}}},
		{"constrained max", "JobStatus == 4", nil, []AggSpec{{Func: AggMax, Arg: "ClusterId"}}},
		{"sum", "true", nil, []AggSpec{{Func: AggSum, Arg: "ProcId"}}},
		{"grouped sum", "true", []GroupCol{{Attr: "Owner"}}, []AggSpec{{Func: AggSum, Arg: "ProcId"}}},
		{"match none", "false", nil, []AggSpec{{Func: AggCount, Arg: "*"}}},
	} {
		var st collections.ScanStats
		if _, err := a.AggregateColsStats(tc.constraint, tc.groups, tc.aggs, &st); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		switch {
		case st.Tier == collections.TierUnreported:
			t.Errorf("%s: no tier reported (stats %+v)", tc.name, st)
		case st.Tier == collections.TierScan && st.RecordsVisited == 0:
			t.Errorf("%s: tier %v but no records visited (stats %+v)", tc.name, st.Tier, st)
		case st.Tier != collections.TierScan && st.RecordsVisited != 0:
			t.Errorf("%s: tier %v but %d records visited (stats %+v)",
				tc.name, st.Tier, st.RecordsVisited, st)
		}
		t.Logf("%s: tier %v, %+v", tc.name, st.Tier, st)
	}
}

// A nil stats argument must still work: the tagging is nil-safe like the counters, so a caller
// that wants no breakdown pays nothing and crashes nowhere.
func TestArchiveAggregateTierNilStats(t *testing.T) {
	a, _ := buildTopKArchive(t, 200, true)
	for _, c := range []string{"true", "false", "ClusterId > 10"} {
		if _, err := a.AggregateColsStats(c, nil, []AggSpec{{Func: AggCount, Arg: "*"}}, nil); err != nil {
			t.Fatalf("%s: %v", c, err)
		}
	}
}
