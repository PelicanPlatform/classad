package collections

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// mustAdNoT parses a known-valid ad literal off the test goroutine (where t.Fatal is unsafe); it
// panics on error, which cannot happen for the fixed literals the soak generates.
func mustAdNoT(src string) *classad.ClassAd {
	ad, err := classad.Parse(src)
	if err != nil {
		panic(err)
	}
	return ad
}

// TestScaleSoak is an opt-in scale + soak test: it ingests a large number of records into a
// persistent store, then drives sustained concurrent churn (writers updating, readers checking,
// a scanner validating the whole snapshot, and a maintainer compacting) for a duration, asserting
// correctness throughout and that storage growth stays bounded -- the eviction / pageable-key-index
// / segment-count / compaction machinery at a scale the normal suite (capped at a few thousand
// records to fit the race detector's memory) never reaches. Finally it reopens the store at scale
// and re-verifies, exercising recovery in the large-segment-count regime.
//
// Gated on CLASSAD_SOAK=1 so it never runs in the normal/PR suite; the nightly workflow enables it.
// Tunables: CLASSAD_SOAK_N (records, default 200k), CLASSAD_SOAK_SECONDS (soak duration, default 60).
//
// Invariant that makes correctness cheap to assert under churn: key "k<i>" only ever holds an ad
// with Id==i (writers update in place, never change the mapping), and the writer set is update-only
// so the live count stays exactly N in any snapshot.
func TestScaleSoak(t *testing.T) {
	if os.Getenv("CLASSAD_SOAK") == "" {
		t.Skip("scale/soak is opt-in; set CLASSAD_SOAK=1 (see the nightly workflow)")
	}
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	n := envInt("CLASSAD_SOAK_N", 200_000)
	dur := time.Duration(envInt("CLASSAD_SOAK_SECONDS", 60)) * time.Second
	dir := t.TempDir()

	c, err := Open(Options{Shards: 16, Dir: dir, SegmentSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}

	// Ingest N records in batches.
	ingestStart := time.Now()
	const batchSize = 512
	batch := make([]AdUpdate, 0, batchSize)
	for i := 0; i < n; i++ {
		batch = append(batch, AdUpdate{Key: []byte(fmt.Sprintf("k%d", i)), Ad: mustAd(t, fmt.Sprintf(`[Id=%d]`, i))})
		if len(batch) == batchSize {
			if updateErr := c.Update(batch); updateErr != nil {
				t.Fatal(updateErr)
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if updateErr := c.Update(batch); updateErr != nil {
			t.Fatal(updateErr)
		}
	}
	if c.Len() != n {
		t.Fatalf("after ingest Len=%d, want %d", c.Len(), n)
	}
	st0 := c.Stats()
	t.Logf("ingested %d records in %s: %d segments, %d live bytes (%.0f B/ad)",
		n, time.Since(ingestStart).Round(time.Millisecond), st0.Segments, st0.LiveBytes(), float64(st0.LiveBytes())/float64(n))

	// Soak: concurrent writers/readers/scanner/maintainer for dur, or until a failure cancels.
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()
	var failOnce sync.Once
	fail := func(format string, args ...any) {
		failOnce.Do(func() {
			t.Errorf(format, args...)
			cancel()
		})
	}
	var wg sync.WaitGroup
	var churns, gets, scans, queries int64

	const writers, readers = 6, 6
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			local := 0
			for ctx.Err() == nil {
				i := r.Intn(n)
				if putErr := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdNoT(fmt.Sprintf(`[Id=%d;R=%d]`, i, local))); putErr != nil {
					fail("writer Put(k%d) failed: %v", i, putErr)
					return
				}
				local++
			}
			atomic.AddInt64(&churns, int64(local))
		}(int64(w) + 1)
	}
	for rd := 0; rd < readers; rd++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			local := 0
			for ctx.Err() == nil {
				i := r.Intn(n)
				ad, ok := c.Get([]byte(fmt.Sprintf("k%d", i)))
				if !ok {
					fail("reader Get(k%d) missing during update-only soak", i)
					return
				}
				if id, ok := ad.EvaluateAttrInt("Id"); !ok || id != int64(i) {
					fail("reader Get(k%d) wrong value Id=%d,ok=%v", i, id, ok)
					return
				}
				local++
			}
			atomic.AddInt64(&gets, int64(local))
		}(int64(rd) + 100)
	}
	// Queriers: realistic filtering scans (condor_q-style constraint over an attribute), not just
	// point lookups. The update-only workload keeps every k<i> at Id==i, so a range predicate
	// "Id >= lo && Id < hi" must return exactly hi-lo records, every one with Id in [lo,hi) --
	// verifiable under concurrent churn/compaction (Query reads a consistent MVCC snapshot).
	const queriers = 3
	for qi := 0; qi < queriers; qi++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			width := n / 20
			if width < 1 {
				width = 1
			}
			for ctx.Err() == nil {
				lo := r.Intn(n - width + 1)
				hi := lo + width
				q, qerr := vm.Parse(fmt.Sprintf("Id >= %d && Id < %d", lo, hi))
				if qerr != nil {
					fail("vm.Parse: %v", qerr)
					return
				}
				got := 0
				for ad := range c.Query(q) {
					id, ok := ad.EvaluateAttrInt("Id")
					if !ok || id < int64(lo) || id >= int64(hi) {
						fail("query [%d,%d) returned Id=%d,ok=%v out of range", lo, hi, id, ok)
						return
					}
					got++
				}
				if ctx.Err() != nil {
					return // cancelled mid-query; do not assert a partial result
				}
				if got != width {
					fail("query [%d,%d) matched %d records, want %d", lo, hi, got, width)
					return
				}
				atomic.AddInt64(&queries, 1) // count each completed, validated query
			}
		}(int64(qi) + 500)
	}
	// Scanner: a consistent snapshot must always see exactly n distinct, in-range, unique keys.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			seen := make([]bool, n)
			count := 0
			for ad := range c.Scan() {
				id, ok := ad.EvaluateAttrInt("Id")
				if !ok || id < 0 || id >= int64(n) {
					fail("scan saw out-of-range/garbage Id=%d,ok=%v", id, ok)
					return
				}
				if seen[id] {
					fail("scan saw Id=%d twice", id)
					return
				}
				seen[id] = true
				count++
			}
			if ctx.Err() != nil {
				return
			}
			if count != n {
				fail("scan snapshot saw %d records, want %d (update-only soak must keep count stable)", count, n)
				return
			}
			atomic.AddInt64(&scans, 1)
			time.Sleep(2 * time.Second)
		}
	}()
	// Maintainer: periodic compaction under load.
	wg.Add(1)
	go func() {
		defer wg.Done()
		tk := time.NewTicker(3 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				c.Compact()
			}
		}
	}()
	wg.Wait()
	t.Logf("soak %s: %d churns, %d gets, %d range queries, %d full scans", dur, churns, gets, queries, scans)

	// Final verification at scale.
	c.Compact()
	if c.Len() != n {
		t.Fatalf("after soak Len=%d, want %d", c.Len(), n)
	}
	seen := make([]bool, n)
	for ad := range c.Scan() {
		id, _ := ad.EvaluateAttrInt("Id")
		if id < 0 || id >= int64(n) || seen[id] {
			t.Fatalf("final scan: bad/duplicate Id=%d", id)
		}
		seen[id] = true
	}
	for i := 0; i < n; i++ {
		if !seen[i] {
			t.Fatalf("final scan missing k%d", i)
		}
	}
	st := c.Stats()
	estMmaps := st.Segments * 2 // one data mapping + up to one sidecar per sealed segment
	t.Logf("post-soak: %d segments (~%d mmaps), dead=%d/%d bytes", st.Segments, estMmaps, st.DeadBytes, st.UsedBytes)
	// Bounded growth: compaction must keep the segment count (and thus mmap pressure) far below the
	// process ceiling, and dead space low after a compact.
	if estMmaps >= 60000 { // vm.max_map_count default is 65530, shared process-wide
		t.Errorf("estimated mmaps %d approaches vm.max_map_count; segment growth not bounded by compaction", estMmaps)
	}
	if st.UsedBytes > 0 && float64(st.DeadBytes)/float64(st.UsedBytes) > 0.6 {
		t.Errorf("dead ratio %.2f too high after compaction; reclamation not keeping up",
			float64(st.DeadBytes)/float64(st.UsedBytes))
	}

	// Recovery at scale: reopen and re-verify the count and a value sample.
	if closeErr := c.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	c2, err := Open(Options{Shards: 16, Dir: dir, SegmentSize: 1 << 20})
	if err != nil {
		t.Fatalf("reopen at scale failed: %v", err)
	}
	defer c2.Close()
	if c2.Len() != n {
		t.Fatalf("after reopen Len=%d, want %d", c2.Len(), n)
	}
	for _, i := range []int{0, n / 2, n - 1} {
		ad, ok := c2.Get([]byte(fmt.Sprintf("k%d", i)))
		if !ok {
			t.Errorf("reopen: k%d missing", i)
			continue
		}
		if id, ok := ad.EvaluateAttrInt("Id"); !ok || id != int64(i) {
			t.Errorf("reopen: k%d wrong value Id=%d,ok=%v", i, id, ok)
		}
	}
}
