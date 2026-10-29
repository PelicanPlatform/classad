package collections

import (
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// benchQueueAd is a job ad of roughly a real queue's width: ~100 attributes, a few of them indexed.
func benchQueueAd(i int, now int64, r *rand.Rand) *classad.ClassAd {
	a := classad.New()
	a.InsertAttr("ClusterId", int64(1000000+i/10))
	a.InsertAttr("ProcId", int64(i%10))
	a.InsertAttrString("Owner", fmt.Sprintf("user%d", i%97))
	st := int64(1)
	if r.Intn(3) == 0 {
		st = 5
	}
	a.InsertAttr("JobStatus", st)
	a.InsertAttr("QDate", now-3*86400+int64(r.Intn(2*86400)))
	a.InsertAttr("EnteredCurrentStatus", now-2*86400+int64(r.Intn(86400)))
	for k := 0; k < 80; k++ {
		a.InsertAttr(fmt.Sprintf("NumAttr%02d", k), int64(r.Intn(100000)))
	}
	for k := 0; k < 20; k++ {
		a.InsertAttrString(fmt.Sprintf("StrAttr%02d", k), fmt.Sprintf("/home/user%d/path/value_%d_%d", i%97, k, r.Intn(50)))
	}
	return a
}

const benchQueueNow = int64(1_800_000_000)

func benchQueueKey(j int) []byte { return []byte(fmt.Sprintf("%d.%d", 1000000+j/10, j%10)) }

// benchQueuePatch is one job update: two counters, and for about a tenth of them a status change.
func benchQueuePatch(r *rand.Rand, now int64) *classad.ClassAd {
	p := classad.New()
	p.InsertAttr("NumAttr00", int64(r.Intn(100000)))
	p.InsertAttr("NumAttr01", now)
	if r.Intn(10) == 0 {
		p.InsertAttr("JobStatus", 2)
		p.InsertAttr("EnteredCurrentStatus", now-int64(r.Intn(7200)))
	}
	return p
}

// openBenchQueue opens a 16-shard delta store of n job ads, indexed, then patched in rounds
// (15% of the jobs each) with no Reindex after.
func openBenchQueue(b *testing.B, n, rounds int) (*Collection, int64) {
	b.Helper()
	c, err := Open(Options{Dir: b.TempDir(), Shards: 16, DeltaMax: 16,
		ValueAttrs:       []string{"JobStatus", "EnteredCurrentStatus", "QDate", "CompletionDate", "ClusterId"},
		CategoricalAttrs: []string{"Owner"}})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { c.Close() })
	r := rand.New(rand.NewSource(1))
	now := benchQueueNow
	for i := 0; i < n; i += 500 {
		var batch []AdUpdate
		for j := i; j < min(i+500, n); j++ {
			batch = append(batch, AdUpdate{Key: benchQueueKey(j), Ad: benchQueueAd(j, now, r)})
		}
		if err := c.Update(batch); err != nil {
			b.Fatal(err)
		}
	}
	c.Reindex()
	for round := 0; round < rounds; round++ {
		w := c.Begin()
		for j := 0; j < n; j++ {
			if r.Intn(100) < 15 {
				w.PatchAttrs(benchQueueKey(j), benchQueuePatch(r, now), nil)
			}
		}
		w.Commit()
	}
	return c, now
}

type benchShape struct{ name, expr string }

// benchQueueShapes are the indexed read shapes a queue mirror serves.
func benchQueueShapes(now int64) []benchShape {
	return []benchShape{
		{"empty", "ClusterId == -5"},
		{"range-and-eq", fmt.Sprintf("EnteredCurrentStatus >= %d && JobStatus == 2", now-3600)},
		{"range", fmt.Sprintf("QDate >= %d", now-3600)},
		{"point", "ClusterId == 1001234"},
	}
}

