package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// strandFragments produces the real stranded state: a live DELTA sitting in a sealed segment
// whose base is still on disk. It suppresses the collapse while the segment seals, then drops the
// shard's pending-seal bookkeeping the way a restart does -- which is exactly how a fragment ends
// up somewhere no later pass will look.
//
// Flipping a whole record's delta flag would be easier and would test the wrong thing: that
// produces a chain with no base at all, which is the already-lost case and is not rescuable.
func strandFragments(t *testing.T, c *Collection, keys []string) [][]byte {
	t.Helper()
	// Block the post-commit collapse so the seal below leaves the fragments behind.
	if !c.collapsing.CompareAndSwap(false, true) {
		t.Fatal("a collapse is already in flight")
	}
	for _, k := range keys {
		p := classad.New()
		p.InsertAttr("JobStatus", int64(7))
		tx := c.Begin()
		tx.PatchAttrs([]byte(k), p, nil)
		if r := tx.Commit(); r.Conflicted() || r.HasUnapplied() {
			c.collapsing.Store(false)
			t.Fatalf("patch %s did not land", k)
		}
	}
	// Fill until the active segment seals, carrying those deltas into a sealed one.
	for i := range 3000 {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(90000+i))
		for j := range 20 {
			ad.InsertAttr(fmt.Sprintf("Pad%02d", j), int64(j))
		}
		tx := c.Begin()
		tx.Put([]byte(fmt.Sprintf("%d.0", 90000+i)), ad)
		tx.Commit()
	}
	// Drop the bookkeeping, as a restart does: pendingSeal and the retry set are in-memory.
	for _, sh := range c.shards {
		sh.mu.Lock()
		sh.pendingSeal = nil
		sh.sealedPending.Store(false)
		sh.mu.Unlock()
	}
	c.collapseMu.Lock()
	clear(c.collapseRetry)
	c.collapseMu.Unlock()
	c.collapsing.Store(false)

	var out [][]byte
	for _, k := range keys {
		out = append(out, []byte(k))
	}
	return out
}

func seedForStranding(t *testing.T, c *Collection, n int) {
	t.Helper()
	for i := range n {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttr("JobStatus", int64(1))
		for j := range 20 {
			ad.InsertAttr(fmt.Sprintf("Pad%02d", j), int64(j))
		}
		tx := c.Begin()
		tx.Put([]byte(fmt.Sprintf("%d.0", i)), ad)
		tx.Commit()
	}
}

// The pre-compaction collapse must find a fragment stranded in a SEALED segment. That pass is the
// last moment one can be rescued -- the rewrite it precedes is what drops the superseded base the
// fragment depends on -- and it used to scan only the active segments, so a stranded fragment
// went into compaction invisible and came out unreadable.
func TestPreCompactionCollapseFindsStrandedFragments(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()
	seedForStranding(t, c, 2000)

	stranded := strandFragments(t, c, []string{"1.0", "2.0", "3.0"})
	if got := c.checkSealCollapseInvariant(20); len(got) == 0 {
		t.Fatal("nothing was stranded: the fixture is not reproducing the state under test")
	}

	// Active-only, as the per-seal pass does: the stranded fragments are invisible.
	activeOnly := c.liveDeltaKeys(false)
	// Sealed included, as the pre-compaction pass now does: they are found.
	withSealed := c.liveDeltaKeys(true)

	found := func(keys [][]byte, want []byte) bool {
		for _, k := range keys {
			if string(k) == string(want) {
				return true
			}
		}
		return false
	}
	for _, k := range stranded {
		if found(activeOnly, k) {
			t.Fatalf("%q was visible to the active-only scan; this fixture is not stranding anything", k)
		}
		if !found(withSealed, k) {
			t.Errorf("%q is stranded in a sealed segment and the sealed scan did not find it", k)
		}
	}
}

// And the repair must actually make them whole again, not merely notice them. A fragment whose
// base is still on disk is rescuable; leaving it is a row that becomes unreadable at the next
// compaction and refuses every write to that key thereafter.
func TestRepairRescuesAStrandedFragment(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()
	seedForStranding(t, c, 2000)

	stranded := strandFragments(t, c, []string{"1.0", "2.0", "3.0"})
	if got := c.checkSealCollapseInvariant(20); len(got) == 0 {
		t.Fatal("nothing was stranded: the fixture is not reproducing the state under test")
	}
	c.repairStrandedSealedDeltas(stranded)

	// Nothing is left stranded, and every rescued key still reads with its attributes intact.
	if got := c.checkSealCollapseInvariant(20); len(got) != 0 {
		t.Errorf("%d fragment(s) still stranded after the repair: %q", len(got), got[0])
	}
	for _, k := range stranded {
		ad, ok := c.Get(k)
		if !ok {
			t.Fatalf("%q is unreadable after the repair", k)
		}
		if _, ok := ad.EvaluateAttrInt("ClusterId"); !ok {
			t.Errorf("%q lost ClusterId: the repair wrote a fragment rather than a whole record", k)
		}
		if _, ok := ad.EvaluateAttrInt("Pad19"); !ok {
			t.Errorf("%q lost its base attributes", k)
		}
		if v, ok := ad.EvaluateAttrInt("JobStatus"); !ok || v != 7 {
			t.Errorf("%q JobStatus = %d (present %v), want 7: the patch was dropped", k, v, ok)
		}
	}
}
