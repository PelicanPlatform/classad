package collections

import (
	"os"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// populatePersistent fills a fresh persistent collection under dir with n ads,
// tiling the sample with unique keys (as populate does for the in-memory store).
func populatePersistent(tb testing.TB, sample []*classad.ClassAd, n int, dir string) *Collection {
	tb.Helper()
	c, err := Open(Options{Shards: 16, Dir: dir})
	if err != nil {
		tb.Fatal(err)
	}
	const batchSize = 512
	batch := make([]AdUpdate, 0, batchSize)
	flush := func() {
		if len(batch) > 0 {
			if err := c.Update(batch); err != nil {
				tb.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	for i := 0; i < n; i++ {
		batch = append(batch, AdUpdate{Key: []byte("ad-" + strconv.Itoa(i)), Ad: sample[i%len(sample)]})
		if len(batch) == batchSize {
			flush()
		}
	}
	flush()
	return c
}

// benchmarkPersistentPut measures strict-msync Put throughput on a single-shard
// persistent collection. Every committed batch is msync'd before Put returns; group
// commit coalesces concurrent writers into one msync, so ads/sync (the coalescing
// factor) climbs with -cpu and per-ad cost drops far below one msync each. Run with
// -cpu=1,4,16 to see the amortization the strict-durability design relies on.
func benchmarkPersistentPut(b *testing.B) {
	dir := b.TempDir()
	var syncs int64
	c, err := Open(Options{Shards: 1, Dir: dir, CommitSync: func() { atomic.AddInt64(&syncs, 1) }})
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	ad := mustAd(b, `[MyType="Machine"; Cpus=8; Memory=16384; Arch="X86_64"; State="Unclaimed"; Rank=Cpus*10]`)
	_ = ad.AST() // sort once up front (shared ad; see benchmarkConcurrentPut)
	var w int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		id := atomic.AddInt64(&w, 1)
		prefix := "w" + strconv.FormatInt(id, 10) + "-"
		k := 0
		for pb.Next() {
			_ = c.Put([]byte(prefix+strconv.Itoa(k&1023)), ad)
			k++
		}
	})
	b.StopTimer()
	if s := atomic.LoadInt64(&syncs); s > 0 {
		b.ReportMetric(float64(b.N)/float64(s), "ads/sync")
	}
}

func BenchmarkPersistentPutMsync(b *testing.B) { benchmarkPersistentPut(b) }

// BenchmarkPersistentRecovery measures how long reopening (recovering) a persistent
// collection takes as a function of its size: recovery mmaps every segment, scans
// records to rebuild the per-shard directory (max-seq per key), and enforces the
// single-current-version invariant. Reports µs per recovered ad.
func BenchmarkPersistentRecovery(b *testing.B) {
	sample := loadCorpus(b)
	n := envInt(envWorkingN, 50000)
	dir := b.TempDir()
	c := populatePersistent(b, sample, n, dir)
	live := c.Len()
	if err := c.Close(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rc, err := Open(Options{Shards: 16, Dir: dir})
		if err != nil {
			b.Fatal(err)
		}
		if rc.Len() != live {
			b.Fatalf("recovered %d, want %d", rc.Len(), live)
		}
		b.StopTimer()
		rc.Close()
		b.StartTimer()
	}
	b.StopTimer()
	perAd := float64(b.Elapsed().Microseconds()) / float64(b.N) / float64(live)
	b.ReportMetric(perAd, "µs/ad")
	b.Logf("recovered %d ads/reopen", live)
}

// TestPersistentDensityReport compares on-arena density (compressed live bytes per
// ad) of the persistent inline-names encoding against the in-memory interned
// encoding, both after training a ZSTD dictionary — the measurement the plan calls
// for (dropping interning for self-contained records should stay competitive once a
// dictionary recovers the shared attribute names).
func TestPersistentDensityReport(t *testing.T) {
	t.Parallel()
	sample := loadCorpus(t)
	// The inline-vs-interned density ratio is per-ad and holds at a few thousand ads;
	// a dictionary trains just as well on this many, so we avoid a 50k-ad store.
	n := 5000

	// Interned (in-memory) + dict.
	ci := populate(t, sample, n)
	if _, err := ci.RetrainDict(4000); err != nil {
		t.Fatalf("interned RetrainDict: %v", err)
	}
	internedBytes := liveBytes(ci)

	// Inline (persistent) + dict.
	dir := t.TempDir()
	cp := populatePersistent(t, sample, n, dir)
	defer cp.Close()
	if _, err := cp.RetrainDict(4000); err != nil {
		t.Fatalf("inline RetrainDict: %v", err)
	}
	inlineBytes := liveBytes(cp)

	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	t.Logf("interned+dict: %.1f bytes/ad (%d ads)", float64(internedBytes)/float64(ci.Len()), ci.Len())
	t.Logf("inline+dict:   %.1f bytes/ad (%d ads)", float64(inlineBytes)/float64(cp.Len()), cp.Len())
	t.Logf("inline/interned density ratio: %.2fx", float64(inlineBytes)/float64(internedBytes))
}
