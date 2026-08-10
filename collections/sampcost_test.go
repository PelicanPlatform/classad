package collections

import (
	"fmt"
	"testing"
)

// What does each sampling strategy COST to run? Training content quality is one axis; the sampling
// pass is a periodic maintenance operation with its own price, and the two get conflated.
//
// The asymmetry that matters: region sampling must DECOMPRESS a whole region to take a chunk out of
// it. At the production ~1 MiB group a region is several hundred KB and regionChunk is 4 KB, so the
// read amplification is large. Record sampling decompresses only the records it keeps.
func sampCostFixture(tb testing.TB) *Collection {
	tb.Helper()
	ads := realOSPoolAds(tb, 20000)
	c := New(Options{Shards: 1, SegmentSize: 1 << 20})
	for i, ad := range ads {
		if err := c.Put([]byte(fmt.Sprintf("slot%d", i)), ad); err != nil {
			tb.Fatal(err)
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 8) {
		tb.Skip("no sealed segments")
	}
	return c
}

func BenchmarkSampleCost(b *testing.B) {
	c := sampCostFixture(b)
	b.Run("records/CollectSamples2000", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if s := c.CollectSamples(2000); len(s) == 0 {
				b.Fatal("no samples")
			}
		}
	})
	b.Run("records/CollectSamplesRecent", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if s := c.CollectSamplesRecent(0, 0, 0); len(s) == 0 {
				b.Fatal("no samples")
			}
		}
	})
	b.Run("regions/CollectRegionSamples112K", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if s := c.CollectRegionSamples(DefaultDictSize, 0); len(s) == 0 {
				b.Fatal("no samples")
			}
		}
	})
	// How many bytes does each actually deliver as training content, and at what decompression cost?
	recs := c.CollectSamples(2000)
	regs := c.CollectRegionSamples(DefaultDictSize, 0)
	rb, gb := 0, 0
	for _, s := range recs {
		rb += len(s)
	}
	for _, s := range regs {
		gb += len(s)
	}
	decompressed := 0
	for _, blk := range c.sampleableBlocks(0) {
		for _, k := range []streamKind{kindColdNum, kindStr, kindCold} {
			if raw, err := blk.regionRaw(k); err == nil {
				decompressed += len(raw)
			}
		}
	}
	b.Logf("records: %d samples, %d B content", len(recs), rb)
	b.Logf("regions: %d samples, %d B content, but ~%d B decompressed to produce it (%.0fx amplification)",
		len(regs), gb, decompressed, float64(decompressed)/float64(gb))
}
