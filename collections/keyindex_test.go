package collections

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestKeyIndexSidecar validates phase 1 of the pageable primary index: after a clean
// Close builds each sealed segment's key sidecar, a reopen maps it, and (a) every
// record in a sealed segment is findable by its key-hash via the sidecar, and (b)
// every live key's current record is locatable via the sidecars (+ the active
// segment), matching the authoritative directory. The sidecar does not yet replace
// the directory; this proves it is correct and ready to.
func TestKeyIndexSidecar(t *testing.T) {
	dir := t.TempDir()
	open := func() *Collection {
		c, err := Open(Options{Dir: dir, Shards: 2, SegmentSize: 4096})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	k := func(i int) []byte { return []byte(fmt.Sprintf("k%05d", i)) }
	put := func(c *Collection, i, seq int) {
		ad, err := classad.Parse(fmt.Sprintf(`[ Seq=%d ]`, seq))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put(k(i), ad); err != nil {
			t.Fatal(err)
		}
	}

	c := open()
	for i := 0; i < 500; i++ {
		put(c, i, i)
	}
	for i := 0; i < 40; i++ { // tombstones
		c.Delete(k(i))
	}
	for i := 100; i < 160; i++ { // updates: supersede older versions (multi-version keys)
		put(c, i, i+1000)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2 := open()
	defer c2.Close()

	// (a) Every record in every sealed segment that carries a sidecar is findable by
	// its own key-hash, and the sidecar's coverage/count are exact.
	sealedWithIdx := 0
	for _, sh := range c2.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil || seg == sh.act {
				continue
			}
			ki := seg.keyIdx.Load()
			if ki == nil {
				continue
			}
			sealedWithIdx++
			if int(ki.upto) != seg.used {
				t.Fatalf("sidecar upto=%d, seg.used=%d", ki.upto, seg.used)
			}
			recs := 0
			for off := 0; off < seg.used; {
				o := uint32(off)
				total := recTotalLen(seg.data, o)
				if total == 0 {
					break
				}
				recs++
				offs := ki.lookup(c2.h.Hash(recKey(seg.data, o)))
				if !containsOff(offs, o) {
					t.Fatalf("record at off %d (key %q) missing from key sidecar", o, recKey(seg.data, o))
				}
				off += int(total)
			}
			if int(ki.count) != recs {
				t.Fatalf("sidecar count=%d, actual records=%d", ki.count, recs)
			}
		}
		sh.mu.RUnlock()
	}
	if sealedWithIdx == 0 {
		t.Fatal("no sealed segment carried a key sidecar -- test is ineffective")
	}
	t.Logf("validated key sidecars on %d sealed segments", sealedWithIdx)

	// (b) Every live key's current record is locatable via the sidecars + active
	// segment, and deleted keys are not.
	for i := 0; i < 500; i++ {
		key := k(i)
		_, live := c2.Get(key)
		found := locateCurrentRecord(c2, key)
		if live != found {
			t.Fatalf("key %q: Get live=%v but sidecar-locate found=%v", key, live, found)
		}
	}
}

func containsOff(offs []uint32, o uint32) bool {
	for _, x := range offs {
		if x == o {
			return true
		}
	}
	return false
}

// locateCurrentRecord finds key's current (non-superseded) record within its shard,
// using each sealed segment's key sidecar where present and a linear scan of a
// sidecar-less (active) segment otherwise -- the phase-2 lookup path, exercised here
// as a validation.
func locateCurrentRecord(c *Collection, key []byte) bool {
	h := c.h.Hash(key)
	sh := c.shards[c.shardOf(key, h)]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	for _, seg := range sh.segs {
		if seg == nil {
			continue
		}
		var offs []uint32
		if ki := seg.keyIdx.Load(); ki != nil {
			offs = ki.lookup(h)
		} else {
			for off := 0; off < seg.used; {
				o := uint32(off)
				total := recTotalLen(seg.data, o)
				if total == 0 {
					break
				}
				offs = append(offs, o)
				off += int(total)
			}
		}
		for _, o := range offs {
			if recSuperseded(seg.data, o) == seqMax && bytes.Equal(recKey(seg.data, o), key) {
				return true
			}
		}
	}
	return false
}
