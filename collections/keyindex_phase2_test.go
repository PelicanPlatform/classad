package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestSealedProbeOracle validates phase 2: the Bloom-gated sealed-segment probe
// (lookupSealed) agrees exactly with the authoritative in-RAM directory. For every
// key, the current record found by the directory is reproduced by the probe when it
// lives in a sealed segment, the probe correctly misses when it lives in the active
// segment (which it does not scan), and both miss for a deleted key. This is the
// oracle check that lets phase 3 evict sealed keys from the directory and rely on the
// probe instead.
func TestSealedProbeOracle(t *testing.T) {
	dir := t.TempDir()
	open := func() *Collection {
		c, err := Open(Options{Dir: dir, Shards: 4, SegmentSize: 4096})
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
	for i := 0; i < 600; i++ {
		put(c, i, i)
	}
	for i := 0; i < 50; i++ {
		c.Delete(k(i))
	}
	for i := 200; i < 280; i++ { // updates: supersede older versions
		put(c, i, i+10000)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2 := open()
	defer c2.Close()

	sealedN, activeN, deletedN := 0, 0, 0
	for i := 0; i < 600; i++ {
		key := k(i)
		h := c2.h.Hash(key)
		sh := c2.shards[c2.shardOf(key, h)]

		sh.mu.RLock()
		dirLoc, dirFound := sh.findCurrent(sh.dirGet(h), key)
		probeLoc, probeFound := sh.lookupSealed(key, h)
		act := sh.act
		var dirSeg *segment
		if dirFound {
			dirSeg = sh.segs[dirLoc.seg]
		}
		sh.mu.RUnlock()

		switch {
		case !dirFound:
			if probeFound {
				t.Fatalf("key %q absent in directory but the probe found it at %v", key, probeLoc)
			}
			deletedN++
		case dirSeg == act:
			// Current record is in the active segment; the sealed-only probe must miss.
			if probeFound {
				t.Fatalf("key %q current in the active segment but the probe found it at %v", key, probeLoc)
			}
			activeN++
		default:
			// Current record is in a sealed segment; the probe must find it there.
			if !probeFound || probeLoc != dirLoc {
				t.Fatalf("key %q current at sealed %v: probe found=%v loc=%v", key, dirLoc, probeFound, probeLoc)
			}
			sealedN++
		}
	}
	if sealedN == 0 {
		t.Fatal("no sealed-resident keys were checked -- test is ineffective")
	}
	t.Logf("oracle validated: %d sealed, %d active, %d deleted keys", sealedN, activeN, deletedN)
}
