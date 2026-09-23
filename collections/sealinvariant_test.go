package collections

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// A healthy store has no live delta in a sealed segment -- that is the seal-collapse invariant,
// and every collapse pass is built on it. The check must agree on a store that is actually
// healthy, or its first production reading is noise.
func TestSealInvariantHoldsOnAHealthyStore(t *testing.T) {
	c, dir := openDelta(t, 16)
	for i := range 400 {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttr("JobStatus", int64(1))
		for j := range 20 {
			ad.InsertAttr(fmt.Sprintf("Pad%02d", j), int64(j))
		}
		tx := c.Begin()
		tx.Put([]byte(fmt.Sprintf("%d.0", i)), ad)
		tx.Commit()
	}
	for round := range 16 {
		for i := range 400 {
			p := classad.New()
			p.InsertAttr("JobStatus", int64(2+round))
			tx := c.Begin()
			tx.PatchAttrs([]byte(fmt.Sprintf("%d.0", i)), p, nil)
			tx.Commit()
		}
	}
	if d, _ := c.DeltaStats(); d == 0 {
		t.Fatal("no deltas written: the invariant is vacuous here and the test proves nothing")
	}
	// The fixture has to put SUPERSEDED deltas in sealed segments, or "no live delta there" is
	// indistinguishable from "no delta there", and the check passes without exercising the one
	// distinction it makes. Dropping the live-only condition survived this test until it did.
	superseded := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil || seg == sh.act || seg.used == 0 {
				continue
			}
			for off := uint32(0); off < uint32(seg.used); {
				tl := recTotalLen(seg.data, off)
				if tl == 0 || off+tl > uint32(seg.used) {
					break
				}
				if !recIsMarker(seg.data, off) && recSuperseded(seg.data, off) != seqMax && segRecIsDelta(seg, off) {
					superseded++
				}
				off += tl
			}
		}
		sh.mu.RUnlock()
	}
	if superseded == 0 {
		t.Fatal("no superseded delta in any sealed segment: the live-vs-superseded distinction is untested")
	}
	if got := c.checkSealCollapseInvariant(20); len(got) != 0 {
		t.Errorf("healthy store reports %d stranded sealed delta(s) among %d superseded: %q",
			len(got), superseded, got[0])
	}
	_ = dir
}

// And it must actually SEE a stranded fragment, or it is a check that can only ever say yes.
// Marking a live record in a sealed segment as a delta is precisely the on-disk state a collapse
// that never ran leaves behind, and the state no pass can subsequently find.
func TestSealInvariantDetectsAStrandedFragment(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()
	for i := range 2000 {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		for j := range 20 {
			ad.InsertAttr(fmt.Sprintf("Pad%02d", j), int64(j))
		}
		tx := c.Begin()
		tx.Put([]byte(fmt.Sprintf("%d.0", i)), ad)
		tx.Commit()
	}

	strandOne := func() bool {
		for _, sh := range c.shards {
			sh.mu.Lock()
			for _, seg := range sh.segs {
				if seg == nil || seg == sh.act || seg.used == 0 {
					continue
				}
				for off := uint32(0); off < uint32(seg.used); {
					tl := recTotalLen(seg.data, off)
					if tl == 0 || off+tl > uint32(seg.used) {
						break
					}
					if !recIsMarker(seg.data, off) && recSuperseded(seg.data, off) == seqMax {
						f := binary.LittleEndian.Uint32(seg.data[off+recKeyLenOff:])
						binary.LittleEndian.PutUint32(seg.data[off+recKeyLenOff:], f|deltaFlag)
						sh.mu.Unlock()
						return true
					}
					off += tl
				}
			}
			sh.mu.Unlock()
		}
		return false
	}
	if !strandOne() {
		t.Fatal("no live record in any sealed segment: nothing to strand, so this proves nothing")
	}

	got := c.checkSealCollapseInvariant(20)
	if len(got) == 0 {
		t.Fatal("the check missed a stranded fragment: it can only ever report healthy")
	}
	if StrandedSealedDeltas() == 0 {
		t.Error("the counter did not move")
	}
}
