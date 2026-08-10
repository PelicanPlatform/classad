package collections

// Dictionary training samples drawn from columnar block REGIONS rather than whole ads.
//
// NOT WIRED INTO RETRAINING, DELIBERATELY -- see the measurement below. Retraining trains on whole
// records (CollectSamples, via compact.go), and that is the right choice. This exists because it is
// the right choice for a regime that is reachable but not the default, and because the reasoning is
// worth keeping next to the code rather than in a pull request.
//
// The premise is sound: a dictionary trained on whole ClassAd records is matched to compressing a
// record and mismatched to a block's three regions, which are a different shape entirely
// (column-major fixed-width integers, concatenated string values, and the record-shaped cold tail).
// Sampling the regions themselves gives training content drawn from what is actually compressed, and
// it does win per region. MEASURED HELD-OUT on the real OSPool corpus, oldest half of segments
// training and newest half measured (TestDictStrategyByGroupSize), at the production ~1 MiB row
// group: strings -4.5% vs the ad dictionary's -3.5%, cold tail -7.0% vs -5.3%.
//
// It still loses, because ONE codec serves the whole collection. The same dictionary compresses
// every record written to the arena, the arena is several times the bytes of the block regions, and
// records are exactly what an ad-trained dictionary is good at. Net stored bytes over the same
// held-out data (TestDictStrategyNetBytes):
//
//	strategy         arena      regions     total
//	none           3514376      481643    3996019
//	ad-trained     1640224      464535    2104759   <- production
//	region-trained 1853546      459125    2312671   (+9.9%)
//
// Region training saves 5.4 KB on the regions and gives back 213 KB on the arena. Per-region
// percentages alone point the other way, which is the trap this comment exists to close.
//
// WHEN IT WOULD PAY. Dictionary value on a region falls off fast as the block grows, because a
// dictionary supplies redundancy the payload cannot supply itself. Same measurement, by block size,
// strings / cold tail with a region dictionary: 8 records -26.1%/-32.4%, 27 records -12.6%/-16.2%,
// 53 records -7.3%/-10.3%, 107 records (the ~1 MiB default) -4.5%/-7.0%. So a deployment that
// configured much smaller row groups than colGroupTargetBytes, or one that stopped storing the row
// arena, could reach the regime where this wins -- and should re-run both tests rather than trust
// these numbers, which are for one corpus.
//
// Two things that are NOT worth building, measured: a per-region-kind dictionary adds nothing over
// one mixed region dictionary (-4.2% vs -4.5% on strings) and fails to train at all on the cold tail
// at every group size at or above 32 records (zstd BuildDict divides by zero on that sample
// distribution). And the cold-numeric region gains nothing from any dictionary -- between 0.0% and
// +0.7%, i.e. slightly worse -- so a dictionary-less codec for that one stream would buy about 100
// bytes per 2.7 MB, which does not pay for a second zstd encoder's resident memory.

// regionKinds is the number of region kinds sampled, and the number of equal shares the byte budget
// is split into.
const regionKinds = 3

// regionChunk is the size of one sample taken from a region. Many smaller samples spread across
// segments and regions make better dictionary content than a few large ones, which would let a
// single segment's bytes crowd out the rest.
const regionChunk = 4 << 10

// CollectRegionSamples returns dictionary-training samples drawn from sealed segments' columnar
// block regions, in equal byte shares across the three kinds, up to about maxBytes in total.
//
// For an APPEND-ONLY collection the draw is biased to recent segments: future writes resemble
// recent history far more than the oldest retained records, and an archive's oldest segments may
// predate schema or workload changes entirely. lookBackBytes caps how far back the draw reaches,
// measured in segment bytes from the newest; 0 means no limit.
//
// Returns nil when no sealed segment carries a block -- there is nothing to sample from, and the
// caller should fall back to record samples (see CollectSamples).
func (c *Collection) CollectRegionSamples(maxBytes, lookBackBytes int) [][]byte {
	if maxBytes <= 0 {
		return nil
	}
	blocks := c.sampleableBlocks(lookBackBytes)
	if len(blocks) == 0 {
		return nil
	}
	share := maxBytes / regionKinds
	var out [][]byte
	// Round-robin the kinds so a shortfall in one (an empty cold tail, say) does not eat another's
	// share, and so the samples interleave rather than arriving in three solid runs.
	for kind := 0; kind < regionKinds; kind++ {
		out = append(out, sampleKind(blocks, streamKind(kind), share)...)
	}
	return out
}

// sampleableBlocks returns the row-group blocks to draw from, newest segment first, honoring the
// look-back budget. A segment contributes ALL of its row groups, and is charged its segment bytes
// once -- the budget bounds how far back in history the draw reaches, not how many blocks it sees.
func (c *Collection) sampleableBlocks(lookBackBytes int) []*columnarBlock {
	type seg struct {
		blocks []*columnarBlock
		used   int
	}
	var segs []seg
	for _, sh := range c.shards {
		sh.mu.RLock()
		act := sh.act
		// sh.segs is oldest-first, so walking backwards is newest-first.
		for i := len(sh.segs) - 1; i >= 0; i-- {
			s := sh.segs[i]
			if s == nil || s == act || s.used == 0 {
				continue
			}
			if cs := s.colblk.Load(); cs != nil && len(cs.blocks) > 0 {
				segs = append(segs, seg{cs.blocks, s.used})
			}
		}
		sh.mu.RUnlock()
	}
	var out []*columnarBlock
	bytes := 0
	for _, s := range segs {
		out = append(out, s.blocks...)
		bytes += s.used
		if lookBackBytes > 0 && bytes >= lookBackBytes {
			break // far enough back: older segments predate what future writes will look like
		}
	}
	return out
}

// sampleKind takes up to share bytes of one region kind, spread evenly across blocks.
func sampleKind(blocks []*columnarBlock, kind streamKind, share int) [][]byte {
	if share <= 0 || len(blocks) == 0 {
		return nil
	}
	perBlock := share / len(blocks)
	if perBlock < regionChunk {
		perBlock = regionChunk // few blocks: take a whole chunk from each rather than slivers
	}
	var out [][]byte
	taken := 0
	for _, b := range blocks {
		if taken >= share {
			break
		}
		raw, err := b.regionRaw(kind)
		if err != nil || len(raw) == 0 {
			continue
		}
		// Spread the take across the region instead of always its head, so the samples are
		// representative of the whole rather than of whatever sorts first.
		for off := 0; off < len(raw) && taken < share; off += regionChunk * 2 {
			end := off + regionChunk
			if end > len(raw) {
				end = len(raw)
			}
			cp := make([]byte, end-off)
			copy(cp, raw[off:end])
			out = append(out, cp)
			taken += len(cp)
			if taken-len(cp) >= perBlock {
				break // this block has given its share; move on so one segment cannot dominate
			}
		}
	}
	return out
}

// regionRaw decompresses one of a block's regions without touching the block cache: training reads
// each region once, and caching them would evict what live queries are using.
func (b *columnarBlock) regionRaw(kind streamKind) ([]byte, error) {
	var comp []byte
	switch kind {
	case kindColdNum:
		comp = b.coldNumComp
	case kindStr:
		comp = b.strComp
	case kindCold:
		comp = b.coldComp
	}
	if len(comp) == 0 {
		return nil, nil
	}
	return b.codec.Decompress(nil, comp)
}
