package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// Dictionary training, revisited now that a block is a bounded ROW GROUP rather than a whole
// segment (see colGroupRows / colGroupTargetBytes).
//
// The block size is exactly what decides whether a dictionary can earn anything. A dictionary
// supplies the redundancy a payload cannot supply itself, so a big block -- which carries hundreds
// of similar records and compresses well unaided -- has little left for a dictionary to add, while a
// small one has plenty. Every earlier dictionary measurement here was taken when a block was a whole
// segment, so it answered the question for a block size that no longer exists.
//
// TWO METHODOLOGY FIXES over the earlier comparison (TestDictStrategiesRealOSPool), both of which
// flattered the dictionaries:
//
//  1. TRAIN/MEASURE SPLIT. That test trained on samples drawn from every block and then measured on
//     every block -- and for the "records" row, measured on the training samples themselves. A
//     dictionary scored against its own training bytes is not measuring compression, it is measuring
//     memorization. Here the OLDEST half of the segments trains and the NEWEST half is measured,
//     which is also what production actually does: train on history, apply to what arrives next.
//
//  2. GROUP SIZE SWEPT. Region samples are drawn from blocks at the SAME group size being measured,
//     so each row is self-consistent rather than sampling production-shaped blocks and applying the
//     result to a different shape.

// dictRegimes are the block shapes measured: two small row-count groups, the production byte budget,
// and one group per segment (the pre-row-group shape).
var dictRegimes = []struct {
	name string
	g    colGrouping
}{
	{"rows8", byRows(8)},
	{"rows32", byRows(32)},
	{"rows64", byRows(64)},
	{"default(~1MiB)", defaultColGrouping()},
}

