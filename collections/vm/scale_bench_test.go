package vm

import (
	"compress/gzip"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// Full-scale benchmark over real ClassAds.
//
// By default it runs against a committed, gzipped 197-ad sample of a real OSPool
// `condor_status -any -l` dump (testdata/pool_sample.ads.gz, ~490KB). The sample
// is proportional by ad type — Machine/StartD/DaemonMaster in their real ratios,
// plus a floor of 2 per rare daemon type (Scheduler, Collector, Negotiator, ...)
// so every ad type is represented. This makes the benchmark reproducible with no
// setup. To benchmark against a full pool dump instead, point CLASSAD_BENCH_ADS
// at it:
//
//	condor_status -any -pool cm-1.ospool.osg-htc.org -l > /tmp/pool.ads  # ~1.4GB, ~4GB RAM
//	CLASSAD_BENCH_ADS=/tmp/pool.ads CLASSAD_BENCH_SAMPLE=5000 \
//	    go test ./vm/ -run=^$ -bench=BenchmarkScale -benchmem
//
// A path ending in .gz is read through gzip. The harness reads up to
// CLASSAD_BENCH_SAMPLE distinct ads (default 2000) and tiles them to a working
// set of CLASSAD_BENCH_N ads (default 100000) for the scan.
const (
	envAdsFile      = "CLASSAD_BENCH_ADS"
	envSampleSize   = "CLASSAD_BENCH_SAMPLE"
	envWorkingN     = "CLASSAD_BENCH_N"
	committedCorpus = "testdata/pool_sample.ads.gz"
)

func loadSampleAds(b *testing.B) []*classad.ClassAd {
	b.Helper()
	path := os.Getenv(envAdsFile)
	if path == "" {
		path = committedCorpus
	}
	f, err := os.Open(path)
	if err != nil {
		b.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var src io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			b.Fatalf("gzip %s: %v", path, err)
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
		b.Fatalf("read %s: %v", path, err)
	}
	if len(ads) == 0 {
		b.Fatalf("no ads parsed from %s", path)
	}
	return ads
}

// buildWorkingSet expands sample to n ads by tiling. Evaluation is read-only, so
// the copies safely share ad pointers; the M2 query reads only Cpus/Memory/Arch,
// which uniqueness would not change. The unique-Key + churn-Counter duplication
// the user described is for the M3 collection scan (where distinct stored ads
// matter) and is added with that benchmark.
func buildWorkingSet(sample []*classad.ClassAd, n int) []*classad.ClassAd {
	if n <= len(sample) {
		return sample[:n]
	}
	out := make([]*classad.ClassAd, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, sample[i%len(sample)])
	}
	return out
}

// benchScaleQuery is a representative machine constraint evaluated over every ad.
const benchScaleQuery = `Cpus >= 1 && Memory > 1024 && (Arch == "X86_64" || Arch == "x86_64")`

func BenchmarkScaleVM(b *testing.B) {
	sample := loadSampleAds(b)
	ws := buildWorkingSet(sample, envInt(envWorkingN, 100000))
	q, err := Parse(benchScaleQuery)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	var matches int
	for i := 0; i < b.N; i++ {
		for _, ad := range ws {
			if q.Matches(ad) {
				matches++
			}
		}
	}
	b.StopTimer()
	perAd := float64(b.Elapsed().Nanoseconds()) / float64(b.N) / float64(len(ws))
	b.ReportMetric(perAd, "ns/ad")
	b.Logf("working set=%d ads, matches/scan=%d", len(ws), matches/max(b.N, 1))
}

// BenchmarkScaleMatcher is the compiled interpreter with a reused Matcher (one
// evaluator + stack for the whole scan), the intended way to run a query over a
// collection.
func BenchmarkScaleMatcher(b *testing.B) {
	sample := loadSampleAds(b)
	ws := buildWorkingSet(sample, envInt(envWorkingN, 100000))
	q, err := Parse(benchScaleQuery)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	var matches int
	for i := 0; i < b.N; i++ {
		m := q.Matcher()
		for _, ad := range ws {
			if m.Matches(ad) {
				matches++
			}
		}
	}
	b.StopTimer()
	perAd := float64(b.Elapsed().Nanoseconds()) / float64(b.N) / float64(len(ws))
	b.ReportMetric(perAd, "ns/ad")
	b.Logf("working set=%d ads, matches/scan=%d", len(ws), matches/max(b.N, 1))
}

func BenchmarkScaleTreeWalk(b *testing.B) {
	sample := loadSampleAds(b)
	ws := buildWorkingSet(sample, envInt(envWorkingN, 100000))
	expr, err := parser.ParseExpr(benchScaleQuery)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	var matches int
	for i := 0; i < b.N; i++ {
		for _, ad := range ws {
			v := classad.NewEvaluator(ad).Evaluate(expr)
			if bv, err := v.BoolValue(); err == nil && bv {
				matches++
			}
		}
	}
	b.StopTimer()
	perAd := float64(b.Elapsed().Nanoseconds()) / float64(b.N) / float64(len(ws))
	b.ReportMetric(perAd, "ns/ad")
	b.Logf("working set=%d ads, matches/scan=%d", len(ws), matches/max(b.N, 1))
}

func envInt(name string, def int) int {
	if s := os.Getenv(name); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			return v
		}
	}
	return def
}
