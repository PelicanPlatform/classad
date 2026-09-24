package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// MigrateSealedAttrs retires the active segment so it gets migrated too. It did that with no
// collapse anywhere in the pass, which is worse than the same omission elsewhere: resealSegmentsAs
// REFUSES a segment holding a delta, so the segment is not migrated either -- the pass reports
// success having both stranded the fragment and left behind the legacy bytes it exists to remove.
//
// A shard whose active segment still holds a live delta must be skipped, not retired.
func TestMigrateWillNotRetireASegmentHoldingALiveDelta(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const secret = "ClaimId-legacy-plaintext-capability"
	const n = 200
	// The real upgrade shape: written WITHOUT a key, so there is legacy plaintext to migrate.
	// Opened with one from the start there is nothing to reseal, the pass migrates zero segments,
	// and the test exercises none of the code under it -- which is what the first version did.
	pre, err := Open(Options{Shards: 2, Dir: dir, SegmentSize: 1 << 16, DeltaMax: 16})
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		ad := mustAd(t, fmt.Sprintf(`[ClusterId=%d; JobStatus=1; ClaimId=%q]`, i, secret))
		tx := pre.Begin()
		tx.Put([]byte(fmt.Sprintf("%d.0", i)), ad)
		if r := tx.Commit(); r.Conflicted() {
			t.Fatal("seed conflicted")
		}
	}
	if err := pre.Close(); err != nil {
		t.Fatal(err)
	}
	if hits := diskBytesContaining(t, dir, secret); len(hits) == 0 {
		t.Fatal("no plaintext on disk: the migration would have nothing to do")
	}

	_, dataKey := deriveDataKey(t)
	c, err := Open(Options{Shards: 2, Dir: dir, SegmentSize: 1 << 16, DataKey: dataKey, DeltaMax: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Live deltas in the active segments, with the post-commit collapse suppressed -- the state a
	// write landing mid-pass leaves behind.
	if !c.collapsing.CompareAndSwap(false, true) {
		t.Fatal("a collapse is already in flight")
	}
	for i := range n {
		p := classad.New()
		p.InsertAttr("JobStatus", int64(9))
		tx := c.Begin()
		tx.PatchAttrs([]byte(fmt.Sprintf("%d.0", i)), p, nil)
		tx.Commit()
	}
	c.collapsing.Store(false)

	dirty := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		if segHasLiveDelta(sh.act) {
			dirty++
		}
		sh.mu.RUnlock()
	}
	if dirty == 0 {
		t.Fatal("no shard has a live delta in its active segment: the state under test was not created")
	}

	// Not asserting a non-zero migration count: resealSegmentsAs refuses any segment holding a
	// delta, so a delta-mode store migrates nothing. What matters here is that the pass does not
	// STRAND anything while retiring active segments on its way to that conclusion.
	c.MigrateSealedAttrs(1)

	if got := c.checkSealCollapseInvariant(50); len(got) != 0 {
		t.Errorf("the migration stranded %d fragment(s), e.g. %q", len(got), got[0])
	}
	for i := range n {
		key := fmt.Sprintf("%d.0", i)
		ad, ok := c.Get([]byte(key))
		if !ok {
			t.Fatalf("%s is unreadable after the migration", key)
		}
		if v, ok := ad.EvaluateAttrInt("JobStatus"); !ok || v != 9 {
			t.Fatalf("%s JobStatus = %d (present %v), want 9: its patch was lost", key, v, ok)
		}
		if v, ok := ad.EvaluateAttrString("ClaimId"); !ok || v != secret {
			t.Fatalf("%s lost ClaimId (%q, present %v)", key, v, ok)
		}
	}
}
