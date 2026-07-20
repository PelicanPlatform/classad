package collections

// Stats is a point-in-time summary of a collection's storage, for observability
// and capacity planning (e.g. Prometheus metrics). All byte counts are of the
// dictionary-compressed on-arena form, not the ads' decompressed text.
type Stats struct {
	// Ads is the number of live keys (== Len).
	Ads int
	// Segments is the number of arena segments across all shards.
	Segments int
	// ArenaBytes is the total capacity of those segments -- the memory the
	// collection has reserved for record storage, and the dominant term in its
	// resident footprint. (RAM segments back this on the Go heap; persistent
	// segments back it with an mmap.)
	ArenaBytes int64
	// UsedBytes is the bytes written into segments: live records plus superseded
	// (dead) records not yet reclaimed by compaction.
	UsedBytes int64
	// DeadBytes is the bytes belonging to superseded records -- reclaimable by
	// Compact. LiveBytes = UsedBytes - DeadBytes.
	DeadBytes int64
}

// LiveBytes is the compressed size of the live records (UsedBytes - DeadBytes).
func (s Stats) LiveBytes() int64 { return s.UsedBytes - s.DeadBytes }

// Stats returns a snapshot of the collection's storage. It takes each shard's
// read lock briefly, so it is safe to call concurrently with readers and writers.
func (c *Collection) Stats() Stats {
	var s Stats
	for _, sh := range c.shards {
		sh.mu.RLock()
		s.Ads += sh.count
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			s.Segments++
			s.ArenaBytes += int64(len(seg.data))
			s.UsedBytes += int64(seg.used)
			s.DeadBytes += seg.dead
		}
		sh.mu.RUnlock()
	}
	return s
}

// OpStats returns a snapshot of the collection's operational timing counters -- where
// its callers spent time blocked in, or holding, each of the store's stall points
// (shard write lock, segment allocation, durability sync, and the maintenance passes).
// Per-shard counters are summed across all shards; every value is a monotonic
// cumulative total, so a scraper derives rate and mean latency from deltas. It reads
// atomics locklessly, so it never contends with readers or writers.
func (c *Collection) OpStats() OpStats {
	var s OpStats
	for _, sh := range c.shards {
		s.add(&sh.metrics)
	}
	s.Compact = c.opm.compact.snapshot()
	s.Retrain = c.opm.retrain.snapshot()
	s.Reindex = c.opm.reindex.snapshot()
	return s
}
