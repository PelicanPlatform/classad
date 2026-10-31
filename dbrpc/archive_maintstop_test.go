package dbrpc

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// One archive maintenance "pass" is seven independent steps -- merge, reindex, index backfill,
// group derivation, schema scan, hot-set refresh, index auto-tune. Some honour StopMaintenance
// internally and some cannot, because they are single indivisible operations, and the ones that
// cannot were the problem: a stop landing in the first step still paid for every later one. On
// a history-scale archive the schema scan's columnarize alone is a 64-segment rewrite budget,
// which is the gap an operator experienced as a shutdown that would not finish.
//
// The hot-set refresh is the step this pins, precisely BECAUSE it has no internal stop check:
// it is a sample and a pointer swap, with no boundary of its own. The orchestrator's gate is
// therefore the only thing that can skip it, so this test measures the gate and not something
// further down. (The merge step is a poor probe for the opposite reason: mergePass's own
// between-merges check already stops it, so gating it in the orchestrator is belt and braces
// that no test could distinguish.)
//
// The running arm is what keeps this honest: it asserts the pass really does change the hot set
// on this fixture, so the stopped arm's unchanged hot set means the stop was respected rather
// than that there was nothing to do.
func TestArchiveMaintenanceStopsBetweenSteps(t *testing.T) {
	// Merge, reindex and backfill out of the way: this is about the hot-set refresh alone.
	opts := db.MaintainOptions{
		SampleMax:                 2000,
		HotTopN:                   8,
		ArchiveMerge:              db.MergeOptions{TriggerSegments: 1 << 20, TargetSegments: 1 << 19},
		ArchiveIndexBackfillBytes: -1,
	}

	pass := func(stop bool) (before, after []string) {
		t.Helper()
		cat, err := db.OpenCatalog(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer cat.Close() //nolint:errcheck
		a, err := cat.CreateArchiveTable("history", db.ArchiveConfig{SegmentSize: 1 << 12})
		if err != nil {
			t.Fatal(err)
		}
		for i := range 2000 {
			ad, perr := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nEnteredHistoryTime = %d\nPad = %q\n",
				i, 1000+i, strings.Repeat("p", 24)))
			if perr != nil {
				t.Fatal(perr)
			}
			if perr := a.Append(ad); perr != nil {
				t.Fatal(perr)
			}
		}
		if err := a.Flush(); err != nil {
			t.Fatal(err)
		}
		// A fresh archive starts on the built-in default hot set; the refresh replaces it with
		// the attributes this fixture's records actually carry, so the change is observable.
		before = a.HotAttrs()
		if stop {
			a.StopMaintenance()
		}
		srv := NewServerCatalog(cat)
		srv.maintainArchives(opts, nil)
		return before, a.HotAttrs()
	}

	runBefore, runAfter := pass(false)
	if slices.Equal(runBefore, runAfter) {
		t.Fatalf("the running pass left the hot set at %v: it is not refreshing on this fixture, so the stopped arm would prove nothing", runAfter)
	}
	stopBefore, stopAfter := pass(true)
	if !slices.Equal(stopBefore, stopAfter) {
		t.Errorf("a stopped pass still refreshed the hot set: %v -> %v (the running pass goes %v -> %v)",
			stopBefore, stopAfter, runBefore, runAfter)
	}
}
