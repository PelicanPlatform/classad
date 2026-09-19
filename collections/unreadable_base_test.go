package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// A patch write whose base cannot be read used to be stored against an EMPTY ad, so one
// transaction's changed attributes became the whole record. On a production mirror that turned a
// job-completion update into a row holding CompletionDate, LastRemoteHost and six siblings, with
// no ClusterId, no JobStatus and no Key -- the job's real ad, replaced by a fragment.
//
// The distinction that matters is WHY the read missed. A key that is genuinely absent is a
// create, and creating is correct. A key the store HOLDS but cannot resolve is a lookup miss, and
// storing the patch alone there is data loss.

// patchTx buffers a patch. removed makes the write delta-INELIGIBLE, which is what routes it
// down the whole-record composition path -- the only path that reads the base, and so the only
// one that can fabricate. An eligible patch is stored as a delta and never reads anything, which
// is why the first version of this test passed a plain patch and proved nothing.
func patchTx(t *testing.T, c *Collection, key string, attr string, v int64, removed ...string) CommitResult {
	t.Helper()
	patch := classad.New()
	patch.InsertAttr(attr, v)
	tx := c.Begin()
	tx.PatchAttrs([]byte(key), patch, removed)
	return tx.Commit()
}

// TestUnreadableBaseRefusesRatherThanFabricate is the regression: with the base unreadable but
// the key present, the write must not land, must be reported as a conflict, and must leave the
// stored ad whole.
func TestUnreadableBaseRefusesRatherThanFabricate(t *testing.T) {
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

	before := UnreadableBaseRefusals()
	readStoredMissHook = func(key []byte) bool { return string(key) == "42.0" }
	defer func() { readStoredMissHook = nil }()

	res := patchTx(t, c, "42.0", "CompletionDate", 1789838852, "Pad00")

	if !res.Conflicted() {
		t.Error("the write was accepted although its base could not be read")
	}
	if res.Committed != 0 {
		t.Errorf("Committed = %d, want 0", res.Committed)
	}
	if got := UnreadableBaseRefusals() - before; got != 1 {
		t.Errorf("UnreadableBaseRefusals rose by %d, want 1", got)
	}
	// The decisive assertion: the stored ad is untouched, not replaced by the patch.
	readStoredMissHook = nil
	ad, ok := c.Get([]byte("42.0"))
	if !ok {
		t.Fatal("42.0 vanished")
	}
	if n := len(ad.AST().Attributes); n != 23 {
		t.Errorf("42.0 has %d attributes, want the original 23 -- a fragment replaced the ad", n)
	}
	if v, ok := ad.EvaluateAttrInt("ClusterId"); !ok || v != 42 {
		t.Errorf("ClusterId = %v (ok=%v): identity lost", v, ok)
	}
	if _, ok := ad.EvaluateAttrInt("CompletionDate"); ok {
		t.Error("the refused patch was stored anyway")
	}
}

// TestAbsentKeyStillCreates: the other side of the distinction. A patch for a key the store does
// not hold is a create, and refusing it would break SetAttribute's documented behaviour.
func TestAbsentKeyStillCreates(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()
	before := UnreadableBaseRefusals()
	if res := patchTx(t, c, "99.0", "JobStatus", 1, "NoSuchAttr"); res.Conflicted() {
		t.Fatal("a patch to an absent key was refused; it should create")
	}
	ad, ok := c.Get([]byte("99.0"))
	if !ok {
		t.Fatal("99.0 was not created")
	}
	if v, ok := ad.EvaluateAttrInt("JobStatus"); !ok || v != 1 {
		t.Errorf("JobStatus = %v (ok=%v), want 1", v, ok)
	}
	if got := UnreadableBaseRefusals() - before; got != 0 {
		t.Errorf("a genuine create counted as an unreadable base (%d)", got)
	}
}
