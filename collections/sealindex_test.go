package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// A sealed segment's durable key index is written by a reindex pass, so between the seal and that
// pass the sealed-segment probe had nothing to look in. On a production mirror that window cost
// three million skipped probes in six hours, and every write refused as delta-index-pending.
//
// A segment must therefore be probeable as soon as it seals.
func TestASealedSegmentIsProbeableImmediately(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()

	for i := range 3000 {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		for j := range 20 {
			ad.InsertAttr(fmt.Sprintf("Pad%02d", j), int64(j))
		}
		tx := c.Begin()
		tx.Put([]byte(fmt.Sprintf("%d.0", i)), ad)
		tx.Commit()
	}

	sealed, unprobeable := 0, 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil || seg == sh.act || seg.used == 0 {
				continue
			}
			sealed++
			if seg.keyIdx.Load() == nil && seg.keyIdxMem.Load() == nil {
				unprobeable++
			}
		}
		sh.mu.RUnlock()
	}
	if sealed == 0 {
		t.Fatal("nothing sealed: the fixture did not roll a segment, so this proves nothing")
	}
	if unprobeable != 0 {
		t.Errorf("%d of %d sealed segments have no key index of either kind", unprobeable, sealed)
	}
	if ProvisionalIndexBuilds() == 0 {
		t.Error("no provisional index was built, so the sealed segments were probeable for some other reason")
	}
}

// The provisional index must answer the same lookups the durable one does, or closing the window
// just replaces a blind probe with a wrong one.
func TestTheProvisionalIndexFindsTheSameKeys(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()

	const n = 3000
	for i := range n {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttrString("Owner", fmt.Sprintf("user%d", i%7))
		for j := range 20 {
			ad.InsertAttr(fmt.Sprintf("Pad%02d", j), int64(j))
		}
		tx := c.Begin()
		tx.Put([]byte(fmt.Sprintf("%d.0", i)), ad)
		tx.Commit()
	}

	// Force every lookup through the SEALED probe: no durable index, no bloom, and an empty
	// bucket directory. Clearing the directory is the part that matters -- Get resolves from the
	// bucket chain first, so without this the sealed probe is never reached and every assertion
	// below passes whatever the probe does.
	provisional := 0
	for _, sh := range c.shards {
		sh.mu.Lock()
		for _, seg := range sh.segs {
			if seg == nil || seg == sh.act {
				continue
			}
			seg.keyIdx.Store(nil)
			seg.keyBloom.Store(nil)
			if seg.keyIdxMem.Load() != nil {
				provisional++
			}
		}
		clear(sh.dir)
		sh.mu.Unlock()
	}
	if provisional == 0 {
		t.Fatal("no sealed segment carries a provisional index: the path under test is not reachable")
	}

	// Correctness AND that the fast path is the one taken. The linear-segment walk added for the
	// same window would find these keys too, so a read-back alone passes with the provisional
	// index ignored entirely; what separates them is whether the probe records a skip. Skips are
	// the cost this exists to remove -- three million of them in six hours on a real mirror.
	before := SealedProbesSkipped()
	found := 0
	for i := range n - 500 { // the tail may still be in the active segment, which sealed probes skip
		key := fmt.Sprintf("%d.0", i)
		ad, ok := c.Get([]byte(key))
		if !ok {
			continue // in the active segment, or otherwise not reachable by a sealed probe
		}
		found++
		if v, ok := ad.EvaluateAttrInt("ClusterId"); !ok || v != int64(i) {
			t.Fatalf("%s ClusterId = %d (present %v): the provisional index resolved the wrong record", key, v, ok)
		}
	}
	if found == 0 {
		t.Fatal("no key resolved through the sealed probe: the path under test was not exercised")
	}
	if got := SealedProbesSkipped() - before; got != 0 {
		t.Errorf("%d probe(s) fell back to walking a segment: the provisional index was not used", got)
	}
}
