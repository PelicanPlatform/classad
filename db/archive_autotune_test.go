package db

import (
	"fmt"
	"slices"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// archiveTestHorizon is a backfill horizon large enough to cover these small fixtures.
// The tuner declines to act on an archive with no horizon at all, so every tuning test has
// to set one -- see TestArchiveAutoTuneNeedsBackfillHorizon.
const archiveTestHorizon = 1 << 30

func fillArchive(t *testing.T, a *ArchiveTable, n int) {
	t.Helper()
	for i := range n {
		ad := classad.New()
		ad.Set("ClusterId", int64(i))
		ad.Set("Owner", fmt.Sprintf("user%d", i%7))
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
}

func drainArchive(t *testing.T, a *ArchiveTable, constraint string) {
	t.Helper()
	seq, err := a.Query(constraint)
	if err != nil {
		t.Fatalf("query %q: %v", constraint, err)
	}
	for range seq {
	}
}

// TestArchiveAutoTuneAddsFromDemand: an archive that has been queried on an unindexed
// attribute enough times gets the index, without anyone asking.
func TestArchiveAutoTuneAddsFromDemand(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{IndexBackfillBytes: archiveTestHorizon})
	if err != nil {
		t.Fatal(err)
	}
	fillArchive(t, a, 500)

	if cat, val := a.IndexedAttrs(); len(cat) != 0 || len(val) != 0 {
		t.Fatalf("archive starts with indexes %v/%v, want none", cat, val)
	}
	for range 10 {
		drainArchive(t, a, `Owner == "user3"`)
	}
	res := a.AutoTune(AutoTuneOptions{MinDemand: 5, Reindex: true})
	if !res.Changed {
		t.Fatalf("AutoTune made no change; suggestions were %v", res.Changes)
	}
	catAttrs, _ := a.IndexedAttrs()
	if !slices.Contains(catAttrs, "Owner") {
		t.Errorf("categorical indexes = %v, want Owner", catAttrs)
	}
}

// TestArchiveAutoTuneBelowThreshold: demand under the threshold is not acted on. Adding an
// archive index means reading history back through the decompressor, so a handful of
// ad-hoc queries must not be enough to trigger it.
func TestArchiveAutoTuneBelowThreshold(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{IndexBackfillBytes: archiveTestHorizon})
	if err != nil {
		t.Fatal(err)
	}
	fillArchive(t, a, 500)
	for range 3 {
		drainArchive(t, a, `Owner == "user3"`)
	}
	if res := a.AutoTune(AutoTuneOptions{MinDemand: 100, Reindex: true}); res.Changed {
		t.Errorf("AutoTune acted on 3 queries against a threshold of 100: %v", res.Changes)
	}
	if c, v := a.IndexedAttrs(); len(c) != 0 || len(v) != 0 {
		t.Errorf("indexes = %v/%v, want none", c, v)
	}
}

// TestArchiveAutoTunePersistsAcrossReopen is the one that matters for cost. An auto-added
// index that is not written to the archive config is discarded on reopen along with the
// backfill that produced it -- and re-added as soon as demand accrues again, paying the
// same backfill once per restart, forever.
func TestArchiveAutoTunePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{IndexBackfillBytes: archiveTestHorizon})
	if err != nil {
		t.Fatal(err)
	}
	fillArchive(t, a, 500)
	for range 10 {
		drainArchive(t, a, `Owner == "user3"`)
	}
	if res := a.AutoTune(AutoTuneOptions{MinDemand: 5, Reindex: true}); !res.Changed {
		t.Fatal("AutoTune made no change")
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}

	cat2, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat2.Close()
	a2, ok := cat2.ArchiveTable("history")
	if !ok {
		t.Fatal("archive table missing after reopen")
	}
	catAttrs, _ := a2.IndexedAttrs()
	if !slices.Contains(catAttrs, "Owner") {
		t.Errorf("categorical indexes after reopen = %v, want Owner retained", catAttrs)
	}
	// And the index actually answers: the reopened archive must still return the rows.
	seq, err := a2.Query(`Owner == "user3"`)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range seq {
		n++
	}
	if want := 500 / 7; n < want-1 || n > want+1 {
		t.Errorf("query after reopen returned %d rows, want ~%d", n, want)
	}
}

// autoIndexedArchive builds an archive whose Owner index was added by the tuner, then
// reopens it. The reopen is what makes the index read as UNUSED: demand is not checkpointed
// here, so the fresh process has none, which is the state an auto-drop acts on.
func autoIndexedArchive(t *testing.T, dir string) *ArchiveTable {
	t.Helper()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{IndexBackfillBytes: archiveTestHorizon})
	if err != nil {
		t.Fatal(err)
	}
	fillArchive(t, a, 500)
	for range 10 {
		drainArchive(t, a, `Owner == "user3"`)
	}
	if res := a.AutoTune(AutoTuneOptions{MinDemand: 5, Reindex: true}); !res.Changed {
		t.Fatal("setup: AutoTune added nothing")
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	cat2, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat2.Close() })
	a2, ok := cat2.ArchiveTable("history")
	if !ok {
		t.Fatal("setup: archive missing after reopen")
	}
	return a2
}

