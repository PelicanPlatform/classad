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

// TestPresenceCountStaticRaceDuringColumnarize isolates the READ-side transient misclassification
// (call it "Bug B") from the known WRITE-side duplicate bug that TestPresenceCountRaceDuringColumnarize
// exercises.
//
// The production symptom for Bug B: `count(*) WHERE JobStatus is undefined` SPIKES (1 -> ~50k -> 1,
// self-healing) while `WHERE JobStatus == 2` DROPS by the exact same amount and the TOTAL count(*)
// stays EXACTLY constant. So records that DO carry JobStatus==2 are transiently READ as
// JobStatus-ABSENT during a background columnarize -- a scan correctness bug, not record churn.
//
// The experimental design that isolates B from the write bug is STATIC data with NO concurrent
// writes or updates:
//
//   - N distinct keys, each written EXACTLY ONCE, every ad carrying JobStatus==2 plus enough varied
//     attributes that columnarization actually builds columns (JobStatus lands in a column).
//   - No writer goroutine at all. The set of keys and their versions never change, so:
//   - a duplicate-version write bug is IMPOSSIBLE (nothing is ever superseded), and
//   - the ground truth is provably fixed: absent==0, ==2==N, total==N.
//   - Concurrently: one maintainer loops the columnarize/reschema rewrites that publish segments,
//     and several readers loop the three counts through the SAME entry points the server uses.
//
// Because the data is static, ANY downward deviation the readers observe is a pure read
// misclassification == Bug B. If the readers only ever see the truth, then B does NOT reproduce on
// static data, which is itself a finding: it would mean B needs concurrent reads+writes+columnarize
// together and points back at the write path.
//
// To exercise the whole-record -> columnarized TRANSITION many times over static data (each segment
// only transitions ONCE, so a single collection gives one burst), the persistent case rebuilds a
// fresh collection for several rounds, each round overlapping a big columnarize burst with heavy
// reader pressure.
func TestPresenceCountStaticRaceDuringColumnarize(t *testing.T) {
	t.Run("persistent", func(t *testing.T) {
		const rounds = 5
		for r := 0; r < rounds; r++ {
			dir := t.TempDir()
			// Tiny segments => hundreds of sealed segments; a large budget => one ColumnarizeSealed
			// call strips+publishes+swaps EVERY sealed segment in a single concurrent burst while the
			// readers hammer, which is the strip/publish window under test at maximum overlap.
			c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 12,
				ColumnarSegmentBudget: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			runStaticPresenceRace(t, c, true)
			c.Close()
		}
	})

	t.Run("memory", func(t *testing.T) {
		c := New(Options{Shards: 1, SegmentSize: 1 << 12})
		defer c.Close()
		runStaticPresenceRace(t, c, false)
	})
}

// sealedSegCount reports how many non-active sealed segments the collection currently holds, and how
// many of those are columnarized -- for logging coverage of the transition under test.
func sealedSegCount(c *Collection) (sealed, columnarized int) {
	for _, sh := range c.shards {
		sh.mu.RLock()
		act := sh.act
		for _, seg := range sh.segs {
			if seg == nil || seg == act || seg.used == 0 {
				continue
			}
			sealed++
			if seg.columnarized() {
				columnarized++
			}
		}
		sh.mu.RUnlock()
	}
	return sealed, columnarized
}