// BenchmarkIndexedReadAfterWrites is a job-queue mirror some time after its last Reindex: every
// job loaded and indexed, then patched in rounds (delta records, about a tenth of them changing
// JobStatus and EnteredCurrentStatus) with no Reindex in between. Each sub-benchmark is one
// indexed read shape against it.
func BenchmarkIndexedReadAfterWrites(b *testing.B) {
	c, now := openBenchQueue(b, 30000, 6)
	for _, s := range benchQueueShapes(now) {
		q, err := vm.Parse(s.expr)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(s.name, func(b *testing.B) {
			var st ScanStats
			for i := 0; i < b.N; i++ {
				st = ScanStats{}
				for range c.QueryRawWireStats(q, []string{"ClusterId"}, false, &st) {
				}
			}
			b.ReportMetric(float64(st.RecordsVisited), "visited/op")
		})
	}
}

// BenchmarkMixedQueueWorkload runs a queue mirror's steady state for a fixed time: one writer
// committing job updates (two per transaction, durable) and four readers looping over the indexed
// read shapes, against 55k jobs in 16 shards. The writer is paced at 300 updates/s or runs flat
// out; the readers run flat out or are paced at 50 reads/s between them, which holds the read load
// equal however fast each read is. It reports the writer's throughput and per-commit latency and
// the readers' per-query latency; run it with -benchtime 1x (each iteration is the whole run).
func BenchmarkMixedQueueWorkload(b *testing.B) {
	for _, tc := range []struct{ writes, reads int }{{300, 0}, {300, 50}, {0, 50}, {0, 0}} {
		rate := func(n int) string {
			if n == 0 {
				return "unpaced"
			}
			return fmt.Sprintf("%dps", n)
		}
		b.Run("writer="+rate(tc.writes)+",readers="+rate(tc.reads), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				runMixedQueueWorkload(b, tc.writes, tc.reads, 8*time.Second)
			}
		})
	}
}

func runMixedQueueWorkload(b *testing.B, rate, readRate int, dur time.Duration) {
	b.StopTimer()
	c, now := openBenchQueue(b, 55000, 2)
	var qs []*vm.Query
	for _, s := range benchQueueShapes(now) {
		q, err := vm.Parse(s.expr)
		if err != nil {
			b.Fatal(err)
		}
		qs = append(qs, q)
	}
	catchUps0 := IndexCatchUps()
	b.StartTimer()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var writeLat []time.Duration
	updates := 0
	wg.Add(1)
	go func() {
		defer wg.Done()
		r := rand.New(rand.NewSource(2))
		start := time.Now()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if rate > 0 {
				// Open loop: the next commit is due when the schedule says, whatever the last one took.
				if due := start.Add(time.Duration(updates) * time.Second / time.Duration(rate)); time.Until(due) > 0 {
					time.Sleep(time.Until(due))
				}
			}
			t0 := time.Now()
			w := c.Begin()
			for k := 0; k < 2; k++ {
				w.PatchAttrs(benchQueueKey(r.Intn(55000)), benchQueuePatch(r, now), nil)
			}
			w.Commit()
			writeLat = append(writeLat, time.Since(t0))
			updates += 2
		}
	}()
	// readLat[g][s] is reader g's latencies for shape s.
	readLat := make([][][]time.Duration, 4)
	for g := 0; g < 4; g++ {
		readLat[g] = make([][]time.Duration, len(qs))
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			start := time.Now()
			for i := g; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if readRate > 0 {
					n := i - g // this reader's reads so far; each reader takes a quarter of readRate
					if due := start.Add(time.Duration(n) * 4 * time.Second / time.Duration(readRate)); time.Until(due) > 0 {
						time.Sleep(time.Until(due))
					}
				}
				s := i % len(qs)
				t0 := time.Now()
				for range c.QueryRawWireStats(qs[s], []string{"ClusterId"}, false, nil) {
				}
				readLat[g][s] = append(readLat[g][s], time.Since(t0))
			}
		}(g)
	}
	time.Sleep(dur)
	close(stop)
	wg.Wait()
	b.StopTimer()
	pct := func(d []time.Duration, p float64) float64 {
		slices.Sort(d)
		return float64(d[min(len(d)-1, int(p*float64(len(d))))].Microseconds()) / 1000
	}
	b.ReportMetric(float64(updates)/dur.Seconds(), "updates/s")
	b.ReportMetric(pct(writeLat, 0.50), "commit-p50-ms")
	b.ReportMetric(pct(writeLat, 0.99), "commit-p99-ms")
	var all []time.Duration
	for s, shape := range benchQueueShapes(now) {
		var reads []time.Duration
		for g := range readLat {
			reads = append(reads, readLat[g][s]...)
		}
		all = append(all, reads...)
		b.ReportMetric(pct(reads, 0.50), shape.name+"-p50-ms")
		b.ReportMetric(pct(reads, 0.99), shape.name+"-p99-ms")
	}
	b.ReportMetric(float64(len(all))/dur.Seconds(), "reads/s")
	b.ReportMetric(pct(all, 0.50), "read-p50-ms")
	b.ReportMetric(pct(all, 0.99), "read-p99-ms")
	b.ReportMetric(float64(IndexCatchUps()-catchUps0)/dur.Seconds(), "catchups/s")
}

