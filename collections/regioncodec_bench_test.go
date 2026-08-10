package collections

import (
	"fmt"
	"testing"
)

// What routing the block regions through a dictionary-less codec actually buys, end to end, on the
// same store: the blocks are built twice over identical data, once with the collection's trained
// dictionary codec (the old behaviour) and once with the region codec (the new one), and the same
// aggregate query is timed against each.
//
// Cold, because that is the regime the block cache leaves for anything larger than it can hold and
// the regime an escaped-value read pays repeatedly.
func BenchmarkRegionCodecAggregate(b *testing.B) {
	ads := realOSPoolAds(b, 20000)
	c := New(Options{Shards: 1, SegmentSize: 1 << 21})
	defer c.Close()
	for i, ad := range ads {
		if err := c.Put([]byte(fmt.Sprintf("s%d", i)), ad); err != nil {
			b.Fatal(err)
		}
	}
	if !c.BuildAndEnableSchemaScan(2000, 8) {
		b.Skip("no sealed segments")
	}
	if _, err := c.RetrainDict(2000); err != nil {
		b.Skipf("retrain declined: %v", err)
	}
	dictCodec := c.currentCodec()
	if dictCodec.Name() != "zstd+dict" {
		b.Skipf("write codec is %q, not a dictionary codec", dictCodec.Name())
	}
	st := c.schemaScan.Load()
	if st == nil {
		b.Fatal("schema scan not enabled")
	}

	// rebuild re-encodes every sealed segment's blocks with the given REGION codec, and reports the
	// total stored region bytes so the size side of the trade is measured on the same data.
	rebuild := func(regionCodec Codec) int {
		bytes := 0
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
				bl, offs := buildColumnarFromSegment(seg.data, seg.used, seg.codec, regionCodec,
					st.schema, st.hot, defaultColGrouping(),
					func(dst, w []byte) ([]byte, bool) { return c.recordToInternedDict(d, dst, w) })
				seg.colblk.Store(&colSegment{blocks: bl, offs: offs})
				for _, blk := range bl {
					bytes += len(blk.coldNumComp) + len(blk.strComp) + len(blk.coldComp)
				}
			}
		}
		return bytes
	}

	// Arena bytes as stored, so the region delta can be read as a fraction of what the collection
	// actually occupies rather than of the regions alone.
	arena := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil {
				arena += seg.used
			}
		}
		sh.mu.RUnlock()
	}
	b.Logf("arena %d B stored", arena)

	for _, regime := range []struct {
		name string
		cd   Codec
	}{{"dictRegions", dictCodec}, {"plainRegions", c.regionCodec()}} {
		regionBytes := rebuild(regime.cd)
		b.Run(regime.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(float64(regionBytes), "regionB")
			b.ReportMetric(100*float64(regionBytes)/float64(arena+regionBytes), "pctOfTotal")
			for i := 0; i < b.N; i++ {
				// Fresh cache each iteration: measure decompression, not cache hits.
				bc, err := newBlockCache(256 << 20)
				if err != nil {
					b.Fatal(err)
				}
				c.schemaScan.Store(&schemaScanState{schema: st.schema, hot: st.hot, cache: bc})
				got, ok := c.NumStatsQuery(nil, "Memory")
				if !ok || got.N == 0 {
					b.Fatal("NumStatsQuery declined or scanned nothing")
				}
			}
		})
	}
}
