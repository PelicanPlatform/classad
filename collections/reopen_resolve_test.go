package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// After a restart, can a by-key read still resolve a key held in a COLUMNARIZED segment?
//
// #229 reconciled the key index and directory at the columnarize swap, in-process. Reopen is a
// different path: persist.go runs Reindex(), which rebuilds indexes by decoding records -- and a
// columnarized segment's records keep only what the schema does not cover. If that rebuild
// mishandled them, keys would stay unresolvable after a restart, which is exactly the condition
// that makes a patch write unable to read its base and (before the refusal) store a fragment.
//
// It PASSES, and it is kept for that reason: the combination was recorded as unverified while a
// production mirror was showing fragment rows shortly after a restart, and this rules the reopen
// path out as their cause. A test that documents a refuted hypothesis is worth its runtime when
// the hypothesis is one somebody will have again.
func TestKeyResolvesAfterReopenOfColumnarizedSegment(t *testing.T) {
	dir := t.TempDir()
	open := func() *Collection {
		c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 12, ColumnarSegmentBudget: 1 << 20, DeltaMax: 16})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	const n = 3000
	for i := 0; i < n; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttr("ProcId", 0)
		ad.InsertAttr("JobStatus", int64(2))
		for j := 0; j < 30; j++ {
			ad.InsertAttr(fmt.Sprintf("Pad%02d", j), int64(j))
		}
		tx := c.Begin()
		tx.Put([]byte(fmt.Sprintf("%d.0", i)), ad)
		if r := tx.Commit(); r.Conflicted() {
			t.Fatalf("seed %d conflicted", i)
		}
	}
	// Columnarize, the way the server's background maintenance does.
	sch, hot, ok := c.deriveSchema(4096, 12)
	if !ok || !c.installSchemaScan(sch, hot) {
		t.Fatal("could not derive/install a schema: the reopen path under test needs a columnarized segment")
	}
	c.EnableSchemaScan(sch, hot)
	st := c.Stats()
	t.Logf("before columnarize: %d ads, %d segments, used %d", st.Ads, st.Segments, st.UsedBytes)
	nc := c.ColumnarizeSealed()
	if nc == 0 {
		t.Fatal("no sealed segment was columnarized: this test would assert nothing")
	}
	t.Logf("columnarized %d sealed segments", nc)
	c.Compact()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2 := open()
	defer c2.Close()

	missing := 0
	for i := 0; i < n; i++ {
		key := []byte(fmt.Sprintf("%d.0", i))
		if _, ok := c2.Get(key); !ok {
			missing++
			continue
		}
	}
	if missing > 0 {
		t.Errorf("after reopen, %d/%d keys do not resolve by key", missing, n)
	}

	// The condition that matters for the fragment bug: a patch write must be able to read its
	// base. Refused writes surface as conflicts (see encodePatchOnly).
	refusedBefore := UnreadableBaseRefusals()
	conflicts := 0
	for i := 0; i < n; i++ {
		patch := classad.New()
		patch.InsertAttr("CompletionDate", int64(1789838852))
		tx := c2.Begin()
		tx.PatchAttrs([]byte(fmt.Sprintf("%d.0", i)), patch, []string{"Pad00"}) // removal ⇒ reads the base
		if r := tx.Commit(); r.Conflicted() {
			conflicts++
		}
	}
	if conflicts > 0 {
		t.Errorf("%d/%d patch writes could not read their base after reopen (refusals %d)",
			conflicts, n, UnreadableBaseRefusals()-refusedBefore)
	}
	// And nothing became a fragment.
	frag := 0
	for i := 0; i < n; i++ {
		ad, ok := c2.Get([]byte(fmt.Sprintf("%d.0", i)))
		if !ok {
			continue
		}
		if _, has := ad.EvaluateAttrInt("ClusterId"); !has {
			frag++
		}
	}
	if frag > 0 {
		t.Errorf("%d rows lost ClusterId: fragments", frag)
	}
}