// BenchmarkExtendSegIndex is one catch-up of one shard's write target at a production queue's
// size: 3,500 jobs (55k across 16 shards) written in full (about 4.5 MiB), then updated until the
// segment reaches 8 MiB, the default segment size, then extended by a 4 KiB tail. "build" is the
// full rebuild it replaces.
func BenchmarkExtendSegIndex(b *testing.B) {
	for _, fill := range []int{0, 8 << 20} {
		name := "full-records"
		if fill > 0 {
			name = "with-updates-8MiB"
		}
		b.Run(name, func(b *testing.B) {
			c, err := Open(Options{Dir: b.TempDir(), Shards: 1, DeltaMax: 16, SegmentSize: 64 << 20,
				ValueAttrs:       []string{"JobStatus", "EnteredCurrentStatus", "QDate", "CompletionDate", "ClusterId"},
				CategoricalAttrs: []string{"Owner"}})
			if err != nil {
				b.Fatal(err)
			}
			defer c.Close()
			r := rand.New(rand.NewSource(1))
			const jobs = 3500
			for j := 0; j < jobs; j++ {
				if err := c.Put(benchQueueKey(j), benchQueueAd(j, benchQueueNow, r)); err != nil {
					b.Fatal(err)
				}
			}
			seg := c.shards[0].act
			for seg.used < fill {
				w := c.Begin()
				for k := 0; k < 100; k++ {
					w.PatchAttrs(benchQueueKey(r.Intn(jobs)), benchQueuePatch(r, benchQueueNow), nil)
				}
				w.Commit()
			}
			spec := c.spec.Load()
			ends := recordEnds(seg, seg.used)
			cut := sort.SearchInts(ends, seg.used-indexCatchUpBytes)
			base := buildSegIndex(c, seg, ends[cut], spec)
			b.Logf("records=%d tail=%d records, %d bytes", len(ends), len(ends)-cut-1, seg.used-ends[cut])
			b.Run("extend", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					extendSegIndex(c, seg, base, seg.used, spec)
				}
			})
			b.Run("build", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					buildSegIndex(c, seg, seg.used, spec)
				}
			})
		})
	}
}

// BenchmarkCatchUpAfterUpdates is one catchUpIndexes pass -- the time it holds reindexMu, and what
// it allocates -- after a burst of job updates spread over the 16 shards of a 55k-job store: 300
// updates is one second of a busy queue's writes, 3000 ten seconds with no indexed read between.
func BenchmarkCatchUpAfterUpdates(b *testing.B) {
	c, now := openBenchQueue(b, 55000, 2)
	c.Reindex()
	r := rand.New(rand.NewSource(3))
	for _, burst := range []int{300, 3000} {
		b.Run(fmt.Sprintf("updates=%d", burst), func(b *testing.B) {
			b.ReportAllocs()
			var segs int64
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				for k := 0; k < burst; k += 2 {
					w := c.Begin()
					w.PatchAttrs(benchQueueKey(r.Intn(55000)), benchQueuePatch(r, now), nil)
					w.PatchAttrs(benchQueueKey(r.Intn(55000)), benchQueuePatch(r, now), nil)
					w.Commit()
				}
				n0 := IndexCatchUps()
				b.StartTimer()
				c.catchUpIndexes()
				segs += IndexCatchUps() - n0
			}
			b.ReportMetric(float64(segs)/float64(b.N), "segments/op")
		})
	}
}
