package collections

// Dictionary training samples drawn from columnar block REGIONS rather than whole ads.
//
// The dictionary is trained from CollectSamples: whole ClassAd records. That content is a good
// match for compressing a record -- small, independent, and full of the attribute names and values
// the dictionary carries. It is a poor match for a block's three regions, which are aggregates over
// a whole segment: column-major fixed-width integers, concatenated string values, and the
// record-shaped cold tail. Measured on a 4000-record fixture, an ad-trained dictionary gained
// -0.1% on the cold numeric region, -14.7% on the strings, and -3.7% on the cold tail -- it helped
// none of them.
//
// Sampling the regions themselves gives training content drawn from what is actually being
// compressed. The interesting case is a SMALL block, which has little internal redundancy of its
// own to exploit and is therefore where a dictionary can still earn something; a large block
// compresses well unaided either way.

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
