package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// A read that fails while merging a delta CHAIN must be classified apart from one that fails on
// the record it started at. Both go through segStoredOrReassembled, and reporting them alike is
// what left production saying only "the chain did not materialize" -- true of six different
// faults with six different repairs.
//
// Here the current record is a delta in the intact active segment, and the whole record it
// merges onto is in a sealed segment whose columnar payload is gone. The read therefore gets
// past its own record and fails on a CHAIN PARTICIPANT, which is delta-reassemble, not
// reassemble.
func TestDeltaChainFailuresAreClassifiedSeparately(t *testing.T) {
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
	// Push the base out of the active segment, so the chain spans a seal.
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
	// Now patch, so the CURRENT record is a delta in the active segment.
	patch := classad.New()
	patch.InsertAttr("JobStatus", int64(2))
	w := c.Begin()
	w.PatchAttrs([]byte(key), patch, nil)
	if r := w.Commit(); r.Conflicted() || r.HasUnapplied() {
		t.Fatal("patch did not land")
	}

	h := c.h.Hash([]byte(key))
	sh := c.shards[c.shardOf([]byte(key), h)]
	sh.mu.Lock()
	act := sh.act
	damaged := 0
	for _, seg := range sh.segs {
		if seg != nil && seg != act {
			seg.colDamaged.Store(true)
			damaged++
		}
	}
	sh.mu.Unlock()
	if damaged == 0 {
		t.Skip("no sealed segment: the chain does not span a seal, so there is nothing to damage")
	}

	beforeChain := UnreadableBaseReasons()["delta-reassemble"]
	beforeOwn := UnreadableBaseReasons()["reassemble"]

	res := patchTx(t, c, key, "CompletionDate", 1789838852, "Pad00")
	if !res.HasUnapplied() {
		t.Skip("the write was not refused; the chain still materialized")
	}

	if got := UnreadableBaseReasons()["delta-reassemble"] - beforeChain; got != 1 {
		t.Errorf("delta-reassemble moved by %d, want 1; reasons = %v", got, UnreadableBaseReasons())
	}
	if got := UnreadableBaseReasons()["reassemble"] - beforeOwn; got != 0 {
		t.Errorf("reassemble moved by %d: a chain-participant failure was counted as the record's own", got)
	}
	// The breakdown must still account for every refusal.
	var sum int64
	for _, n := range UnreadableBaseReasons() {
		sum += n
	}
	if sum != UnreadableBaseRefusals() {
		t.Errorf("per-reason counts sum to %d, total %d", sum, UnreadableBaseRefusals())
	}
}
