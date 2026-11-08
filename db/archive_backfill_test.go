package db

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestArchiveReindexAllReachesAutoIndexBeyondHorizon follows the production sequence: a
// history archive with a human index on ClusterId, queried on EnteredHistoryTime until the
// auto-tuner indexes it. The tuner's add is carried only as far as the backfill horizon, so
// the older segments answer EnteredHistoryTime queries by scanning. Reindex alone changes
// nothing for them; ReindexAll must cover them all, and keep them covered across a reopen
// with the index's auto provenance intact.
func TestArchiveReindexAllReachesAutoIndexBeyondHorizon(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{
		SegmentSize:        1 << 12,
		ValueAttrs:         []string{"ClusterId"},
		IndexBackfillBytes: 16 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("p", 24)
	for i := range 2000 {
		ad := classad.New()
		ad.Set("ClusterId", int64(i))
		ad.Set("EnteredHistoryTime", int64(1000+i))
		ad.Set("Pad", pad)
		if err = a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	const expr = `EnteredHistoryTime == 1500`
	for range 10 {
		drainArchive(t, a, expr)
	}
	if res := a.AutoTune(AutoTuneOptions{MinDemand: 5, Reindex: true}); !res.Changed {
		t.Fatalf("auto-tuner added nothing: %v", res.Changes)
	}
	if !slices.Contains(a.AutoIndexNames(), "EnteredHistoryTime") {
		t.Fatalf("auto indexes = %v, want EnteredHistoryTime", a.AutoIndexNames())
	}
	a.Reindex()
	ex, err := a.Explain(expr)
	if err != nil {
		t.Fatal(err)
	}
	if ex.UnindexedSegments == 0 {
		t.Fatalf("fixture: the horizon left nothing unindexed (%s)", ex.Reason)
	}
	if pending, _ := a.IndexBackfillPending(); pending == 0 {
		t.Fatal("IndexBackfillPending reports nothing while segments lack the index")
	}

	res := a.ReindexAll()
	if res.PendingSegments != 0 || res.Rebuilt == 0 {
		t.Fatalf("ReindexAll = %+v", res)
	}
	if ex, _ := a.Explain(expr); ex.UnindexedSegments != 0 {
		t.Errorf("after ReindexAll: %s", ex.Reason)
	}
	if err = cat.Close(); err != nil {
		t.Fatal(err)
	}

	cat2, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat2.Close()
	a2, ok := cat2.ArchiveTable("history")
	if !ok {
		t.Fatal("archive not recovered")
	}
	if ex, _ := a2.Explain(expr); ex.UnindexedSegments != 0 {
		t.Errorf("after reopen: %s", ex.Reason)
	}
	if pending, _ := a2.IndexBackfillPending(); pending != 0 {
		t.Errorf("after reopen %d segments pending", pending)
	}
	if !slices.Contains(a2.AutoIndexNames(), "EnteredHistoryTime") {
		t.Errorf("auto provenance lost across reopen: %v", a2.AutoIndexNames())
	}
	seq, err := a2.Query(expr)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range seq {
		n++
	}
	if n != 1 {
		t.Errorf("%s: %d rows, want 1", expr, n)
	}
}

func TestMaintainOptionsArchiveIndexBackfillBudget(t *testing.T) {
	for _, tc := range []struct {
		set  int64
		want int64
		on   bool
	}{
		{0, DefaultArchiveIndexBackfillBytes, true},
		{1 << 20, 1 << 20, true},
		{-1, 0, false},
	} {
		got, on := MaintainOptions{ArchiveIndexBackfillBytes: tc.set}.ArchiveIndexBackfillBudget()
		if got != tc.want || on != tc.on {
			t.Errorf("ArchiveIndexBackfillBytes=%d: (%d, %v), want (%d, %v)", tc.set, got, on, tc.want, tc.on)
		}
	}
}

// TestArchiveAutoCompositeReachesSegmentsBeyondHorizon follows the production sequence for
// a schedd-sync history archive: ClusterId and ProcId indexed from the start, a workload
// that pins them together on every ingested record (the dedup probe), and the maintenance
// pass's auto-tuner creating the composite from that demand. The archive is mature, so most
// of it lies beyond the backfill horizon -- which is where the composite has to reach, since
// that is where the history it answers questions about lives.
func TestArchiveAutoCompositeReachesSegmentsBeyondHorizon(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{
		SegmentSize:        64 << 10,
		IndexBackfillBytes: 32 << 10,
		CategoricalAttrs:   []string{"Owner"},
		ValueAttrs:         []string{"ClusterId", "ProcId"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for c := range 300 {
		for p := range 20 {
			ad := classad.New()
			ad.Set("ClusterId", int64(c))
			ad.Set("ProcId", int64(p))
			ad.Set("Owner", "user")
			ad.Set("Pad", strings.Repeat("p", 24))
			if err := a.Append(ad); err != nil {
				t.Fatal(err)
			}
		}
	}
	a.Reindex()
	if n, _ := a.IndexBackfillPending(); n != 0 {
		t.Fatalf("%d segment(s) uncovered before the composite exists: the test cannot tell the "+
			"composite's backfill from an ordinary one", n)
	}

	for i := range 300 {
		q := fmt.Sprintf("ClusterId == %d && ProcId == %d", i%300, i%20)
		seq, err := a.QueryLimit(q, 1)
		if err != nil {
			t.Fatal(err)
		}
		for range seq {
		}
	}
	res := a.AutoTune(AutoTuneOptions{SampleMax: 4096, MinDemand: 100, Reindex: true})
	if !res.Changed || len(a.CompositeAttrs()) == 0 {
		t.Fatalf("the tuner did not create the composite (changes %v); the rest of the test has "+
			"nothing to measure", res.Changes)
	}

	deferred := a.a.StaleIndexSegmentsByPolicy()
	if deferred == 0 {
		t.Fatal("no sealed segment fell outside the horizon, so nothing was left for the backfill")
	}
	if n, _ := a.IndexBackfillPending(); n != deferred {
		t.Errorf("IndexBackfillPending = %d, want the %d segment(s) left outside the horizon", n, deferred)
	}
	if r := a.BackfillIndexes(1 << 30); r.Rebuilt != deferred || r.PendingSegments != 0 {
		t.Errorf("backfill rebuilt %d of %d segment(s), %d still pending", r.Rebuilt, deferred, r.PendingSegments)
	}
	if n := a.a.StaleIndexSegmentsByPolicy(); n != 0 {
		t.Errorf("%d sealed segment(s) still carry the pre-composite index generation", n)
	}

	// The payoff, in the planner's own words.
	ex, err := a.Explain("ClusterId == 7 && ProcId == 3")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ex.Reason, "from the composite index on ClusterId+ProcId") {
		t.Errorf("the conjunction does not answer from the composite: %s", ex.Reason)
	}
}
