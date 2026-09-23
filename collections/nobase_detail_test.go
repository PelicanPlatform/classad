package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// A chain with no whole record has three unrelated causes, and production could not tell them
// apart: 1,919 refusals over nine hours, across 320 distinct keys out of 345 -- so damage is
// being created continuously rather than a fixed set being retried, and the reason had to say
// WHICH creation path.
//
// The one that matters most is delta-index-pending, because it is TRANSIENT: a sealed segment's
// key index is built by a reindex pass rather than at seal time, so for a window after a seal
// the probe cannot see that segment's records at all. A refusal in that window would have
// succeeded on a retry, and refusals are currently reported as non-retryable.
func TestNoBaseNamesWhichFaultItWas(t *testing.T) {
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
	// Push the base into a sealed segment, then patch so the current record is a delta whose
	// base lives behind a seal.
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

	// Two routes reach the base and BOTH have to miss for a chain to look baseless, which is
	// itself the finding: dropping only the sealed key index is not enough, because the bucket
	// chain still walks to the base. So cut the chain at the current record -- pointing it at a
	// segment id that does not exist, which is what a link into a reaped segment looks like --
	// and drop the sealed indexes.
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
		setRecNext(seg.data, head.off, loc{seg: 1 << 30, off: 0})
	}
	sh.mu.Unlock()
	if dropped == 0 {
		t.Fatal("no sealed segment: the base never left the active segment, so this proves nothing")
	}

	before := UnreadableBaseReasons()
	beforeSkipped := SealedProbesSkipped()
	res := patchTx(t, c, key, "CompletionDate", 1789838852, "Pad00")
	if !res.HasUnapplied() {
		t.Skip("the write still landed; the base was reachable another way")
	}

	reasons := UnreadableBaseReasons()
	got := ""
	for _, r := range []string{"delta-chain-broken", "delta-index-pending", "delta-no-base"} {
		if reasons[r] > before[r] {
			got = r
			break
		}
	}
	if got == "" {
		t.Fatalf("no no-base reason moved; reasons = %v", reasons)
	}
	if got != "delta-chain-broken" {
		t.Errorf("reason = %s, want delta-chain-broken: the walk truncated at a dead link; reasons = %v", got, reasons)
	}
	if SealedProbesSkipped() <= beforeSkipped {
		t.Error("no sealed probe was counted as skipped, so that half of the state was not exercised")
	}
	versions, chainBroken, sealedSkipped := LastNoBaseDetail()
	if versions == 0 && !chainBroken && sealedSkipped == 0 {
		t.Error("the sample is empty: nothing recorded the shape of the failing chain")
	}
	t.Logf("reason=%s versions=%d chainBroken=%v sealedSkipped=%d", got, versions, chainBroken, sealedSkipped)
}
