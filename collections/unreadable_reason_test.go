package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestUnreadableBaseReasonIsClassified drives a REAL unreadable base -- a segment whose columnar
// payload cannot be reassembled -- rather than the readStoredMissHook seam, because the seam can
// only ever report the reason it is written to report. The refusal must land under the specific
// reason, and the per-reason breakdown must account for every refusal the total counts: a
// production mirror refusing writes needs to know which of these five conditions it is hitting,
// and a breakdown that silently drops a case sends the investigation somewhere else.
func TestUnreadableBaseReasonIsClassified(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()

	full := classad.New()
	full.InsertAttr("ClusterId", int64(42))
	full.InsertAttr("ProcId", int64(0))
	full.InsertAttr("JobStatus", int64(2))
	for i := 0; i < 20; i++ {
		full.InsertAttr(fmt.Sprintf("Pad%02d", i), int64(i))
	}
	tx := c.Begin()
	tx.Put([]byte("42.0"), full)
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("seed conflicted")
	}

	// Mark the segments holding the key as having lost their columnar payload. The record stays
	// present and locatable -- keyExists still finds it, which is exactly the shape that makes
	// this a refusal rather than a create -- but its content can no longer be materialized.
	key := []byte("42.0")
	h := c.h.Hash(key)
	sh := c.shards[c.shardOf(key, h)]
	sh.mu.Lock()
	for _, seg := range sh.segs {
		if seg != nil {
			seg.colDamaged.Store(true)
		}
	}
	sh.mu.Unlock()
	if !c.keyExists(key, h) {
		t.Fatal("key must still be present, or this exercises the create path instead")
	}

	beforeTotal := UnreadableBaseRefusals()
	beforeReason := UnreadableBaseReasons()["reassemble"]

	res := patchTx(t, c, "42.0", "CompletionDate", 1789838852, "Pad00")
	if !res.HasUnapplied() {
		t.Fatal("the write was not refused, so nothing was classified")
	}

	if got := UnreadableBaseRefusals() - beforeTotal; got != 1 {
		t.Fatalf("refusal total moved by %d, want 1", got)
	}
	if got := UnreadableBaseReasons()["reassemble"] - beforeReason; got != 1 {
		t.Errorf("reason %q moved by %d, want 1; reasons = %v", "reassemble", got, UnreadableBaseReasons())
	}

	// The breakdown must sum to the total, or a reason is being counted in the wrong bucket.
	var sum int64
	for _, n := range UnreadableBaseReasons() {
		sum += n
	}
	if sum != UnreadableBaseRefusals() {
		t.Errorf("per-reason counts sum to %d, total refusals %d: a refusal is unaccounted for", sum, UnreadableBaseRefusals())
	}
}
