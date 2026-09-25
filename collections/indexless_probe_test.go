package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// A sealed segment's key index is built by a reindex pass, not at seal time, so there is a window
// after every seal with no index to probe. Skipping the segment made the probe silently blind for
// that window: a chain whose base lived there could not be found, and because a refusal is
// terminal the write was dropped -- when a retry moments later would have succeeded.
//
// Production ran at 14 of those in 98 minutes once the real stranding was fixed, every one of
// them an update lost to a transient condition. The probe must read the segment instead.
func TestProbeFindsAKeyInAnIndexlessSegment(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()

	const key = "42.0"
	full := classad.New()
	full.InsertAttr("ClusterId", int64(42))
	full.InsertAttr("JobStatus", int64(1))
	for i := range 20 {
		full.InsertAttr(fmt.Sprintf("Pad%02d", i), int64(i))
	}
	tx := c.Begin()
	tx.Put([]byte(key), full)
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("seed conflicted")
	}
	// Push the base behind a seal, then patch so the current record is a delta whose base lives
	// in a sealed segment.
	for i := range 2000 {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(1000+i))
		for j := range 20 {
			ad.InsertAttr(fmt.Sprintf("Pad%02d", j), int64(j))
		}
		w := c.Begin()
		w.Put([]byte(fmt.Sprintf("%d.0", 1000+i)), ad)
		w.Commit()
	}
	p := classad.New()
	p.InsertAttr("JobStatus", int64(2))
	w := c.Begin()
	w.PatchAttrs([]byte(key), p, nil)
	if r := w.Commit(); r.Conflicted() || r.HasUnapplied() {
		t.Fatal("patch did not land")
	}

	h := c.h.Hash([]byte(key))
	sh := c.shards[c.shardOf([]byte(key), h)]

	// Reproduce the post-seal window: no key index on any sealed segment. Also cut the bucket
	// chain, because that route reaches the base independently and would mask the blindness --
	// which is exactly how an earlier version of this test passed while proving nothing.
	sh.mu.Lock()
	dropped := 0
	for _, seg := range sh.segs {
		if seg != nil && seg != sh.act {
			seg.keyIdx.Store(nil)
			seg.keyBloom.Store(nil)
			dropped++
		}
	}
	head := sh.dirGet(h)
	if seg := sh.segForLoc(head); seg != nil {
		setRecNext(seg.data, head.off, noLoc)
	}
	sh.mu.Unlock()
	if dropped == 0 {
		t.Fatal("no sealed segment: the base never left the active segment")
	}

	// The sealed probe must still find the base, so the chain materializes and the write lands.
	res := patchTx(t, c, key, "CompletionDate", 1789838852, "Pad00")
	if res.HasUnapplied() {
		t.Fatalf("the write was refused while a sealed segment had no key index; reasons = %v",
			UnreadableBaseReasons())
	}
	ad, ok := c.Get([]byte(key))
	if !ok {
		t.Fatal("the key is unreadable while a sealed segment has no key index")
	}
	if v, ok := ad.EvaluateAttrInt("ClusterId"); !ok || v != 42 {
		t.Errorf("ClusterId = %d (present %v): the base was not found", v, ok)
	}
}
