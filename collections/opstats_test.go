package collections

import (
	"fmt"
	"testing"
	"time"
)

// TestOpCounterHistogram: observe() files durations into the right buckets, tracks the
// max, and addStat sums buckets while taking the max of maxes across shards.
func TestOpCounterHistogram(t *testing.T) {
	t.Parallel()
	var o opCounter
	// One in the 100µs bucket (bound[0]), one in 10ms (bound[2]), one at 20s (bound[7]),
	// one at 45s (overflow bucket, index len(bounds)).
	o.observe(50 * time.Microsecond)
	o.observe(5 * time.Millisecond)
	o.observe(20 * time.Second)
	o.observe(45 * time.Second)

	s := o.snapshot()
	if s.Count != 4 {
		t.Fatalf("Count = %d, want 4", s.Count)
	}
	if s.MaxNanos != int64(45*time.Second) {
		t.Errorf("MaxNanos = %d, want %d", s.MaxNanos, int64(45*time.Second))
	}
	if len(s.Buckets) != len(latencyBucketBoundsNanos)+1 {
		t.Fatalf("len(Buckets) = %d, want %d", len(s.Buckets), len(latencyBucketBoundsNanos)+1)
	}
	want := map[int]int64{0: 1, 2: 1, 7: 1, len(latencyBucketBoundsNanos): 1}
	for i, got := range s.Buckets {
		if got != want[i] {
			t.Errorf("bucket[%d] = %d, want %d", i, got, want[i])
		}
	}

	// addStat: counts and buckets sum; MaxNanos takes the larger.
	var dst OpStat
	addStat(&dst, s)
	addStat(&dst, OpStat{Count: 1, MaxNanos: int64(2 * time.Second), Buckets: mustBuckets(6, 1)})
	if dst.Count != 5 {
		t.Errorf("aggregated Count = %d, want 5", dst.Count)
	}
	if dst.MaxNanos != int64(45*time.Second) {
		t.Errorf("aggregated MaxNanos = %d, want %d (max of maxes)", dst.MaxNanos, int64(45*time.Second))
	}
	if dst.Buckets[6] != 1 || dst.Buckets[7] != 1 || dst.Buckets[len(latencyBucketBoundsNanos)] != 1 {
		t.Errorf("aggregated buckets wrong: %v", dst.Buckets)
	}
}

// mustBuckets returns a bucket slice with count at index i.
func mustBuckets(i int, count int64) []int64 {
	b := make([]int64, len(latencyBucketBoundsNanos)+1)
	b[i] = count
	return b
}

// TestOpStatsWriteAndSegmentCounters: writes count shard write-lock wait/hold and
// segment allocations.
func TestOpStatsWriteAndSegmentCounters(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4, SegmentSize: 1 << 14})
	const n = 300
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	op := c.OpStats()
	if op.ShardWriteHold.Count == 0 {
		t.Error("ShardWriteHold.Count = 0; every write holds a shard lock")
	}
	if op.ShardWriteWait.Count == 0 {
		t.Error("ShardWriteWait.Count = 0; every write acquires a shard lock")
	}
	if op.ShardWriteHold.Count != op.ShardWriteWait.Count {
		t.Errorf("wait/hold counts differ (%d vs %d); they are recorded as a pair",
			op.ShardWriteWait.Count, op.ShardWriteHold.Count)
	}
	if op.SegmentAlloc.Count == 0 {
		t.Error("SegmentAlloc.Count = 0; the first write to each shard allocates a segment")
	}
	// Counters are cumulative and non-negative.
	if op.ShardWriteHold.Nanos < 0 || op.SegmentAlloc.Nanos < 0 {
		t.Errorf("negative cumulative nanos: hold=%d seg=%d", op.ShardWriteHold.Nanos, op.SegmentAlloc.Nanos)
	}
}

// TestOpStatsMaintenanceCounters: each maintenance pass bumps exactly its own counter.
func TestOpStatsMaintenanceCounters(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 2, SegmentSize: 1 << 14})
	for i := 0; i < 50; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}

	before := c.OpStats()
	c.Compact()
	if got, want := c.OpStats().Compact.Count, before.Compact.Count+1; got != want {
		t.Errorf("Compact.Count = %d, want %d", got, want)
	}

	before = c.OpStats()
	c.Reindex()
	if got, want := c.OpStats().Reindex.Count, before.Reindex.Count+1; got != want {
		t.Errorf("Reindex.Count = %d, want %d", got, want)
	}

	before = c.OpStats()
	if _, err := c.RetrainDict(64); err != nil {
		t.Logf("RetrainDict returned %v (still expected to record a timing)", err)
	}
	if got, want := c.OpStats().Retrain.Count, before.Retrain.Count+1; got != want {
		t.Errorf("Retrain.Count = %d, want %d", got, want)
	}
}
