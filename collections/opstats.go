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

// opCounter is one instrumented operation: a call count and cumulative nanoseconds.
type opCounter struct {
	count atomic.Int64
	nanos atomic.Int64
}

// observe records one occurrence lasting d.
func (o *opCounter) observe(d time.Duration) {
	o.count.Add(1)
	o.nanos.Add(int64(d))
}

// snapshot reads the counter locklessly.
func (o *opCounter) snapshot() OpStat {
	return OpStat{Count: o.count.Load(), Nanos: o.nanos.Load()}
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
}