// TestArchiveAutoIndexProvenancePersists: an index the tuner added must come back from a
// restart still marked auto. Restored as human-created it would be exempt from trimming and
// from any future auto-drop -- permanently, on the strength of nothing but a restart.
func TestArchiveAutoIndexProvenancePersists(t *testing.T) {
	a := autoIndexedArchive(t, t.TempDir())
	if got := a.AutoIndexNames(); !slices.Contains(got, "Owner") {
		t.Errorf("auto index names after reopen = %v, want Owner", got)
	}
}

// TestArchiveAutoTuneNeverDrops: DropUnused is forced off whatever the caller passes.
// Dropping an archive index is nearly free and re-adding it costs a backfill, so the
// asymmetry runs the wrong way for acting on an absence of queries.
//
// The raw collections path DOES drop in this exact state (verified while writing this), so
// the assertion is about the archive rule, not about the drop being unreachable.
func TestArchiveAutoTuneNeverDrops(t *testing.T) {
	a := autoIndexedArchive(t, t.TempDir())
	a.AutoTune(AutoTuneOptions{
		MinDemand: 5, Reindex: true,
		DropUnused:           true, // asked for, and must be ignored
		DropUnusedMinWindow:  -1,   // waive every generic guard, leaving only the archive rule
		DropUnusedMinQueries: -1,
	})
	if catAttrs, _ := a.IndexedAttrs(); !slices.Contains(catAttrs, "Owner") {
		t.Errorf("Owner index dropped: indexes = %v", catAttrs)
	}
}

// TestArchiveAutoTuneNeedsBackfillHorizon: the tuner acts on the default config (zero takes
// the 1 GiB default) but declines when backfill is explicitly unbounded. A background pass
// nobody watched decide must not be able to start work bounded only by the archive's size.
func TestArchiveAutoTuneNeedsBackfillHorizon(t *testing.T) {
	for _, tc := range []struct {
		name     string
		horizon  int64
		wantTune bool
	}{
		{"zero takes the default and tunes", 0, true},
		{"explicitly unbounded declines", -1, false},
		{"bounded horizon tunes", 1 << 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cat, err := OpenCatalog(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer cat.Close()
			a, err := cat.CreateArchiveTable("history", ArchiveConfig{IndexBackfillBytes: tc.horizon})
			if err != nil {
				t.Fatal(err)
			}
			fillArchive(t, a, 500)
			for range 10 {
				drainArchive(t, a, `Owner == "user3"`)
			}
			res := a.AutoTune(AutoTuneOptions{MinDemand: 5, Reindex: true})
			if res.Changed != tc.wantTune {
				t.Errorf("AutoTune changed = %v, want %v (horizon %d)", res.Changed, tc.wantTune, tc.horizon)
			}
			catAttrs, _ := a.IndexedAttrs()
			if got := slices.Contains(catAttrs, "Owner"); got != tc.wantTune {
				t.Errorf("Owner indexed = %v, want %v; indexes = %v", got, tc.wantTune, catAttrs)
			}
		})
	}
}

// TestArchiveBackfillHorizonDefault pins the resolution rule, since all three cases are
// reachable from a config file and only one of them is what an unset field means.
func TestArchiveBackfillHorizonDefault(t *testing.T) {
	for _, tc := range []struct {
		set, want int64
	}{
		{0, defaultIndexBackfillBytes}, // unset: bounded, not unbounded
		{-1, 0},                        // explicitly unbounded, spelled 0 for the storage layer
		{4 << 20, 4 << 20},             // honoured verbatim
	} {
		if got := (ArchiveConfig{IndexBackfillBytes: tc.set}).backfillHorizon(); got != tc.want {
			t.Errorf("backfillHorizon(%d) = %d, want %d", tc.set, got, tc.want)
		}
	}
}

// TestArchiveHorizonDoesNotStarveMergedSegments: the backfill horizon bounds rebuilding an
// EXISTING sidecar under a newer spec. It must not stop a MERGED segment -- which has no
// sidecar at all -- from getting one, however old the data in it is. If it did, merging
// would silently convert indexed old segments into unindexed ones, and the horizon would
// make the archive slower the more maintenance ran on it.
func TestArchiveHorizonDoesNotStarveMergedSegments(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{
		SegmentSize:        1 << 14, // small, so the fixture produces many segments
		IndexBackfillBytes: 1,       // horizon smaller than a single segment
		CategoricalAttrs:   []string{"Owner"},
	})
	if err != nil {
		t.Fatal(err)
	}
	fillArchive(t, a, 4000)

	before := a.Stats().Segments
	if before < 4 {
		t.Fatalf("fixture made %d segments, need several to merge", before)
	}
	merges := a.MergePass(MergeOptions{
		TargetSegments: 2, TriggerSegments: 3,
		MinMergeBytes: 1, KeepRecent: 1, MaxRun: 64,
	})
	if merges == 0 {
		t.Fatalf("no merge happened (%d segments); the test would prove nothing", before)
	}

	// The merged output covers the OLDEST data, well outside a 1-byte horizon.
	if stale, sealed := a.StaleIndexSegments(); stale != 0 {
		t.Errorf("%d of %d sealed segments left without a current index after merging", stale, sealed)
	}
	// And the index answers over the merged data: every user must still be findable.
	for u := range 7 {
		seq, err := a.Query(fmt.Sprintf(`Owner == "user%d"`, u))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for range seq {
			n++
		}
		if want := 4000 / 7; n < want-1 || n > want+1 {
			t.Errorf("user%d: %d rows after merge, want ~%d", u, n, want)
		}
	}
}
