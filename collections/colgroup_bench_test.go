package collections

import (
	"fmt"
	"testing"
)

// What the row-group policy costs on the REAL store aggregate path, over TWO ad shapes.
//
// The mechanism: reading a value that ESCAPED its fixed slot means decompressing its block's cold
// tail. With one block per segment that is the whole segment's tail to read one record; with row
// groups it is only that record's group. So the cost scales with the block, and bounding the block
// bounds the cost.
//
// Splitting is not free: each block is one decompress call and one cache entry per scan. So BOTH ad
// shapes are measured -- fat ads (real OSPool slot ads, ~561 attributes, ~10 KB) and small ads (5
// attributes, tens of bytes) -- because the same row count means ~1 MiB of block on one and a few
// kilobytes on the other, a 300x spread in what a "128-row group" actually is.
//
// MEASURED (medians of 3, 8 MiB segments): on fat ads, bounding the block takes a cold aggregate
// from 4.02ms to 3.06ms and the decompression component (cold minus warm) from 1.32ms to 0.36ms.
// On small ads the group size is a NON-EFFECT: 128-row groups land within ~3% of whole-segment
// blocks, which is inside the run-to-run noise. Warm is flat on both.
//
// Beware two traps here, both of which produced wrong conclusions during this work. (1) ORDER: the
// regimes run sequentially against one store, and the first one measured absorbs warmup -- an
// earlier version of this benchmark reported a 57% small-ad regression that was entirely the cost
// of running first, and vanished under -count. Compare medians across -count>=3, never single runs.
// (2) SEGMENT SIZE: if a segment holds fewer records than the group size being measured, that
// regime silently equals whole-segment, and two "different" rows of the table are the same
// measurement. Check the rec/blk metric before believing any comparison.
//
// The cold variants build a FRESH cache per iteration. That is not pessimism, it is the regime a
// segment-sized block actually lives in: at ~8867 B/ad of regions a block past ~30k records exceeds
// the cache's entire 256 MiB budget, is never admitted, and re-decompresses on every read forever.
// Always-cold IS its steady state.

// groupPolicy is one measured regime.
type groupPolicy struct {
	name string
	g    colGrouping
}

// benchPolicies spans the production default, a row-count sweep, and a group larger than any
// segment in these fixtures -- which reproduces the old one-block-per-segment shape.
var benchPolicies = []groupPolicy{
	{"default", defaultColGrouping()},
	{"rows128", byRows(128)},
	{"rows512", byRows(512)},
	{"wholeSegment", byRows(1 << 20)},
}

// coldCache swaps in a fresh block cache, so the next query pays full decompression.
func coldCache(tb testing.TB, c *Collection) {
	tb.Helper()
	st := c.schemaScan.Load()
	if st == nil {
		tb.Fatal("schema scan not enabled")
	}
	bc, err := newBlockCache(256 << 20)
	if err != nil {
		tb.Fatal(err)
	}
	c.schemaScan.Store(&schemaScanState{schema: st.schema, hot: st.hot, cache: bc})
}

// regroup rebuilds every sealed segment's accelerator under g, and reports the resulting block
// count and mean records per block -- so a regime that produced only one block per segment cannot be
// mistaken for one that split.
func regroup(tb testing.TB, c *Collection, g colGrouping) (blocks, recs int) {
	tb.Helper()
	st := c.schemaScan.Load()
	if st == nil {
		tb.Fatal("schema scan not enabled")
	}
	for _, sh := range c.shards {
		sh.mu.RLock()
		act := sh.act
		segs := append([]*segment(nil), sh.segs...)
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil || seg == act || seg.used == 0 {
				continue
			}
			d := seg.dict.Load()
			bl, offs := buildColumnarFromSegment(seg.data, seg.used, seg.codec, c.regionCodec(), st.schema, st.hot, g,
				func(dst, w []byte) ([]byte, bool) { return c.recordToInternedDict(d, dst, w) })
			seg.colblk.Store(&colSegment{blocks: bl, offs: offs})
			blocks += len(bl)
			recs += len(offs)
		}
	}
	if blocks == 0 {
		tb.Fatal("regrouped no segment: there is no columnar path to measure")
	}
	return blocks, recs
}

// ospoolFixture loads the real OSPool slot corpus into a store with sealed segments -- ~561
// attributes per ad, which is the shape the row-group measurements were taken on.
func ospoolFixture(tb testing.TB) (*Collection, string) {
	tb.Helper()
	ads, _ := loadOSPoolAds(tb)
	if len(ads) == 0 {
		tb.Skip("no OSPool ads")
	}
	// 8 MiB segments over ~10 KB ads: ~800 records per segment, so a whole-segment block holds
	// several times the byte budget and the policies are far apart. (At 2 MiB a segment is only
	// ~208 records, which already caps the row-count regimes and understates the difference.)
	store := New(Options{Shards: 1, SegmentSize: 1 << 23})
	for i, ad := range ads {
		if err := store.Put([]byte(fmt.Sprintf("s%d", i)), ad); err != nil {
			tb.Fatal(err)
		}
	}
	// Drive demand on Memory so it lands in the hot tier, as a maintenance pass would.
	if !store.BuildAndEnableSchemaScan(2000, 8) {
		tb.Skip("no schema derived from the OSPool corpus")
	}
	if info := store.SchemaScanInfo(); info.CoveredSegments == 0 {
		tb.Skipf("accelerator covers no sealed segment (%d sealed)", info.SealedSegments)
	}
	return store, "Memory"
}

