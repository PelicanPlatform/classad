package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestCollapseBeforeRewriteLeavesNoFragment pins what the pre-compaction collapse is FOR.
//
// Compaction takes the active segment as a source and seals it (compactShard phase 1), and the
// active segment is the only place a live delta can be -- so every open chain is about to be
// rewritten by a path that would strip its delta flag and leave a fragment claiming to be a whole
// ad. The collapse has to convert all of them first, and this checks the result rather than the
// mechanism: after it, nothing current is a delta and every key reads back whole.
func TestCollapseBeforeRewriteLeavesNoFragment(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()

	const n = 60
	for i := 0; i < n; i++ {
		tx := c.Begin()
		tx.Put([]byte(fmt.Sprintf("job%d", i)), jobAd(i, nil))
		if r := tx.Commit(); r.Conflicted() {
			t.Fatalf("seed %d conflicted", i)
		}
	}
	for round := 0; round < 3; round++ {
		for i := 0; i < n; i++ {
			patch := classad.New()
			patch.InsertAttr("JobStatus", int64(round+3))
			tx := c.Begin()
			tx.PatchAttrs([]byte(fmt.Sprintf("job%d", i)), patch, nil)
			if r := tx.Commit(); r.Conflicted() {
				t.Fatalf("patch %d conflicted", i)
			}
		}
	}
	deltas, _ := c.DeltaStats()
	if deltas == 0 {
		t.Fatal("no delta records were written; the test would pass trivially")
	}
	// Chains are open here: confirm it through the store rather than the tracker, since the
	// tracker is the thing under test's input.
	openBefore := 0
	for i := 0; i < n; i++ {
		if c.endsInDelta([]byte(fmt.Sprintf("job%d", i))) {
			openBefore++
		}
	}
	if openBefore == 0 {
		t.Fatal("no key's current record is a delta; nothing for the collapse to do")
	}

	c.collapseBeforeRewrite()

	for i := 0; i < n; i++ {
		key := fmt.Sprintf("job%d", i)
		if c.endsInDelta([]byte(key)) {
			t.Fatalf("%s still ends in a delta after the collapse", key)
		}
		ad, ok := c.Get([]byte(key))
		if !ok {
			t.Fatalf("%s vanished", key)
		}
		v, ok := ad.EvaluateAttrInt("JobStatus")
		if !ok || v != 5 {
			t.Fatalf("%s JobStatus=%v (ok=%v), want 5 -- the last patch did not survive", key, v, ok)
		}
		if got := len(ad.AST().Attributes); got < 44 {
			t.Fatalf("%s has %d attributes: a fragment, not the whole ad", key, got)
		}
	}
	t.Logf("collapsed %d open chains; every key reads back whole", openBefore)
}
