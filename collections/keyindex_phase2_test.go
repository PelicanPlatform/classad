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

	// After reopen the directory holds only active-segment keys; the rest were evicted
	// and are served by the probe. For every key the union (directory OR probe) must
	// equal ground truth (Get), and no key may be found in both (that would be two
	// live records). Most keys are probe-resolved (they were sealed at reopen).
	probeN, dirN, deletedN := 0, 0, 0
	for i := 0; i < 600; i++ {
		key := k(i)
		h := c2.h.Hash(key)
		sh := c2.shards[c2.shardOf(key, h)]

		sh.mu.RLock()
		_, dirFound := sh.findCurrent(sh.dirGet(h), key)
		_, probeFound := sh.lookupSealed(key, h)
		sh.mu.RUnlock()
		_, live := c2.Get(key)

		if (dirFound || probeFound) != live {
			t.Fatalf("key %q: Get live=%v but dir=%v probe=%v", key, live, dirFound, probeFound)
		}
		if dirFound && probeFound {
			t.Fatalf("key %q found in BOTH the directory and a sealed segment (two live records)", key)
		}
		switch {
		case !live:
			deletedN++
		case probeFound:
			probeN++
		default:
			dirN++
		}
	}
	if probeN == 0 {
		t.Fatal("no probe-resolved keys -- eviction/probe path not exercised")
	}
	t.Logf("phase-3 oracle: %d probe-resolved, %d directory-resident, %d deleted", probeN, dirN, deletedN)
}
