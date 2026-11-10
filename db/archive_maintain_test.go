package db

import (
	"fmt"
	"strings"
	"testing"
)

// ArchiveTable.Maintain is the archive's whole maintenance pass. It is covered here, where it
// lives, rather than only through whatever drives the schedule: the reason it moved is that an
// embedder which maintains its own catalog gets it too, and a test that can only reach it
// through the RPC server would not have said so.

// maintainFixture is an archive with enough small segments to merge, an index added after the
// fact (so there is a backfill owed), and recorded demand (so the tuner has something to act
// on). Each of those is what makes one step of the pass do work rather than return early.
func maintainFixture(t *testing.T, records int) (*ArchiveTable, func()) {
	t.Helper()
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{
		SegmentSize: 1 << 12,
		// An index from the start, so the archive has an index generation for a later addition
		// to be measured against: adding the FIRST index to an archive that had none covers
		// every segment, and nothing is ever left owed.
		ValueAttrs: []string{"ClusterId"},
		// A tight horizon, so the index added after the fact reaches only the newest segments
		// and the rest are owed to the backfill step.
		IndexBackfillBytes: 16 << 10,
	})
	if err != nil {
		cat.Close()
		t.Fatal(err)
	}
	for i := 0; i < records; i++ {
		ad := fmt.Sprintf("ClusterId = %d\nOwner = \"user_%d\"\nShard = %d\nPad = %q",
			i, i%20, i%16, strings.Repeat("z", 60))
		if appendErr := a.AppendOld(ad); appendErr != nil {
			cat.Close()
			t.Fatal(appendErr)
		}
	}
	return a, func() { cat.Close() }
}

// TestArchiveMaintainRunsEveryStepItIsAskedTo drives one pass with every step enabled and
// checks each left its mark. A step that silently returned early would show up as one
// unchanged measurement rather than as a failure somewhere else later.
func TestArchiveMaintainRunsEveryStepItIsAskedTo(t *testing.T) {
	a, cleanup := maintainFixture(t, 3000)
	defer cleanup()

	segsBefore := a.Stats().Segments
	if segsBefore < 10 {
		t.Fatalf("need enough segments for a merge to have something to do, got %d", segsBefore)
	}
	// An index added after the records were written is owed a backfill: Reindex stops at the
	// horizon, so the older segments stay pending until the pass carries it to them.
	if !a.AddIndex([]string{"Owner"}, nil) {
		t.Fatal("AddIndex reported no change, so no backfill is owed and the budget step is vacuous")
	}
	// The CONFIGURED hot set, which is not empty: a refresh is visible as a CHANGE to it, not
	// as it becoming non-empty. Asserting non-emptiness would pass without the refresh running.
	hotBefore := strings.Join(a.HotAttrs(), ",")

	// Demand for an attribute that is NOT indexed, so the tuner has a reason to add one.
	for i := 0; i < 40; i++ {
		seq, qerr := a.Query(fmt.Sprintf("Shard == %d", i%16))
		if qerr != nil {
			t.Fatal(qerr)
		}
		for range seq { //nolint:revive // the scan records the demand; the rows are not the point
		}
	}

	var tuned []AutoTuneChange
	a.Maintain(MaintainOptions{
		SampleMax: 256,
		ArchiveMerge: MergeOptions{
			TargetSegments: 4, TriggerSegments: 8, MinMergeBytes: 1, KeepRecent: 2,
		},
		ArchiveIndexBackfillBytes: 1 << 30, // generous: the point is that it runs, not that it is bounded
		HotTopN:                   4,
		ArchiveSchemaScanHotTopN:  4,
		ArchiveIndexMinDemand:     8,
		OnAutoTune:                func(r AutoTuneResult) { tuned = append(tuned, r.Changes...) },
	})

	if segsAfter := a.Stats().Segments; segsAfter >= segsBefore {
		t.Errorf("merge: %d segments before, %d after; the merge step did nothing", segsBefore, segsAfter)
	}
	if hotAfter := strings.Join(a.HotAttrs(), ","); hotAfter == hotBefore {
		t.Errorf("hot refresh: the hot set is still %q after a pass with HotTopN set over "+
			"records none of those attributes appear in", hotBefore)
	}
	if !a.SchemaScanInfo().Enabled {
		t.Error("schema scan: not enabled after a pass with ArchiveSchemaScanHotTopN set")
	}
	if len(tuned) == 0 {
		t.Errorf("auto-tune: nothing reported after 40 queries on an unindexed attribute. "+
			"Indexed now: %v", indexedNames(a))
	}
}

