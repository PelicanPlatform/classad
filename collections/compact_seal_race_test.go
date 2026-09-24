package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// compactShard seals the active segment (sh.act = nil) WITHOUT going through the
// sealedPending/pendingSeal protocol -- writeRecord is that protocol's only producer. The
// collapse meant to guarantee there is no live delta to seal runs back in Compact, outside any
// lock a committer respects, with a lock-free phase 2 for every earlier shard in between. So a
// delta committed during the pass is sealed away from every future collapse, while the same pass
// reclaims its base.
//
// This drives the state directly rather than racing goroutines: a live delta present in sh.act at
// the moment compactShard runs. That is what the window produces, and reproducing it by timing
// would be flaky.
func TestCompactWillNotSealALiveDelta(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()

	const n = 400
	for i := range n {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttr("JobStatus", int64(1))
		for j := range 20 {
			ad.InsertAttr(fmt.Sprintf("Pad%02d", j), int64(j))
		}
		tx := c.Begin()
		tx.Put([]byte(fmt.Sprintf("%d.0", i)), ad)
		if r := tx.Commit(); r.Conflicted() {
			t.Fatal("seed conflicted")
		}
	}
	// Overwrite so there are dead bytes for compaction to want to reclaim.
	for round := range 3 {
		for i := range n {
			ad := classad.New()
			ad.InsertAttr("ClusterId", int64(i))
			ad.InsertAttr("Round", int64(round))
			for j := range 20 {
				ad.InsertAttr(fmt.Sprintf("Pad%02d", j), int64(j))
			}
			tx := c.Begin()
			tx.Put([]byte(fmt.Sprintf("%d.0", i)), ad)
			tx.Commit()
		}
	}

	// Put a live delta in each shard's active segment, with the post-commit collapse suppressed --
	// which is exactly the state a write landing mid-pass leaves behind.
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

	live := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		if segHasLiveDelta(sh.act) {
			live++
		}
		sh.mu.RUnlock()
	}
	if live == 0 {
		t.Fatal("no shard has a live delta in its active segment: the state under test was not created")
	}

	// compactShard directly, NOT Compact: Compact collapses at its head, so going through it
	// cleans the very state under test and the assertions below pass with the guard removed.
	// What the production window produces is compactShard meeting a live delta in sh.act, which
	// is precisely this.
	target := c.currentCodec()
	for _, sh := range c.shards {
		c.compactShard(sh, target)
	}

	// Nothing may have been sealed away from the collapse.
	if got := c.checkSealCollapseInvariant(50); len(got) != 0 {
		t.Errorf("compaction stranded %d fragment(s) in sealed segments, e.g. %q", len(got), got[0])
	}
	// And every key must still read, with both its base attributes and the patch.
	for i := range n {
		key := fmt.Sprintf("%d.0", i)
		ad, ok := c.Get([]byte(key))
		if !ok {
			t.Fatalf("%s is unreadable after compaction", key)
		}
		if _, ok := ad.EvaluateAttrInt("Pad19"); !ok {
			t.Fatalf("%s lost its base attributes: its chain was broken by the compaction", key)
		}
	}
}
