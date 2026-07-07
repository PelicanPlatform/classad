package collections

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// liveBytes sums the used bytes across a collection's non-nil segments.
func liveBytes(c *Collection) int {
	n := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil {
				n += seg.used
			}
		}
		sh.mu.RUnlock()
	}
	return n
}

func TestCompactionReclaimsAndPreservesData(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 2, SegmentSize: 1 << 16})
	const n = 500
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d; C=0]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	// Churn: update every key many times to generate garbage.
	for round := 1; round <= 20; round++ {
		for i := 0; i < n; i++ {
			if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d; C=%d]`, i, round))); err != nil {
				t.Fatal(err)
			}
		}
	}
	before := liveBytes(c)
	compacted := c.Compact()
	after := liveBytes(c)
	if compacted == 0 {
		t.Fatal("expected at least one shard to compact after heavy churn")
	}
	if after >= before {
		t.Errorf("compaction did not reduce live bytes: before=%d after=%d", before, after)
	}

	// All data still correct after compaction.
	if c.Len() != n {
		t.Fatalf("Len after compaction = %d, want %d", c.Len(), n)
	}
	for i := 0; i < n; i++ {
		ad, ok := c.Get([]byte(fmt.Sprintf("k%d", i)))
		if !ok {
			t.Fatalf("k%d missing after compaction", i)
		}
		if got, _ := ad.EvaluateAttrInt("C"); got != 20 {
			t.Fatalf("k%d has C=%d after compaction, want 20", i, got)
		}
	}

	// Scan still yields each key exactly once after compaction.
	seen := make([]int, n)
	for ad := range c.Scan() {
		id, _ := ad.EvaluateAttrInt("Id")
		seen[id]++
	}
	for i, cnt := range seen {
		if cnt != 1 {
			t.Fatalf("Id %d seen %d times after compaction, want 1", i, cnt)
		}
	}
}

// TestInflightScanAcrossCompaction verifies that a scan already in progress still
// sees each key exactly once even when the shard it is scanning is compacted (and
// updated) mid-iteration. With one shard, the scan snapshots all data up front;
// the compaction retires those segments, but the snapshot's windows keep them
// alive, so the scan is unaffected.
func TestInflightScanAcrossCompaction(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 1, SegmentSize: 1 << 14})
	const n = 800
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d; C=0]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	seen := make([]int, n)
	total := 0
	triggered := false
	for ad := range c.Scan() {
		id, _ := ad.EvaluateAttrInt("Id")
		seen[id]++
		total++
		if !triggered {
			triggered = true
			// Mutate the world underneath the in-flight scan: update every key and
			// force compaction of the shard being scanned.
			for i := 0; i < n; i++ {
				_ = c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d; C=99]`, i)))
			}
			c.Compact()
		}
	}
	if total != n {
		t.Fatalf("in-flight scan yielded %d, want %d", total, n)
	}
	for i, cnt := range seen {
		if cnt != 1 {
			t.Fatalf("Id %d seen %d times across compaction, want 1", i, cnt)
		}
	}
}

// TestScanExactlyOnceUnderUpdatesAndCompaction stresses the invariant with
// concurrent updaters and a concurrent compactor while scanning.
func TestScanExactlyOnceUnderUpdatesAndCompaction(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 8, SegmentSize: 1 << 14})
	const n = 3000
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d; C=0]`, i))); err != nil {
			t.Fatal(err)
		}
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			ctr := 1
			for x := seed; !stop.Load(); x += 13 {
				_ = c.Put([]byte(fmt.Sprintf("k%d", x%n)), mustAd(t, fmt.Sprintf(`[Id=%d; C=%d]`, x%n, ctr)))
				ctr++
			}
		}(w * 700)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			c.Compact()
		}
	}()

	seen := make([]int, n)
	total := 0
	for ad := range c.Scan() {
		id, ok := ad.EvaluateAttrInt("Id")
		if !ok || id < 0 || id >= n {
			t.Errorf("bad Id %d", id)
			continue
		}
		seen[id]++
		total++
	}
	stop.Store(true)
	wg.Wait()

	for i, cnt := range seen {
		if cnt != 1 {
			t.Fatalf("Id %d seen %d times, want exactly 1", i, cnt)
		}
	}
	if total != n {
		t.Fatalf("scan yielded %d, want %d", total, n)
	}
}
