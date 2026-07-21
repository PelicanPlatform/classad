package collections

import (
	"sync/atomic"
	"time"
)

// Operational timing counters -- "where does the collection block the world?"
//
// A collection stalls its callers in a handful of places: waiting for and holding a
// shard's write lock, allocating a new arena segment (a syscall under the shard
// lock), flushing mmap'd data to disk (msync), holding the collection-wide snapshot
// lock exclusively (Truncate/Restore reload the world), and the heavy maintenance
// passes (compact, dict retrain, reindex). opMetrics accumulates, for each of those,
// how many times it ran and the total wall time spent in it, as monotonic counters.
//
// The design is Prometheus-friendly: OpStats() returns cumulative {Count, Nanos}
// pairs, so a scraper (e.g. htcondordb, which runs the store in a separate process)
// derives rate() from the counters and mean latency from Nanos/Count -- exactly the
// _sum/_count pair a Prometheus summary exposes. Nothing here depends on a metrics
// library, keeping the collections package dependency-free.
//
// Per-shard counters live on the shard itself so writers under the shard lock and
// concurrent readers do not all contend on one cache line; collection-wide counters
// (maintenance, snapshot lock) are infrequent and live on the Collection. All are
// plain atomics, read locklessly by OpStats().

// latencyBucketBoundsNanos are the inclusive upper bounds of the latency histogram,
// log-scale from 100µs to 30s. observe() files each occurrence into the first bucket it
// fits; anything larger than the last bound lands in a final overflow bucket (so there
// are len+1 buckets). The mean (Nanos/Count) hides tail stalls -- a slow msync or a
// long retrain shows up here as a count in the 1s/10s/30s buckets and in MaxNanos.
var latencyBucketBoundsNanos = [...]int64{
	100_000,        // 100µs
	1_000_000,      // 1ms
	10_000_000,     // 10ms
	100_000_000,    // 100ms
	1_000_000_000,  // 1s
	5_000_000_000,  // 5s
	10_000_000_000, // 10s
	30_000_000_000, // 30s
}

// LatencyBucketBoundsNanos returns a copy of the histogram's inclusive upper bounds (in
// nanoseconds). An OpStat's Buckets slice has one more entry than this: buckets[i] counts
// observations <= bound[i], and the final entry counts observations above the last bound.
func LatencyBucketBoundsNanos() []int64 {
	return append([]int64(nil), latencyBucketBoundsNanos[:]...)
}

// bucketIndex maps a duration (nanos) to its histogram bucket.
func bucketIndex(n int64) int {
	for i, b := range latencyBucketBoundsNanos {
		if n <= b {
			return i
		}
	}
	return len(latencyBucketBoundsNanos) // overflow bucket
}

// opCounter is one instrumented operation: a call count, cumulative nanoseconds, the
// max single duration seen, and a latency histogram -- so a caller can see the tail, not
// just the mean. All fields are plain atomics, read locklessly by snapshot().
type opCounter struct {
	count   atomic.Int64
	nanos   atomic.Int64
	max     atomic.Int64
	buckets [len(latencyBucketBoundsNanos) + 1]atomic.Int64
}

// observe records one occurrence lasting d.
func (o *opCounter) observe(d time.Duration) {
	n := int64(d)
	o.count.Add(1)
	o.nanos.Add(n)
	for { // lockless max
		cur := o.max.Load()
		if n <= cur || o.max.CompareAndSwap(cur, n) {
			break
		}
	}
	o.buckets[bucketIndex(n)].Add(1)
}

// snapshot reads the counter locklessly.
func (o *opCounter) snapshot() OpStat {
	s := OpStat{Count: o.count.Load(), Nanos: o.nanos.Load(), MaxNanos: o.max.Load()}
	s.Buckets = make([]int64, len(o.buckets))
	for i := range o.buckets {
		s.Buckets[i] = o.buckets[i].Load()
	}
	return s
}

// shardMetrics are the per-shard operational counters, kept on each shard to avoid
// cross-shard contention on a single collection-wide atomic under multi-shard writes.
// Read-lock timing is intentionally omitted: a reader that holds a shard too long
// shows up as writeWait on the writers it blocks, which is the signal that matters.
type shardMetrics struct {
	writeWait opCounter // time blocked acquiring the shard write lock
	writeHold opCounter // time holding the shard write lock (the world is blocked)
	segAlloc  opCounter // time allocating/mapping a new arena segment
	sync      opCounter // time flushing segment bytes to disk (msync/fsync)
}

// opMetrics are the collection-wide maintenance-pass counters; per-shard counters
// live on each shard's shardMetrics. (The collection-wide snapshot lock lives in the
// db package, which keeps its own counter -- see db.OpStats.)
type opMetrics struct {
	compact opCounter // Compact() reclaiming dead space
	retrain opCounter // dictionary retrain + rewrite
	reindex opCounter // index rebuild
}

// OpStat is one operation's cumulative call count and total wall-nanoseconds. Both
// are monotonic; a scraper derives rate and mean latency (Nanos/Count) from deltas.
type OpStat struct {
	Count int64 `json:"count"`
	Nanos int64 `json:"nanos"`
	// MaxNanos is the longest single occurrence seen, and Buckets is the latency
	// histogram (see LatencyBucketBoundsNanos: buckets[i] counts occurrences <= bound[i],
	// the last entry counts the overflow above the final bound). Both surface the tail
	// the mean (Nanos/Count) hides. Omitted from JSON when empty so an older peer that
	// never populated them stays byte-compatible.
	MaxNanos int64   `json:"maxNanos,omitempty"`
	Buckets  []int64 `json:"buckets,omitempty"`
}

// OpStats is a snapshot of a collection's operational timing counters -- the wall
// time its callers spent blocked in, or holding, each of the store's stall points.
// Shard counters are summed across all shards. See opMetrics for what each measures.
// OpStats reports, per stall point, the cumulative call count and total wall time.
// The collection-wide snapshot lock (Truncate/Restore) lives one layer up in the db
// package, which composes its own snapshot-lock counter onto this snapshot.
type OpStats struct {
	ShardWriteWait OpStat `json:"shardWriteWait"`
	ShardWriteHold OpStat `json:"shardWriteHold"`
	SegmentAlloc   OpStat `json:"segmentAlloc"`
	Sync           OpStat `json:"sync"`
	Compact        OpStat `json:"compact"`
	Retrain        OpStat `json:"retrain"`
	Reindex        OpStat `json:"reindex"`
}

// add accumulates one shard's counters into the snapshot.
func (s *OpStats) add(m *shardMetrics) {
	addStat(&s.ShardWriteWait, m.writeWait.snapshot())
	addStat(&s.ShardWriteHold, m.writeHold.snapshot())
	addStat(&s.SegmentAlloc, m.segAlloc.snapshot())
	addStat(&s.Sync, m.sync.snapshot())
}

func addStat(dst *OpStat, v OpStat) {
	dst.Count += v.Count
	dst.Nanos += v.Nanos
	if v.MaxNanos > dst.MaxNanos {
		dst.MaxNanos = v.MaxNanos
	}
	if len(v.Buckets) > 0 {
		if dst.Buckets == nil {
			dst.Buckets = make([]int64, len(v.Buckets))
		}
		for i := range v.Buckets {
			if i < len(dst.Buckets) {
				dst.Buckets[i] += v.Buckets[i]
			}
		}
	}
}
