package collections

import "sort"

// Index memory accounting. IndexSizes measures the resident bytes each configured
// index occupies across all live segments -- the roaring posting bitmaps plus their
// keys -- and reports them against the live data bytes, so an operator (and the
// watermark auto-tuner) can see how much memory indexing costs and decide whether it
// is worth it. Only built segment indexes count; an unindexed (not-yet-Reindexed)
// segment contributes nothing.

// sizeBytes estimates the resident footprint of one attribute's categorical postings.
func (cp *catPostings) sizeBytes() int64 {
	var n int64
	for k, bm := range cp.post {
		n += int64(len(k)) + int64(bm.GetSizeInBytes())
	}
	for k, bm := range cp.exact {
		n += int64(len(k)) + int64(bm.GetSizeInBytes())
	}
	for k, v := range cp.exactCase {
		n += int64(len(k) + len(v))
	}
	if cp.exc != nil {
		n += int64(cp.exc.GetSizeInBytes())
	}
	if cp.posted != nil {
		n += int64(cp.posted.GetSizeInBytes())
	}
	return n
}

// sizeBytes estimates the resident footprint of one attribute's value postings.
func (vp *valPostings) sizeBytes() int64 {
	var n int64
	for _, bm := range vp.post {
		n += 8 + int64(bm.GetSizeInBytes()) // 8 for the float64 key
	}
	n += int64(len(vp.sortedKeys)) * 8
	if vp.exc != nil {
		n += int64(vp.exc.GetSizeInBytes())
	}
	if vp.posted != nil {
		n += int64(vp.posted.GetSizeInBytes())
	}
	return n
}

// IndexSize is the measured memory footprint of one attribute's index.
type IndexSize struct {
	Attr  string  `json:"attr"`
	Kind  string  `json:"kind"`  // "categorical" | "value"
	Bytes int64   `json:"bytes"` // resident posting bytes across all live segments
	Auto  bool    `json:"auto"`  // created by the auto-tuner (vs human/Options)
	Frac  float64 `json:"frac"`  // Bytes as a fraction of the live data bytes
}

// IndexSizes is the collection's index memory, per attribute and in total, against the
// live data bytes -- the denominator for the "index is N% of data" watermark.
type IndexSizes struct {
	PerIndex   []IndexSize `json:"perIndex"`
	TotalBytes int64       `json:"totalBytes"`
	DataBytes  int64       `json:"dataBytes"` // live compressed record bytes
	Frac       float64     `json:"frac"`      // TotalBytes / DataBytes
}

// IndexSizes measures each configured index's resident bytes across all live segments,
// tagged with provenance (human vs auto), against the live data bytes. It takes each
// shard's read lock briefly, so it is safe alongside readers and writers.
func (c *Collection) IndexSizes() IndexSizes {
	catBytes := map[uint32]int64{}
	valBytes := map[uint32]int64{}
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			si := seg.idx.Load()
			if si == nil {
				continue
			}
			for id, cp := range si.cat {
				catBytes[id] += cp.sizeBytes()
			}
			for id, vp := range si.val {
				valBytes[id] += vp.sizeBytes()
			}
		}
		sh.mu.RUnlock()
	}

	spec := c.spec.Load()
	name := func(id uint32) (string, bool) {
		if spec != nil && spec.inline {
			n, ok := spec.names[id]
			return n, ok
		}
		return c.intern.Name(id)
	}
	dataBytes := c.Stats().LiveBytes()
	out := IndexSizes{DataBytes: dataBytes}
	appendSizes := func(m map[uint32]int64, kind string) {
		for id, b := range m {
			nm, ok := name(id)
			if !ok {
				continue
			}
			sz := IndexSize{Attr: nm, Kind: kind, Bytes: b, Auto: spec.isAuto(id)}
			if dataBytes > 0 {
				sz.Frac = float64(b) / float64(dataBytes)
			}
			out.PerIndex = append(out.PerIndex, sz)
			out.TotalBytes += b
		}
	}
	appendSizes(catBytes, "categorical")
	appendSizes(valBytes, "value")
	// Largest first: the indexes a memory budget would trim.
	sort.Slice(out.PerIndex, func(i, j int) bool {
		if out.PerIndex[i].Bytes != out.PerIndex[j].Bytes {
			return out.PerIndex[i].Bytes > out.PerIndex[j].Bytes
		}
		return out.PerIndex[i].Attr < out.PerIndex[j].Attr
	})
	if dataBytes > 0 {
		out.Frac = float64(out.TotalBytes) / float64(dataBytes)
	}
	return out
}