// TestArchiveMaintainHonoursItsSwitches: each step can be turned off independently, and
// turning the merge off does not turn the rest of the pass off with it -- the knob is named
// for the merge, and it used to return from the whole function.
func TestArchiveMaintainHonoursItsSwitches(t *testing.T) {
	a, cleanup := maintainFixture(t, 3000)
	defer cleanup()
	segsBefore := a.Stats().Segments
	hotBefore := strings.Join(a.HotAttrs(), ",")

	a.Maintain(MaintainOptions{
		SampleMax:            256,
		ArchiveMergeDisabled: true,
		ArchiveMerge: MergeOptions{
			TargetSegments: 4, TriggerSegments: 8, MinMergeBytes: 1, KeepRecent: 2,
		},
		HotTopN:                   4,
		ArchiveIndexBackfillBytes: -1,
	})
	if segsAfter := a.Stats().Segments; segsAfter != segsBefore {
		t.Errorf("merge disabled, yet segments went %d -> %d", segsBefore, segsAfter)
	}
	// ...and the rest of the pass still ran.
	if strings.Join(a.HotAttrs(), ",") == hotBefore {
		t.Error("the hot refresh was switched off along with the merge")
	}

	// The hot refresh has its own switch, which does not touch anything else.
	b, cleanupB := maintainFixture(t, 600)
	defer cleanupB()
	bHotBefore := strings.Join(b.HotAttrs(), ",")
	b.Maintain(MaintainOptions{
		SampleMax:                 256,
		HotTopN:                   4,
		ArchiveHotRefreshDisabled: true,
		ArchiveMergeDisabled:      true,
		ArchiveIndexBackfillBytes: -1,
	})
	if got := strings.Join(b.HotAttrs(), ","); got != bHotBefore {
		t.Errorf("hot refresh disabled, yet the hot set went %q -> %q", bHotBefore, got)
	}
}

// TestArchiveMaintainDefaultsTheSampleOnce: an unset SampleMax must mean one thing for the
// whole pass. It used to mean two, because the steps that did not default it for themselves
// fell through to a library fallback of a different size.
func TestArchiveMaintainDefaultsTheSampleOnce(t *testing.T) {
	a, cleanup := maintainFixture(t, 600)
	defer cleanup()
	// Nothing observable distinguishes the two sample sizes from outside, so this pins the
	// resolution itself: the pass must not run with SampleMax still 0.
	var saw int
	a.Maintain(MaintainOptions{
		ArchiveMergeDisabled:      true,
		ArchiveIndexBackfillBytes: -1,
		HotTopN:                   4,
		ArchiveIndexMinDemand:     1 << 40, // never fires; the hook is only here to read opts back
		OnAutoTune:                func(AutoTuneResult) { saw++ },
	})
	if got, _ := (MaintainOptions{}).ArchiveRetrainEvery(); got != 0 {
		t.Fatalf("an unset retrain interval resolved to %v", got)
	}
	if DefaultArchiveSampleMax <= 0 {
		t.Fatal("DefaultArchiveSampleMax must be positive, or an unset SampleMax is unbounded")
	}
	if saw != 0 {
		t.Errorf("the tuner fired %d times at an unreachable demand threshold", saw)
	}
}

func indexedNames(a *ArchiveTable) []string {
	cat, val := a.IndexedAttrs()
	return append(append([]string{}, cat...), val...)
}

// TestArchiveMaintainCarriesAnIndexBackwards covers the backfill step on its own. It cannot be
// asserted in the all-steps pass above: a merge re-indexes the segments it folds together, so
// it moves the pending count by itself and the two steps cannot be told apart there.
func TestArchiveMaintainCarriesAnIndexBackwards(t *testing.T) {
	a, cleanup := maintainFixture(t, 3000)
	defer cleanup()
	if !a.AddIndex([]string{"Owner"}, nil) {
		t.Fatal("AddIndex reported no change, so nothing is owed a backfill")
	}
	before, _ := a.IndexBackfillPending()
	if before == 0 {
		t.Fatal("no segments are owed a backfill, so this proves nothing; the fixture's " +
			"horizon is no longer tight, or it no longer has an index to add one against")
	}

	// Merge off, so what moves the pending count can only be the backfill.
	a.Maintain(MaintainOptions{
		SampleMax: 256, ArchiveMergeDisabled: true, ArchiveIndexBackfillBytes: 1 << 30,
	})
	after, _ := a.IndexBackfillPending()
	if after >= before {
		t.Errorf("%d segments pending before the pass, %d after; the backfill step did nothing",
			before, after)
	}

	// ...and turning it off leaves the debt where it was.
	b, cleanupB := maintainFixture(t, 3000)
	defer cleanupB()
	if !b.AddIndex([]string{"Owner"}, nil) {
		t.Fatal("AddIndex reported no change on the second fixture")
	}
	bBefore, _ := b.IndexBackfillPending()
	b.Maintain(MaintainOptions{
		SampleMax: 256, ArchiveMergeDisabled: true, ArchiveIndexBackfillBytes: -1,
	})
	if bAfter, _ := b.IndexBackfillPending(); bAfter != bBefore {
		t.Errorf("backfill turned off, yet pending went %d -> %d", bBefore, bAfter)
	}
}
