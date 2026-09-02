package collections

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestPresenceCountRaceDuringColumnarize checks that COUNT(*) queries over a mutable Collection stay
// exactly correct while a background columnarize rewrites its sealed segments -- the production
// scenario in which a `count(*) WHERE JobStatus is undefined` / `== 2` was reported to go badly
// wrong and then self-heal.
//
// Every ad carries JobStatus == 2 and every distinct key has exactly one current version, so the
// ground truth is fixed and known:
//
//	JobStatus is undefined   -> 0
//	JobStatus == 2           -> N
//	total (present or not)    -> N
//
// A background maintainer columnarizes the sealed segments while a writer keeps superseding keys with
// fresh JobStatus==2 versions (the live-mirror condition) and readers hammer the three counts.
//
// FINDINGS (as of this test's authorship):
//   - The presence / value FAST PATHS never misclassify: the absent-count stays 0 and the `== 2`
//     count never drops below N. So the reported "records read as JobStatus-absent" is NOT the
//     misread site -- schemaScanPresenceCount / schemaScanCountMulti resolve the field per SEGMENT
//     schema and capture each window atomically, and are robust here.
//   - The PERSISTENT store DOES corrupt the counts, in the OPPOSITE direction: `== 2` and the total
//     both climb ABOVE N by the same amount. A concurrent update to a key whose current version lives
//     in a segment being columnarized fails to supersede that prior version, so it stays live
//     alongside the new one -- a duplicate. See commitSegmentRewrite / columnarizeSegment: the swap
//     rewrites every record's offset but does not reconcile the in-memory directory (sh.dir) or the
//     sealed key index to the new offsets, so put()'s findCurrent/lookupSealed can no longer locate
//     the record to supersede. (Root cause below.)
//   - The IN-MEMORY store passes: it never runs the columnarize REWRITE path
//     (columnarizeSealedSegment returns false with no allocNamed), so no segment is swapped and no
//     offset goes stale -- the "always test both stores" divergence.
func TestPresenceCountRaceDuringColumnarize(t *testing.T) {
	t.Run("persistent", func(t *testing.T) {
		dir := t.TempDir()
		// Small segments => many sealed segments; a small budget => columnarize spans many passes,
		// so the readers overlap a long stream of segment rewrites.
		c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 12,
			ColumnarSegmentBudget: 3})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		runPresenceRace(t, c)
	})

	t.Run("memory", func(t *testing.T) {
		c := New(Options{Shards: 1, SegmentSize: 1 << 12})
		defer c.Close()
		runPresenceRace(t, c)
	})
}

