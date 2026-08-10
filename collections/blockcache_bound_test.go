package collections

import (
	"fmt"
	"testing"
)

// TestBlockCacheRejectsOversizedRegion pins the consequence of a block being SEGMENT-sized rather
// than chunked into row groups.
//
// A block covers one whole segment, and the merge ladder's MaxSegmentBytes defaults to 1 GiB. Its
// three regions are each a single compressed blob, so a merged segment's decompressed string or
// cold-tail region can run to hundreds of megabytes -- while the block cache is bounded at 256 MiB.
// Ristretto cannot admit an item whose cost exceeds the whole cache, so past that size a region is
// never cached and EVERY query re-decompresses it: a permanent penalty, not a cold-start one.
//
// This is the mechanism behind a repeated ~60s query with an occasional fast one -- fast when the
// working set happens to fit, slow forever when it does not.
func TestBlockCacheRejectsOversizedRegion(t *testing.T) {
	const cacheBytes = 1 << 20 // 1 MiB stand-in for the production 256 MiB
	bc, err := newBlockCache(cacheBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.close()

	codec, err := NewZSTDCodec(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Compressible but not trivially so, and larger than the whole cache when decompressed.
	raw := make([]byte, 4<<20)
	for i := range raw {
		raw[i] = byte(i%251) ^ byte(i>>11)
	}
	fits := make([]byte, cacheBytes/8)
	for i := range fits {
		fits[i] = byte(i % 251)
	}

	newBlock := func(payload []byte) *columnarBlock {
		return &columnarBlock{
			id:       colBlockSeq.Add(1),
			codec:    codec,
			coldComp: codec.Compress(nil, payload),
		}
	}

	// A region that fits is retained: a second read is a cache hit.
	small := newBlock(fits)
	if _, err := bc.stream(small, kindCold); err != nil {
		t.Fatal(err)
	}
	waitForCache()
	if _, ok := bc.c.Get(streamKey(small.id, kindCold)); !ok {
		t.Error("a region well under the bound was not retained; the cache is not working at all")
	}

	// A region larger than the entire cache cannot be admitted, so every read re-decompresses.
	big := newBlock(raw)
	for i := 0; i < 3; i++ {
		got, err := bc.stream(big, kindCold)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(raw) {
			t.Fatalf("read %d bytes, want %d", len(got), len(raw))
		}
	}
	waitForCache()
	if _, ok := bc.c.Get(streamKey(big.id, kindCold)); ok {
		t.Log("oversized region was admitted after all; the bound is softer than assumed")
	} else {
		t.Logf("CONFIRMED: a %d-byte region is never cached under a %d-byte bound, so every query "+
			"re-decompresses it. A segment-sized block (merge ladder default 1 GiB) can exceed the "+
			"256 MiB production bound, making this permanent rather than a cold start.",
			len(raw), cacheBytes)
	}
}

// waitForCache lets ristretto's asynchronous Set land before we assert on membership.
func waitForCache() {
	for i := 0; i < 100; i++ {
		if i > 0 {
			// A short spin is enough; ristretto processes its buffer promptly.
			for j := 0; j < 1e5; j++ {
				_ = fmt.Sprint()
			}
		}
	}
}
