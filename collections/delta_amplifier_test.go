package collections

import (
	"encoding/binary"
	"fmt"
	"math"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// classadNew builds a wide ad so the records are big enough to behave like real job ads.
func classadNew(t *testing.T, cluster int) *classad.ClassAd {
	t.Helper()
	a := classad.New()
	a.InsertAttr("ClusterId", int64(cluster))
	a.InsertAttr("JobStatus", int64(1))
	for j := range 20 {
		a.InsertAttr(fmt.Sprintf("Pad%02d", j), int64(j))
	}
	return a
}

// setRecDeltaForTest flags a record as a delta in its header.
func setRecDeltaForTest(seg *segment, off uint32) {
	f := binary.LittleEndian.Uint32(seg.data[off+recKeyLenOff:])
	binary.LittleEndian.PutUint32(seg.data[off+recKeyLenOff:], f|deltaFlag)
}

// A key whose collapse FAILED must not look freshly collapsed to the next write.
//
// collapseLiveChains resets every depth BEFORE the collapse runs, and absence means "depth 0,
// base exists" (delta.go's next). So without marking, a key that was not collapsed starts a new
// chain on a base that is not there, DeltaMax never bites because the count restarts at every
// pass, and the chain grows without bound -- 29 to 41 versions with no whole record among them
// on the production mirror this came from.
func TestFailedCollapseForcesTheNextWriteToCompose(t *testing.T) {
	tr := newDeltaTracker()
	const h = 0x1234
	const max = 16

	// A healthy key chains, as it should.
	if !tr.next(h, true, true, max) {
		t.Fatal("a healthy key did not chain; the tracker is not in the state this test assumes")
	}

	// Its collapse fails.
	tr.mustCompose(h)

	// The next write must compose a whole record rather than extend the chain...
	if tr.next(h, true, true, max) {
		t.Error("the write after a failed collapse extended the chain")
	}
	// ...and repeatedly, if it keeps failing: next() resets depth to 0 on the compose branch, so
	// re-marking is what keeps it from drifting back into chaining.
	tr.mustCompose(h)
	for i := range 3 {
		if tr.next(h, true, true, max) {
			t.Fatalf("write %d after a failed collapse extended the chain", i)
		}
		tr.mustCompose(h)
	}
}

// reset() must not resurrect a marked key, because reset is exactly what runs at the head of the
// next collapse pass.
func TestResetDoesNotUnmarkWhatThePassThenFails(t *testing.T) {
	tr := newDeltaTracker()
	const h = 0x5678
	tr.mustCompose(h)
	tr.reset()
	// After a reset the key looks fresh again -- that is reset's job -- so the pass that resets
	// must re-mark on failure. This pins the ordering contract collapseLiveChains relies on.
	if !tr.next(h, true, true, 16) {
		t.Error("reset did not clear the mark; collapseLiveChains' reset/collapse/mark order is not what this assumes")
	}
	tr.mustCompose(h)
	if tr.next(h, true, true, 16) {
		t.Error("re-marking after the failed collapse did not take effect")
	}
}

func TestMustComposeExceedsAnyBound(t *testing.T) {
	tr := newDeltaTracker()
	const h = 0x9abc
	tr.mustCompose(h)
	if tr.next(h, true, true, math.MaxInt-1) {
		t.Error("mustCompose did not exceed a very large DeltaMax")
	}
}

// And collapseLiveChains must actually apply the mark, which the tracker-level tests above do
// not prove -- deleting the call site leaves them all passing.
//
// This drives a collapse that genuinely fails: the key's chain is made baseless by flipping its
// whole record's delta flag, so materializeAt finds no base, collapseBatch exhausts its retries,
// and the key reaches the leftover path. Afterwards its next write must compose.
func TestCollapseMarksTheKeysItCouldNotCollapse(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()

	const key = "42.0"
	full := classadNew(t, 42)
	tx := c.Begin()
	tx.Put([]byte(key), full)
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("seed conflicted")
	}
	p := classadNew(t, 42)
	p.InsertAttr("JobStatus", int64(9))
	w := c.Begin()
	w.PatchAttrs([]byte(key), p, nil)
	if r := w.Commit(); r.Conflicted() || r.HasUnapplied() {
		t.Fatal("patch did not land")
	}

	h := c.h.Hash([]byte(key))
	sh := c.shards[c.shardOf([]byte(key), h)]

	// Make the chain baseless: flag the WHOLE record as a delta so no participant is a base.
	sh.mu.Lock()
	flipped := false
	for _, seg := range sh.segs {
		if seg == nil || seg.used == 0 {
			continue
		}
		for off := uint32(0); off < uint32(seg.used); {
			tl := recTotalLen(seg.data, off)
			if tl == 0 || off+tl > uint32(seg.used) {
				break
			}
			if !recIsMarker(seg.data, off) && string(recKey(seg.data, off)) == key && !segRecIsDelta(seg, off) {
				setRecDeltaForTest(seg, off)
				flipped = true
			}
			off += tl
		}
	}
	sh.mu.Unlock()
	if !flipped {
		t.Fatal("no whole record found for the key: the chain is not baseless, so the collapse would succeed")
	}
	if !c.endsInDelta([]byte(key)) {
		t.Fatal("the key does not end in a delta; collapseBatch would not reach the leftover path")
	}

	c.collapseLiveChains(false)

	// The next write must compose rather than extend a chain that has no base.
	if c.deltas.next(h, true, true, 16) {
		t.Error("after a failed collapse the next write still chained: the leftover was not marked")
	}
}