func runPresenceRace(t *testing.T, c *Collection) {
	const n = 6000
	for i := 0; i < n; i++ {
		// JobStatus is ALWAYS 2. The other attributes vary so the derived schema is non-trivial and
		// columnarization actually has columns to build.
		ad, err := classad.Parse(fmt.Sprintf(
			`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=2; RequestMemory=%d; `+
				`RequestCpus=%d; RequestDisk=%d; QDate=%d; NumJobStarts=%d; ExitCode=%d ]`,
			i, i%10, i%37, (i%16)*1024, 1+i%8, (i%32)*2048, 1600000000+i, i%5, i%256))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}

	// Enable the columnar accelerator (schema + hot tier) so CountQuery routes to the columnar path.
	sch, hot, ok := c.deriveSchema(4096, 8)
	if !ok || !c.installSchemaScan(sch, hot) {
		t.Skip("schema scan did not enable")
	}
	// Cover the currently-sealed segments with sidecar blocks up front (matches the server flow;
	// harmless for the persistent path, which will later rewrite them columnar-native).
	c.EnableSchemaScan(sch, hot)

	absentQ, err := vm.Parse("JobStatus is undefined")
	if err != nil {
		t.Fatal(err)
	}
	presentQ, err := vm.Parse("JobStatus == 2")
	if err != nil {
		t.Fatal(err)
	}

	// Sanity: before any concurrency the answers must be the known truth, and the columnar path must
	// actually serve them (otherwise the test proves nothing about that path).
	if got, served := c.CountQuery(absentQ); !served || got != 0 {
		t.Fatalf("pre-race: absent count served=%v got=%d, want served & 0", served, got)
	}
	if got, served := c.CountQuery(presentQ); !served || got != n {
		t.Fatalf("pre-race: present count served=%v got=%d, want served & %d", served, got, n)
	}

	// Every record has JobStatus, so this matches every current version exactly once.
	totalQ, err := vm.Parse("JobStatus >= 0 || JobStatus < 0 || JobStatus is undefined")
	if err != nil {
		t.Fatal(err)
	}

	var stop atomic.Bool
	var worstAbsent int64  // max absent-count any reader observed (should stay 0)
	var worstMissing int64 // max (n - presentCount) any reader observed (should stay 0)
	var worstExtra int64   // max (presentCount - n) any reader observed (should stay 0)
	var worstTotal int64   // max |totalCount - n| any reader observed (should stay 0)
	var reads int64
	var wg sync.WaitGroup
	bump := func(p *int64, v int64) {
		for {
			old := atomic.LoadInt64(p)
			if v <= old || atomic.CompareAndSwapInt64(p, old, v) {
				return
			}
		}
	}

	// Readers: hammer the counts and record any deviation from the known truth.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				if got, served := c.CountQuery(absentQ); served {
					bump(&worstAbsent, int64(got))
				}
				if got, served := c.CountQuery(presentQ); served {
					bump(&worstMissing, int64(n-got))
					bump(&worstExtra, int64(got-n))
				}
				// Total records (one current version per key) == n, always.
				tot := 0
				for range c.Query(totalQ) {
					tot++
				}
				d := int64(tot - n)
				if d < 0 {
					d = -d
				}
				bump(&worstTotal, d)
				atomic.AddInt64(&reads, 1)
			}
		}()
	}

	// Writer: keep superseding existing keys with fresh versions that ALSO carry JobStatus==2, the
	// live-mirror condition. The set of distinct keys and their JobStatus never change, so the truth
	// is still: undefined==0, ==2 == n. This churns the active segment and supersedes records in the
	// sealed (and columnarized) segments while they are being read.
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for !stop.Load() {
			key := i % n
			i++
			ad, err := classad.Parse(fmt.Sprintf(
				`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=2; RequestMemory=%d; `+
					`RequestCpus=%d; RequestDisk=%d; QDate=%d; NumJobStarts=%d; ExitCode=%d ]`,
				key, key%10, key%37, (key%16)*1024, 1+key%8, (key%32)*2048, 1600000000+key, i%5, i%256))
			if err != nil {
				return
			}
			_ = c.Put([]byte(fmt.Sprintf("%d.0", key)), ad)
		}
	}()

	// Maintainer: rebuild the segment/block set every way the background maintenance does --
	// columnarize (strips records into columns), reschema (drops+rebuilds blocks), reindex, compact --
	// so the readers overlap a steady stream of block-set rewrites.
	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			c.ColumnarizeSealed()
		}
	}()

	// Let the maintainer run its course, then stop the readers.
	time.Sleep(4 * time.Second)
	stop.Store(true)
	wg.Wait()

	t.Logf("reads=%d worstAbsent=%d worstMissing=%d worstExtra=%d worstTotal=%d",
		atomic.LoadInt64(&reads), atomic.LoadInt64(&worstAbsent), atomic.LoadInt64(&worstMissing),
		atomic.LoadInt64(&worstExtra), atomic.LoadInt64(&worstTotal))

	// Ground truth: no record is ever JobStatus-absent, every record always has JobStatus==2, and the
	// total is always exactly n.
	if w := atomic.LoadInt64(&worstAbsent); w != 0 {
		t.Errorf("a reader saw %d records as JobStatus-absent; the true value is 0 "+
			"(records were miscounted as absent during a concurrent columnarize)", w)
	}
	if w := atomic.LoadInt64(&worstMissing); w != 0 {
		t.Errorf("a reader saw the JobStatus==2 count drop by %d below the true %d "+
			"during a concurrent columnarize", w, n)
	}
	if w := atomic.LoadInt64(&worstExtra); w != 0 {
		t.Errorf("a reader saw the JobStatus==2 count exceed the true %d by %d "+
			"during a concurrent columnarize", n, w)
	}
	if w := atomic.LoadInt64(&worstTotal); w != 0 {
		t.Errorf("a reader saw the TOTAL record count deviate from the true %d by %d "+
			"during a concurrent columnarize", n, w)
	}

	// After the dust settles the answers must be back to the truth (the bug self-heals).
	if got, served := c.CountQuery(absentQ); !served || got != 0 {
		t.Errorf("post-race: absent count served=%v got=%d, want served & 0", served, got)
	}
	if got, served := c.CountQuery(presentQ); !served || got != n {
		t.Errorf("post-race: present count served=%v got=%d, want served & %d", served, got, n)
	}
}