// benchAggregate runs the aggregate query over each policy, cold and warm.
func benchAggregate(b *testing.B, newStore func(testing.TB) (*Collection, string)) {
	store, attr := newStore(b)
	if _, ok := store.NumStatsQuery(nil, attr); !ok {
		b.Skipf("NumStatsQuery declined for %s; the columnar path is not serving it", attr)
	}
	for _, p := range benchPolicies {
		blocks, recs := regroup(b, store, p.g)
		perBlock := float64(recs) / float64(blocks)
		b.Run(p.name+"/cold", func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(float64(blocks), "blocks")
			b.ReportMetric(perBlock, "rec/blk")
			for i := 0; i < b.N; i++ {
				coldCache(b, store)
				st, ok := store.NumStatsQuery(nil, attr)
				if !ok {
					b.Fatal("NumStatsQuery declined mid-benchmark")
				}
				if st.N == 0 {
					b.Fatal("no values scanned")
				}
			}
		})
		coldCache(b, store) // one fresh cache, then let it warm
		b.Run(p.name+"/warm", func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(perBlock, "rec/blk")
			for i := 0; i < b.N; i++ {
				if _, ok := store.NumStatsQuery(nil, attr); !ok {
					b.Fatal("NumStatsQuery declined mid-benchmark")
				}
			}
		})
	}
}

// BenchmarkRowGroupOSPool is the fat-ad case: real slot ads, where bounding the block is the win.
func BenchmarkRowGroupOSPool(b *testing.B) {
	benchAggregate(b, func(tb testing.TB) (*Collection, string) { return ospoolFixture(tb) })
}

// BenchmarkRowGroupSmallAds is the small-ad case: blocks so small that splitting them changes
// nothing measurable, which is why the budget is in bytes rather than rows -- not because rows are
// slower here, but because a row count means something entirely different at this ad width.
func BenchmarkRowGroupSmallAds(b *testing.B) {
	benchAggregate(b, func(tb testing.TB) (*Collection, string) {
		c, _ := groupFixture(tb, 8000)
		return c, "ProcId"
	})
}

// BenchmarkRowGroupBuild measures the transcode: each group compresses independently, so it is
// worth knowing whether seal-time cost moved.
func BenchmarkRowGroupBuild(b *testing.B) {
	store, _ := ospoolFixture(b)
	st := store.schemaScan.Load()
	var seg *segment
	for _, sh := range store.shards {
		sh.mu.RLock()
		for _, s := range sh.segs {
			if s != nil && s != sh.act && s.used > 0 {
				seg = s
			}
		}
		sh.mu.RUnlock()
	}
	if seg == nil {
		b.Fatal("no sealed segment")
	}
	d := seg.dict.Load()
	for _, p := range benchPolicies {
		b.Run(p.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				blocks, _ := buildColumnarFromSegment(seg.data, seg.used, seg.codec, store.regionCodec(), st.schema, st.hot, p.g,
					func(dst, w []byte) ([]byte, bool) { return store.recordToInternedDict(d, dst, w) })
				if len(blocks) == 0 {
					b.Fatal("no blocks")
				}
			}
		})
	}
}

// BenchmarkRowGroupBlockBytes is a size report, not a timing: what splitting costs on disk.
// Compressing each group independently gives up cross-group redundancy, the one axis where a
// smaller group is genuinely worse.
func BenchmarkRowGroupBlockBytes(b *testing.B) {
	for _, fx := range []struct {
		name  string
		build func(testing.TB) (*Collection, string)
	}{
		{"ospool", func(tb testing.TB) (*Collection, string) { return ospoolFixture(tb) }},
		{"smallAds", func(tb testing.TB) (*Collection, string) {
			c, _ := groupFixture(tb, 8000)
			return c, "ProcId"
		}},
	} {
		store, _ := fx.build(b)
		for _, p := range benchPolicies {
			regroup(b, store, p.g)
			bytes, blocks, recs := 0, 0, 0
			for _, sh := range store.shards {
				sh.mu.RLock()
				segs := append([]*segment(nil), sh.segs...)
				act := sh.act
				sh.mu.RUnlock()
				for _, seg := range segs {
					if seg == nil || seg == act || seg.used == 0 {
						continue
					}
					cs := seg.colblk.Load()
					if cs == nil {
						continue
					}
					for _, blk := range cs.blocks {
						bytes += len(blk.hot) + len(blk.coldNumComp) + len(blk.strComp) + len(blk.coldComp)
						blocks++
						recs += blk.n
					}
				}
			}
			if recs == 0 {
				b.Fatal("no records")
			}
			b.Logf("%-9s %-13s blocks=%-4d %5d records  %9d B  %.1f B/record  %.0f rec/blk",
				fx.name, p.name, blocks, recs, bytes, float64(bytes)/float64(recs),
				float64(recs)/float64(blocks))
		}
	}
}