// dictCorpus loads real OSPool ads into a store with many sealed segments.
//
// CORPUS LIMIT: the committed corpus is 1500 slot ads at ~10 KB, so ~15 MB total. Getting enough
// segments to hold out half for training caps a segment at ~1 MiB, i.e. ~100 ads -- which is
// coincidentally the production row-group size, since the byte budget is 1 MiB. So this sweep covers
// production-shaped blocks and SMALLER. The multi-megabyte block that one-block-per-segment used to
// produce cannot be reproduced on this corpus without duplicating ads, and duplication would
// manufacture exactly the cross-block redundancy the measurement is about.
func dictCorpus(t *testing.T) *Collection {
	t.Helper()
	ads := realOSPoolAds(t, 20000)
	c := New(Options{Shards: 1, SegmentSize: 1 << 20})
	for i, ad := range ads {
		if err := c.Put([]byte(fmt.Sprintf("slot%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []string{"Memory >= 0", "Cpus >= 0"} {
		q, err := vm.Parse(e)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 20; i++ {
			for range c.Query(q) {
			}
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to sample")
	}
	t.Logf("corpus: %d ads, %d sealed segments", len(ads), c.SchemaScanInfo().SealedSegments)
	return c
}

// sealedSegs returns the sealed segments oldest-first.
func sealedSegs(c *Collection) []*segment {
	var out []*segment
	for _, sh := range c.shards {
		sh.mu.RLock()
		act := sh.act
		for _, s := range sh.segs {
			if s != nil && s != act && s.used > 0 {
				out = append(out, s)
			}
		}
		sh.mu.RUnlock()
	}
	return out
}

// blocksAt builds the columnar blocks for segs at group size g, with an IDENTITY codec so the
// regions are stored raw -- the comparison then compresses those raw bytes with each candidate
// dictionary, instead of measuring a compression of a compression.
func blocksAt(t *testing.T, c *Collection, segs []*segment, g colGrouping) []*columnarBlock {
	t.Helper()
	st := c.schemaScan.Load()
	if st == nil {
		t.Fatal("schema scan not enabled")
	}
	var out []*columnarBlock
	for _, seg := range segs {
		d := seg.dict.Load()
		bl, _ := buildColumnarFromSegment(seg.data, seg.used, identityCodec{}, st.schema, st.hot, g,
			func(dst, w []byte) ([]byte, bool) { return c.recordToInternedDict(d, dst, w) })
		out = append(out, bl...)
	}
	return out
}

// regionsOf gathers one kind's raw regions from blocks.
func regionsOf(blocks []*columnarBlock, kind streamKind) [][]byte {
	var out [][]byte
	for _, b := range blocks {
		if raw, err := b.regionRaw(kind); err == nil && len(raw) > 0 {
			out = append(out, raw)
		}
	}
	return out
}

// TestDictStrategyNetBytes is the decision: TOTAL stored bytes under each training strategy, at the
// production group size, counting BOTH things the collection stores -- the per-record arena (the
// source of truth, compressed per record at ingest) and the columnar block regions (the derived
// accelerator in the sidecar).
//
// It exists because the per-region percentages alone point the wrong way. Region-trained
// dictionaries beat ad-trained ones on every region kind, so a per-region table reads as an
// argument for switching what production trains on. But one codec serves the whole collection, the
// arena is several times the bytes of the regions, and records ARE ads -- so an ad-trained
// dictionary is matched to the payload that dominates. Netting it out reverses the ranking.
func TestDictStrategyNetBytes(t *testing.T) {
	c := dictCorpus(t)
	defer c.Close()
	segs := sealedSegs(c)
	if len(segs) < 4 {
		t.Skipf("need several sealed segments, have %d", len(segs))
	}
	half := len(segs) / 2
	trainSegs, measSegs := segs[:half], segs[half:]

	mk := func(d []byte) Codec {
		cd, err := NewZSTDCodec(d)
		if err != nil {
			t.Fatal(err)
		}
		return cd
	}
	adDict, err := TrainDict(segmentRecordSamples(t, c, trainSegs, 2000))
	if err != nil {
		t.Fatal(err)
	}
	kinds := []streamKind{kindColdNum, kindStr, kindCold}
	trainBlocks := blocksAt(t, c, trainSegs, defaultColGrouping())
	var mixed [][]byte
	for _, k := range kinds {
		mixed = append(mixed, sampleKind(trainBlocks, k, DefaultDictSize/regionKinds)...)
	}
	regionDict, rerr := TrainDict(mixed)
	if rerr != nil {
		t.Skipf("region dict did not train: %v", rerr)
	}

	measBlocks := blocksAt(t, c, measSegs, defaultColGrouping())
	measRecs := segmentRecordSamples(t, c, measSegs, 2000)

	for _, strat := range []struct {
		name string
		cd   Codec
	}{{"none", mk(nil)}, {"ad-trained", mk(adDict)}, {"region-trained", mk(regionDict)}} {
		arena := 0
		for _, p := range measRecs {
			arena += len(strat.cd.Compress(nil, p))
		}
		regions := 0
		for _, k := range kinds {
			for _, p := range regionsOf(measBlocks, k) {
				regions += len(strat.cd.Compress(nil, p))
			}
		}
		t.Logf("%-15s arena=%9d  regions=%8d  TOTAL=%9d", strat.name, arena, regions, arena+regions)
	}
}

// TestDictStrategyByGroupSize is the measurement: for each block shape, how much does each
// dictionary-training strategy save on each region kind, trained on held-out data?
//
// Strategies:
//
//	none        no dictionary (the baseline every percentage is against)
//	ad          trained on whole ClassAd records -- what production trains on today
//	region      trained on region bytes, all three kinds mixed (CollectRegionSamples)
//	per-kind    trained on ONLY the kind being compressed -- the candidate improvement, which needs
//	            a codec per stream kind rather than one codec per collection
func TestDictStrategyByGroupSize(t *testing.T) {
	c := dictCorpus(t)
	defer c.Close()

	segs := sealedSegs(c)
	if len(segs) < 4 {
		t.Skipf("need several sealed segments to split train/measure, have %d", len(segs))
	}
	half := len(segs) / 2
	trainSegs, measSegs := segs[:half], segs[half:]
	t.Logf("train on %d oldest segment(s), measure on %d newest", len(trainSegs), len(measSegs))

	// Ad-record samples come from the TRAIN segments only, for the same held-out reason.
	adSamples := segmentRecordSamples(t, c, trainSegs, 2000)
	adDict, err := TrainDict(adSamples)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ad-trained dictionary: %d samples, %d B dict", len(adSamples), len(adDict))

	mk := func(d []byte) Codec {
		cd, err := NewZSTDCodec(d)
		if err != nil {
			t.Fatal(err)
		}
		return cd
	}
	none, adTrained := mk(nil), mk(adDict)

	kinds := []struct {
		kind streamKind
		name string
	}{{kindColdNum, "cold-numeric"}, {kindStr, "strings"}, {kindCold, "cold-tail"}}

	// THE PER-RECORD ARENA. One codec serves the whole collection, so the same dictionary that
	// compresses a block's regions also compresses every record written to the arena (ingest
	// compresses per record). A training strategy that wins on regions and loses here may be a net
	// loss, since a columnar collection stores both. Measured on held-out records.
	measRecs := segmentRecordSamples(t, c, measSegs, 2000)
	t.Run("records", func(t *testing.T) {
		var mixedAll [][]byte
		for i := range kinds {
			blocks := blocksAt(t, c, trainSegs, defaultColGrouping())
			mixedAll = append(mixedAll, sampleKind(blocks, kinds[i].kind, DefaultDictSize/regionKinds)...)
		}
		regionDict, rerr := TrainDict(mixedAll)
		var raw, bn, ba, br int
		for _, p := range measRecs {
			raw += len(p)
			bn += len(none.Compress(nil, p))
			ba += len(adTrained.Compress(nil, p))
		}
		msg := "   n/a"
		if rerr == nil {
			rc := mk(regionDict)
			for _, p := range measRecs {
				br += len(rc.Compress(nil, p))
			}
			msg = fmt.Sprintf("%+6.1f%%", 100*float64(br-bn)/float64(bn))
		}
		t.Logf("records(%d held-out)  raw=%9d  none=%8d  ad=%+6.1f%%  region=%s",
			len(measRecs), raw, bn, 100*float64(ba-bn)/float64(bn), msg)
	})

	for _, regime := range dictRegimes {
		trainBlocks := blocksAt(t, c, trainSegs, regime.g)
		measBlocks := blocksAt(t, c, measSegs, regime.g)
		if len(measBlocks) == 0 {
			t.Fatalf("%s: no blocks to measure", regime.name)
		}
		recs := 0
		for _, b := range measBlocks {
			recs += b.n
		}
		t.Logf("--- %s: %d train blocks, %d measure blocks, %.0f rec/blk ---",
			regime.name, len(trainBlocks), len(measBlocks), float64(recs)/float64(len(measBlocks)))

		// region dict: all kinds mixed, from the TRAIN blocks at this group size.
		var mixed [][]byte
		for i := range kinds {
			mixed = append(mixed, sampleKind(trainBlocks, kinds[i].kind, DefaultDictSize/regionKinds)...)
		}
		// A dictionary can fail to train: zstd's BuildDict panics on some degenerate sample
		// distributions and TrainDict contains that as an error. Report it as a missing number
		// rather than failing the sweep -- it is itself a result about the strategy.
		regionTrained, regionOK := none, false
		if len(mixed) > 0 {
			if d, err := TrainDict(mixed); err != nil {
				t.Logf("  %s: region dict did not train: %v", regime.name, err)
			} else {
				regionTrained, regionOK = mk(d), true
			}
		}

		for _, kv := range kinds {
			payloads := regionsOf(measBlocks, kv.kind)
			if len(payloads) == 0 {
				continue
			}
			// per-kind dict: trained on this kind alone, at this group size.
			perKind, perKindOK := none, false
			if s := sampleKind(trainBlocks, kv.kind, DefaultDictSize); len(s) > 0 {
				if d, err := TrainDict(s); err != nil {
					t.Logf("  %s/%s: per-kind dict did not train: %v", regime.name, kv.name, err)
				} else {
					perKind, perKindOK = mk(d), true
				}
			}
			var raw, bn, ba, br, bp int
			for _, p := range payloads {
				raw += len(p)
				bn += len(none.Compress(nil, p))
				ba += len(adTrained.Compress(nil, p))
				br += len(regionTrained.Compress(nil, p))
				bp += len(perKind.Compress(nil, p))
			}
			pct := func(v int, ok bool) string {
				if !ok {
					return "   n/a"
				}
				return fmt.Sprintf("%+6.1f%%", 100*float64(v-bn)/float64(bn))
			}
			t.Logf("  %-13s raw=%9d  none=%8d  ad=%s  region=%s  perKind=%s",
				kv.name, raw, bn, pct(ba, true), pct(br, regionOK), pct(bp, perKindOK))
		}
	}
}

// segmentRecordSamples collects whole-record wire samples from specific segments -- the held-out
// equivalent of CollectSamples, which draws from the entire collection.
func segmentRecordSamples(t *testing.T, c *Collection, segs []*segment, max int) [][]byte {
	t.Helper()
	var out [][]byte
	for _, seg := range segs {
		d := seg.dict.Load()
		for off := 0; off < seg.used && len(out) < max; {
			o := uint32(off)
			total := recTotalLen(seg.data, o)
			if total == 0 {
				break
			}
			if !recIsMarker(seg.data, o) {
				if w, err := seg.codec.Decompress(nil, recAd(seg.data, o)); err == nil {
					if iw, ok := c.recordToInternedDict(d, nil, w); ok {
						out = append(out, iw)
					}
				}
			}
			off += int(total)
		}
	}
	return out
}
