package db

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/collections"
)

// Retention is the only step in the archive maintenance pass that bounds the table's SIZE, and
// the only one that destroys data. These cover both halves of that: that the pass actually
// enforces the configured bounds (it did not, so an embedder that wrote a retention policy and
// no rotation loop grew without limit), and that it cannot touch an archive that configured
// none, which is what makes running by default acceptable.

// rotateFixture builds an archive with tiny segments and enough ads to fill several of them,
// so there is something outside any bound worth testing. Returns the table and the catalog's
// directory, for the tests that need to reach the segment files.
func rotateFixture(t *testing.T, cfg ArchiveConfig) (*ArchiveTable, string) {
	t.Helper()
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	if cfg.SegmentSize == 0 {
		cfg.SegmentSize = 4096
	}
	hist, err := cat.CreateArchiveTable("history", cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2000 {
		if err := hist.AppendOld(fmt.Sprintf(
			"Id = %d\nCompletionDate = %d\nPad = \"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\"",
			i, 1000+i)); err != nil {
			t.Fatal(err)
		}
	}
	return hist, dir
}

// quietPass is a maintenance pass with everything but rotation turned off, so a count that
// changes can only have been rotation's doing. The backfill and merge are the two steps that
// would otherwise move segments around underneath the assertions.
func quietPass() MaintainOptions {
	return MaintainOptions{ArchiveMergeDisabled: true, ArchiveIndexBackfillBytes: -1}
}

// TestMaintainRotatesToRetention: the pass enforces the archive's configured bounds. Until it
// did, ArchiveTable.Rotate had exactly one caller anywhere, so every other embedder of this
// package got a history table that only grew.
func TestMaintainRotatesToRetention(t *testing.T) {
	hist, _ := rotateFixture(t, ArchiveConfig{Retention: collections.Retention{MaxSegments: 2}})

	// Precondition, asserted rather than assumed: a fixture already inside its bounds would
	// drop nothing whatever the code did, and the test would pass on an empty pass.
	segsBefore := hist.Stats().Segments
	if segsBefore <= 2 {
		t.Fatalf("fixture holds %d segments, which is already within MaxSegments=2: nothing to drop", segsBefore)
	}
	countBefore := hist.Count()

	var got []RotateResult
	opts := quietPass()
	opts.OnRotate = func(res RotateResult) { got = append(got, res) }
	hist.Maintain(opts)

	if len(got) != 1 {
		t.Fatalf("OnRotate fired %d times, want exactly 1 for a pass that dropped segments", len(got))
	}
	if got[0].Err != nil {
		t.Fatalf("rotation failed: %v", got[0].Err)
	}
	if got[0].Dropped <= 0 {
		t.Fatalf("the pass reported a rotation that dropped %d segments", got[0].Dropped)
	}
	segsAfter := hist.Stats().Segments
	if segsAfter >= segsBefore {
		t.Fatalf("segment count did not shrink: %d -> %d", segsBefore, segsAfter)
	}
	if want := segsBefore - segsAfter; got[0].Dropped != want {
		t.Errorf("reported %d segments dropped, the table lost %d", got[0].Dropped, want)
	}
	if hist.Count() >= countBefore {
		t.Fatalf("record count did not shrink: %d -> %d", countBefore, hist.Count())
	}
}

// TestMaintainRotateUsesTheSuppliedClock: the pass supplies now to Rotate, and it is the clock
// the pass holds rather than zero or the wall clock. Both halves matter -- a zero now never
// crosses a MaxAge ceiling, and the wall clock crosses every ceiling a synthetic fixture has --
// so each is a case here.
func TestMaintainRotateUsesTheSuppliedClock(t *testing.T) {
	// CompletionDate runs 1000..2999, and a segment goes when its NEWEST value is older than
	// now-MaxAge. Synthetic values far below any real unix time, so the wall clock would
	// condemn the whole table.
	cfg := ArchiveConfig{
		ZoneAttrs: []string{"CompletionDate"},
		Retention: collections.Retention{MaxAgeAttr: "CompletionDate", MaxAge: 100},
	}

	t.Run("now past the ceiling drops", func(t *testing.T) {
		hist, _ := rotateFixture(t, cfg)
		segsBefore := hist.Stats().Segments
		if segsBefore <= 1 {
			t.Fatalf("fixture holds %d segments; need several sealed ones to rotate", segsBefore)
		}
		hist.nowFn = func() float64 { return 5000 } // every sealed segment's max is < 4900
		var got []RotateResult
		opts := quietPass()
		opts.OnRotate = func(res RotateResult) { got = append(got, res) }
		hist.Maintain(opts)
		if len(got) != 1 || got[0].Dropped <= 0 {
			t.Fatalf("a clock past the MaxAge ceiling dropped nothing: %+v", got)
		}
		if hist.Stats().Segments >= segsBefore {
			t.Fatalf("segment count did not shrink: %d -> %d", segsBefore, hist.Stats().Segments)
		}
	})

	t.Run("now before the ceiling keeps", func(t *testing.T) {
		hist, _ := rotateFixture(t, cfg)
		segsBefore, countBefore := hist.Stats().Segments, hist.Count()
		if segsBefore <= 1 {
			t.Fatalf("fixture holds %d segments; need several sealed ones to rotate", segsBefore)
		}
		// 1050-100 = 950, below every CompletionDate in the fixture: nothing has aged out.
		hist.nowFn = func() float64 { return 1050 }
		var got []RotateResult
		opts := quietPass()
		opts.OnRotate = func(res RotateResult) { got = append(got, res) }
		hist.Maintain(opts)
		if len(got) != 0 {
			t.Fatalf("a clock short of the MaxAge ceiling rotated anyway: %+v", got)
		}
		if hist.Stats().Segments != segsBefore || hist.Count() != countBefore {
			t.Fatalf("nothing had aged out, but the table changed: segments %d -> %d, records %d -> %d",
				segsBefore, hist.Stats().Segments, countBefore, hist.Count())
		}
	})
}

// TestMaintainRotateDisabled: the switch is honoured. Asserted against the same fixture run
// twice -- once off, once on -- because "dropped nothing" proves the switch only if the same
// table would otherwise have dropped something.
func TestMaintainRotateDisabled(t *testing.T) {
	hist, _ := rotateFixture(t, ArchiveConfig{Retention: collections.Retention{MaxSegments: 2}})
	segsBefore, countBefore := hist.Stats().Segments, hist.Count()
	if segsBefore <= 2 {
		t.Fatalf("fixture holds %d segments, already within MaxSegments=2", segsBefore)
	}

	var got []RotateResult
	opts := quietPass()
	opts.ArchiveRotateDisabled = true
	opts.OnRotate = func(res RotateResult) { got = append(got, res) }
	hist.Maintain(opts)

	if len(got) != 0 {
		t.Fatalf("rotation ran with ArchiveRotateDisabled set: %+v", got)
	}
	if hist.Stats().Segments != segsBefore || hist.Count() != countBefore {
		t.Fatalf("the disabled pass changed the table: segments %d -> %d, records %d -> %d",
			segsBefore, hist.Stats().Segments, countBefore, hist.Count())
	}

	// The same table, the same bounds, the switch cleared: it drops. Without this the check
	// above would pass on a build where rotation never ran at all.
	opts.ArchiveRotateDisabled = false
	hist.Maintain(opts)
	if len(got) != 1 || got[0].Dropped <= 0 {
		t.Fatalf("with the switch cleared the same table dropped nothing: %+v", got)
	}
}

// TestMaintainRotateKeepsEverythingWithNoRetention is the property that makes rotation safe to
// run by default: retention is per-archive configuration, the zero Retention is "keep all", and
// an archive that configured none must come through the pass untouched no matter how much
// history it holds.
func TestMaintainRotateKeepsEverythingWithNoRetention(t *testing.T) {
	hist, _ := rotateFixture(t, ArchiveConfig{}) // no Retention

	if r := hist.Retention(); r != (collections.Retention{}) {
		t.Fatalf("fixture configured retention after all: %+v", r)
	}
	segsBefore, countBefore := hist.Stats().Segments, hist.Count()
	// The precondition that gives the test teeth: there is a pile of sealed segments here,
	// so a rotation that ignored the configuration would have plenty to drop.
	if segsBefore <= 2 {
		t.Fatalf("fixture holds only %d segments; a pass over it would drop nothing either way", segsBefore)
	}
	if countBefore == 0 {
		t.Fatal("fixture holds no records")
	}

	var got []RotateResult
	// The DEFAULT options, not quietPass: ArchiveRotateDisabled unset is what an embedder
	// gets, and that is the configuration under test.
	hist.Maintain(MaintainOptions{OnRotate: func(res RotateResult) { got = append(got, res) }})

	if len(got) != 0 {
		t.Fatalf("an archive with no retention configured rotated: %+v", got)
	}
	if hist.Count() != countBefore {
		t.Fatalf("record count changed with no retention configured: %d -> %d", countBefore, hist.Count())
	}
	// Segments, not just records: a merge could hold the record count steady while dropping
	// segments, so the count alone would not catch a rotation that fired.
	if hist.Stats().Segments != segsBefore {
		t.Fatalf("segment count changed with no retention configured: %d -> %d",
			segsBefore, hist.Stats().Segments)
	}
}

// TestMaintainRotateErrorDoesNotAbortThePass: a rotation that cannot release a dropped
// segment's file is reported and the pass carries on. The records are gone from the table
// either way, so abandoning the merge and the reindex over a leaked file would cost far more
// than it saved.
func TestMaintainRotateErrorDoesNotAbortThePass(t *testing.T) {
	hist, dir := rotateFixture(t, ArchiveConfig{Retention: collections.Retention{MaxSegments: 2}})
	if segs := hist.Stats().Segments; segs <= 2 {
		t.Fatalf("fixture holds %d segments, already within MaxSegments=2", segs)
	}
	// Unlink the oldest segment's file out from under the archive. The mapping stays valid,
	// so reads are unaffected; the reap that follows the drop fails on the missing file,
	// which is the error path. Rotation drops oldest-first, so this one is certainly dropped.
	if err := os.Remove(oldestSegmentFile(t, dir)); err != nil {
		t.Fatal(err)
	}

	// Due for a retrain, so the LAST step of the pass is observable: if the error aborted the
	// pass, OnRetrain never fires.
	hist.lastRetrain = time.Now().Add(-365 * 24 * time.Hour)
	var rotated []RotateResult
	var retrained int
	opts := quietPass()
	opts.ArchiveRetrainInterval = time.Nanosecond
	opts.OnRotate = func(res RotateResult) { rotated = append(rotated, res) }
	opts.OnRetrain = func(RetrainResult) { retrained++ }
	hist.Maintain(opts)

	if len(rotated) != 1 {
		t.Fatalf("OnRotate fired %d times, want 1", len(rotated))
	}
	if rotated[0].Err == nil {
		t.Fatal("removing a dropped segment's file did not make the reap fail; the error path is untested")
	}
	if rotated[0].Dropped <= 0 {
		t.Errorf("the segments were dropped from the table, so the report must still say how many: %+v", rotated[0])
	}
	if retrained != 1 {
		t.Fatalf("the pass stopped at the rotation error: OnRetrain fired %d times, want 1", retrained)
	}
}

// oldestSegmentFile returns the path of the lowest-numbered segment data file under dir.
// Segment files are named <prefix>-<n>.d<dict>.dat inside a per-shard directory; n is the
// segment id, so the smallest is the oldest and the first one rotation drops.
func oldestSegmentFile(t *testing.T, dir string) string {
	t.Helper()
	var paths []string
	if err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".dat") {
			paths = append(paths, p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no segment files under %s", dir)
	}
	sort.Slice(paths, func(i, j int) bool { return segFileID(paths[i]) < segFileID(paths[j]) })
	return paths[0]
}

// segFileID extracts n from a <prefix>-<n>.d<dict>.dat segment file name, or -1.
func segFileID(path string) int {
	base := filepath.Base(path)
	dash := strings.LastIndex(base, "-")
	dot := strings.Index(base, ".")
	if dash < 0 || dot < 0 || dot <= dash+1 {
		return -1
	}
	n, err := strconv.Atoi(base[dash+1 : dot])
	if err != nil {
		return -1
	}
	return n
}
