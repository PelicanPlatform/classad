package collections

import "testing"

// TestColNativeSharesOneBlockCache is the regression for the per-segment block-cache leak: every
// columnarized segment must reference the collection's ONE shared cache, not a fresh cache of its
// own. A cache per segment made ristretto's fixed admission metadata (count-min sketch + bloom)
// scale with segment count -- gigabytes of live heap on a large archive that grew as segments
// sealed.
func TestColNativeSharesOneBlockCache(t *testing.T) {
	c, s, hot := columnarFixture(t, 6000) // SegmentSize 64KiB -> several sealed segments
	defer c.Close()

	sh := c.shards[0]
	sh.mu.Lock()
	act := sh.act
	var srcs []*segment
	for _, seg := range sh.segs {
		if seg != nil && seg != act && seg.used > 0 && !seg.columnarized() {
			srcs = append(srcs, seg)
		}
	}
	var caches []*blockCache
	var dsts []*segment
	for _, src := range srcs {
		dst, _, _ := c.columnarizeSegment(sh, src, s, hot)
		if dst == nil || !dst.columnarized() {
			continue
		}
		dsts = append(dsts, dst)
		caches = append(caches, dst.colNative.Load().cache)
	}
	sh.mu.Unlock()
	defer func() {
		for _, dst := range dsts {
			dst.retire()
			dst.reapAndHook()
		}
	}()

	if len(caches) < 2 {
		t.Skipf("need >=2 columnarized segments to prove sharing, got %d", len(caches))
	}
	if c.colCache == nil {
		t.Fatal("shared colCache was never created")
	}
	for i, bc := range caches {
		if bc != c.colCache {
			t.Errorf("columnarized segment %d has its own cache %p, want the shared %p", i, bc, c.colCache)
		}
	}
}