func runStaticPresenceRace(t *testing.T, c *Collection, persistent bool) {
	const n = 8000
	for i := 0; i < n; i++ {
		// JobStatus is ALWAYS 2. Every other attribute varies so the derived schema is non-trivial
		// and columnarization has real columns to build (JobStatus is a hot numeric field that gets
		// stripped from the record body into a column -- the whole point of the isolation).
		ad, err := classad.Parse(fmt.Sprintf(
			`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=2; RequestMemory=%d; `+
				`RequestCpus=%d; RequestDisk=%d; QDate=%d; NumJobStarts=%d; ExitCode=%d; `+
				`RemoteHost="slot%d@host%d.example"; JobUniverse=%d ]`,
			i, i%10, i%37, (i%16)*1024, 1+i%8, (i%32)*2048, 1600000000+i, i%5, i%256,
			i%64, i%128, 5))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}

	// Enable the columnar accelerator (schema + hot tier) so the counts route to the columnar path.
	sch, hot, ok := c.deriveSchema(4096, 12)
	if !ok || !c.installSchemaScan(sch, hot) {
		t.Skip("schema scan did not enable")
	}
	// Cover the currently-sealed segments with sidecar blocks up front (matches the server flow).
	c.EnableSchemaScan(sch, hot)

	absentQ, err := vm.Parse("JobStatus is undefined")
	if err != nil {
		t.Fatal(err)
	}
	presentQ, err := vm.Parse("JobStatus == 2")
	if err != nil {
		t.Fatal(err)
	}
	// Matches every record with a JobStatus (all of them), so the row-path total is exactly n.
	totalQ, err := vm.Parse("JobStatus >= 0 || JobStatus < 0 || JobStatus is undefined")
	if err != nil {
		t.Fatal(err)
	}

	// Sanity: before any concurrency the answers must be the known truth AND the columnar path must
	// actually serve them (otherwise the test proves nothing about that path).
	if got, served := c.CountQuery(absentQ); !served || got != 0 {
		t.Fatalf("pre-race: absent count served=%v got=%d, want served & 0", served, got)
	}
	if got, served := c.CountQuery(presentQ); !served || got != n {
		t.Fatalf("pre-race: present count served=%v got=%d, want served & %d", served, got, n)
	}
	sealedBefore, _ := sealedSegCount(c)

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

	// Readers: hammer the counts and record any deviation from the fixed truth.
	for r := 0; r < 6; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				// Absent count via the columnar presence fast path (schemaScanPresenceCount).
				if got, served := c.CountQuery(absentQ); served {
					bump(&worstAbsent, int64(got))
				}
				// ==2 count via the columnar value fast path (schemaScanCountMulti).
				if got, served := c.CountQuery(presentQ); served {
					bump(&worstMissing, int64(n-got))
					bump(&worstExtra, int64(got-n))
				}
				// Total via the ROW path (full-ad reconstruction through recordWire/spliceInto).
				// If B fires, this stays n while the columnar counts diverge -- the exact production
				// signature of "records read as absent while the total is unchanged".
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

	// Maintainer: loop the segment rewrites that publish columnar payloads. NO writes -- the record
	// set is frozen. The first ColumnarizeSealed call strips EVERY sealed segment's schema'd
	// attributes into columns and swaps the rewritten segments in (one big concurrent burst with the
	// readers); ReschemaScan then drops+rebuilds sidecar blocks and clears schemaScan transiently,
	// keeping colblk publishes churning for the whole window.
	var passes int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(2 * time.Second)
		// The first call is the big strip+publish+swap burst (all sealed segments at once), overlapped
		// with the readers -- the primary window. After it, loop ReschemaScan so the schemaScan-nil
		// transient (ReschemaScan clears c.schemaScan before rebuilding) and the colblk.Store(nil)
		// rebuild path also run concurrently with the readers for the rest of the window.
		c.ColumnarizeSealed()
		atomic.AddInt64(&passes, 1)
		for time.Now().Before(deadline) {
			c.ReschemaScan(4096, 12)
			c.ColumnarizeSealed() // idempotent once all are columnarized; cheap re-scan
			atomic.AddInt64(&passes, 1)
		}
	}()

	time.Sleep(2 * time.Second)
	stop.Store(true)
	wg.Wait()

	_, colNow := sealedSegCount(c)
	seg, saved := ColumnarizedSegments()
	t.Logf("persistent=%v sealedSegs=%d columnarizedNow=%d reads=%d passes=%d "+
		"columnarizedSegments(total)=%d bytesSaved=%d "+
		"worstAbsent=%d worstMissing=%d worstExtra=%d worstTotal=%d",
		persistent, sealedBefore, colNow, atomic.LoadInt64(&reads), atomic.LoadInt64(&passes),
		seg, saved, atomic.LoadInt64(&worstAbsent), atomic.LoadInt64(&worstMissing),
		atomic.LoadInt64(&worstExtra), atomic.LoadInt64(&worstTotal))

	// Ground truth over static data: no record is EVER JobStatus-absent, every record ALWAYS has
	// JobStatus==2, and the total is ALWAYS exactly n. Any deviation is a pure read misclassification.
	if w := atomic.LoadInt64(&worstAbsent); w != 0 {
		t.Errorf("Bug B: a reader saw %d records as JobStatus-absent over STATIC data; the true value "+
			"is 0 (records read as absent during a concurrent columnarize)", w)
	}
	if w := atomic.LoadInt64(&worstMissing); w != 0 {
		t.Errorf("Bug B: a reader saw the JobStatus==2 count DROP by %d below the true %d over STATIC "+
			"data during a concurrent columnarize", w, n)
	}
	if w := atomic.LoadInt64(&worstExtra); w != 0 {
		t.Errorf("a reader saw the JobStatus==2 count EXCEED the true %d by %d over STATIC data "+
			"(unexpected: no writes, so no duplicate version is possible)", n, w)
	}
	if w := atomic.LoadInt64(&worstTotal); w != 0 {
		t.Errorf("a reader saw the TOTAL record count deviate from the true %d by %d over STATIC data", n, w)
	}

	// After the dust settles the answers must be the truth (B self-heals in production).
	if got, served := c.CountQuery(absentQ); !served || got != 0 {
		t.Errorf("post-race: absent count served=%v got=%d, want served & 0", served, got)
	}
	if got, served := c.CountQuery(presentQ); !served || got != n {
		t.Errorf("post-race: present count served=%v got=%d, want served & %d", served, got, n)
	}
}
