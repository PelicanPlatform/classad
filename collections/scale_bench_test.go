package collections

import (
	"compress/gzip"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// Full-scale benchmarks over real ClassAds. By default they use the committed,
// gzipped OSPool `condor_status -l` sample shared with the vm package
// (vm/testdata/pool_sample.ads.gz). Point CLASSAD_BENCH_ADS at a larger dump to
// benchmark at real scale:
//
//	CLASSAD_BENCH_ADS=/tmp/pool.ads CLASSAD_BENCH_N=1000000 \
//	    go test . -run=^$ -bench=BenchmarkScale -benchmem
const (
	envAdsFile      = "CLASSAD_BENCH_ADS"
	envSampleSize   = "CLASSAD_BENCH_SAMPLE"
	envWorkingN     = "CLASSAD_BENCH_N"
	committedCorpus = "vm/testdata/pool_sample.ads.gz"
)

func loadCorpus(tb testing.TB) []*classad.ClassAd {
	tb.Helper()
	path := os.Getenv(envAdsFile)
	if path == "" {
		path = committedCorpus
		if _, err := os.Stat(path); err != nil {
			tb.Skipf("corpus %s not found; set %s to a `condor_status -l` dump", path, envAdsFile)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		tb.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var src io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			tb.Fatalf("gzip %s: %v", path, err)
		}
		defer gz.Close()
		src = gz
	}
	sampleMax := envInt(envSampleSize, 2000)
	var ads []*classad.ClassAd
	r := classad.NewOldReader(src)
	for r.Next() && len(ads) < sampleMax {
		ads = append(ads, r.ClassAd())
	}
	if err := r.Err(); err != nil {
		tb.Fatalf("read %s: %v", path, err)
	}
	if len(ads) == 0 {
		tb.Fatalf("no ads parsed from %s", path)
	}
	return ads
}

// populate fills a fresh collection with n ads, tiling the sample and giving each
// a unique key so the store holds n distinct records (as a real pool would).
func populate(tb testing.TB, sample []*classad.ClassAd, n int) *Collection {
	tb.Helper()
	c := New(Options{Shards: 16})
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
		batch = append(batch, AdUpdate{
			Key: []byte("ad-" + strconv.Itoa(i)),
			Ad:  sample[i%len(sample)],
		})
		if len(batch) == batchSize {
			flush()
		}
	}
	flush()
	return c
}

// TestStoreDensityReport reports the store's memory density on real ads.
func TestStoreDensityReport(t *testing.T) {
	sample := loadCorpus(t)
	n := 50000
	c := populate(t, sample, n)
	live := liveBytes(c)
	t.Logf("stored %d real ads: %d live bytes = %.0f bytes/ad (codec=%s)",
		c.Len(), live, float64(live)/float64(c.Len()), c.currentCodec().Name())
}

var benchScaleQuery = `Cpus >= 1 && Memory > 1000`

func BenchmarkScaleQuery(b *testing.B) {
	sample := loadCorpus(b)
	c := populate(b, sample, envInt(envWorkingN, 100000))
	q, err := vm.Parse(benchScaleQuery)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	var matches int
	for i := 0; i < b.N; i++ {
		matches = 0
		for range c.Query(q) {
			matches++
		}
	}
	b.StopTimer()
	perAd := float64(b.Elapsed().Nanoseconds()) / float64(b.N) / float64(c.Len())
	b.ReportMetric(perAd, "ns/ad")
	b.Logf("collection=%d ads, matches/scan=%d", c.Len(), matches)
}

// BenchmarkScaleQuerySelective uses a query that matches (almost) no ads, so
// nearly every ad is rejected on a partial decode and never fully decoded — the
// best case for the fast path and a measure of its per-ad floor.
func BenchmarkScaleQuerySelective(b *testing.B) {
	sample := loadCorpus(b)
	c := populate(b, sample, envInt(envWorkingN, 100000))
	q, err := vm.Parse(`Cpus >= 1 && Memory > 1000 && Name == "no-such-slot-xyzzy"`)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	var matches int
	for i := 0; i < b.N; i++ {
		matches = 0
		for range c.Query(q) {
			matches++
		}
	}
	b.StopTimer()
	perAd := float64(b.Elapsed().Nanoseconds()) / float64(b.N) / float64(c.Len())
	b.ReportMetric(perAd, "ns/ad")
	b.Logf("collection=%d ads, matches/scan=%d", c.Len(), matches)
}

// BenchmarkScaleQuerySelectiveHot is BenchmarkScaleQuerySelective with the
// queried attributes front-loaded in the hot header, so each is resolved in O(1)
// instead of scanning the ad body.
func BenchmarkScaleQuerySelectiveHot(b *testing.B) {
	sample := loadCorpus(b)
	c := New(Options{Shards: 16, HotAttrs: []string{"Cpus", "Memory", "Name"}})
	const batchSize = 512
	batch := make([]AdUpdate, 0, batchSize)
	n := envInt(envWorkingN, 100000)
	for i := 0; i < n; i++ {
		batch = append(batch, AdUpdate{Key: []byte("ad-" + strconv.Itoa(i)), Ad: sample[i%len(sample)]})
		if len(batch) == batchSize {
			if err := c.Update(batch); err != nil {
				b.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if err := c.Update(batch); err != nil {
			b.Fatal(err)
		}
	}
	q, err := vm.Parse(`Cpus >= 1 && Memory > 1000 && Name == "no-such-slot-xyzzy"`)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for range c.Query(q) {
		}
	}
	b.StopTimer()
	perAd := float64(b.Elapsed().Nanoseconds()) / float64(b.N) / float64(c.Len())
	b.ReportMetric(perAd, "ns/ad")
}

func BenchmarkScaleUpdate(b *testing.B) {
	sample := loadCorpus(b)
	c := New(Options{Shards: 16})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Put([]byte("ad-"+strconv.Itoa(i)), sample[i%len(sample)])
	}
}

// BenchmarkScaleChurn measures update-in-place throughput on an existing working
// set (the daemon-rewrite pattern), triggering compaction as garbage accrues.
func BenchmarkScaleChurn(b *testing.B) {
	sample := loadCorpus(b)
	n := envInt(envWorkingN, 100000)
	c := populate(b, sample, n)
	b.ReportAllocs()
	b.ResetTimer()
	compactions := 0
	for i := 0; i < b.N; i++ {
		_ = c.Put([]byte("ad-"+strconv.Itoa(i%n)), sample[i%len(sample)])
		if i%n == n-1 {
			compactions += c.Compact()
		}
	}
	b.StopTimer()
	b.Logf("compactions=%d", compactions)
}

func envInt(name string, def int) int {
	if s := os.Getenv(name); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			return v
		}
	}
	return def
}
