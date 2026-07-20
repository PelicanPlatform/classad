package collections

import (
	"fmt"
	"testing"
)

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
