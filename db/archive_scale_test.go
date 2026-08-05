package db

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Scale profile for the history archive.
//
// What this establishes, and what it corrected.
//
// The index-resident GROUP BY was originally claimed to be O(segments x distinct values) and
// independent of record count. Measurement says otherwise: it is LINEAR in archive size, like
// the scan it replaces, just ~300x cheaper. The original claim came from a microbenchmark
// whose configurations were too small and too warm to separate the terms.
//
// Profiling the path shows why. ~87% of its time is countAttrRange -- decompressing the
// records of the UNSEALED active segment, which by design carries no sidecar index -- and
// ~89% of that is zstd. Reading the actual index (catCanonicalValues) is ~13%. So the real
// cost model is:
//
//	per query ~= O(segments x ndv)      // index reads: the cheap part
//	          +  O(SegmentSize)          // rescan of the open segment: the dominant part
//
// The second term is paid on EVERY query and is bounded by segment size, not archive size.
// That is an independent argument for keeping segments small, and it points at the obvious
// next optimization: cache or incrementally index the active segment's contribution, which
// would remove ~87% of the cost.
//
// Sweeping record count at fixed segment size and segment size at fixed record count is what
// separates the two terms; either sweep alone is confounded, because at fixed segment size
// the segment count is proportional to the record count.
//
// Skipped unless CLASSAD_SCALE is set (it builds and queries multi-hundred-MB archives):
//
//	CLASSAD_SCALE=1 go test -run TestArchiveScaleProfile -v -timeout 60m ./db/
//
// CLASSAD_SCALE=N multiplies the record counts, so the same harness can be pointed at a
// realistic corpus size on a machine that has the disk for it.

// scaleAd renders one history-shaped record: a spread of typed attributes plus padding, so
// bytes/record is in the range a real completed-job ad occupies rather than a toy.
func scaleAd(i int, ndv int) string {
	return fmt.Sprintf(
		"ClusterId = %d\nProcId = 0\nOwner = %q\nAccountingGroup = %q\n"+
			"CompletionDate = %d\nEnteredHistoryTime = %d\nJobStatus = 4\nExitCode = %d\n"+
			"RequestMemory = %d\nRequestCpus = %d\nRequestDisk = %d\nNumJobStarts = %d\n"+
			"RemoteWallClockTime = %d.0\nBytesSent = %d\nBytesRecvd = %d\n"+
			"GlobalJobId = %q\nCmd = \"/usr/bin/payload\"\nIwd = \"/home/user/work\"\n"+
			"Args = %q\n",
		i, fmt.Sprintf("user%04d", i%ndv), fmt.Sprintf("grp%02d", i%16),
		1700000000+i, 1700000005+i, i%3,
		1024+(i%8)*512, 1+(i%4), 10240+(i%16)*1024, 1+(i%3),
		60+(i%3600), 1<<20+i, 1<<19+i,
		fmt.Sprintf("sched.example.org#%d.0#%d", i, 1700000000+i),
		strings.Repeat("x", 120),
	)
}

type scaleResult struct {
	group     string
	records   int
	segSize   int
	segments  int
	diskBytes int64
	buildMS   float64
	reopenMS  float64
	heapMB    float64
	indexedMS float64 // GROUP BY on the categorically indexed attribute (index-resident)
	scanMS    float64 // GROUP BY on an identical-cardinality UNindexed attribute (scan)
	groups    int
}

func TestArchiveScaleProfile(t *testing.T) {
	if os.Getenv("CLASSAD_SCALE") == "" {
		t.Skip("set CLASSAD_SCALE=1 (or N) to run the archive scale profile")
	}
	mult, err := strconv.Atoi(os.Getenv("CLASSAD_SCALE"))
	if err != nil || mult < 1 {
		mult = 1
	}

	const ndv = 64
	// Two sweeps. Varying records at fixed segment size is the deployment-realistic axis;
	// varying segment size at fixed records isolates the active-segment rescan, which is
	// the term that dominates and the one segment size controls.
	cases := []struct {
		records, segSize int
		group            string
	}{
		// Fixed segment SIZE, 4x records: the deployment-realistic axis. Segment count
		// grows with the corpus, so both paths grow -- what must hold is the gap between
		// them.
		{100_000 * mult, 1 << 20, "fixed-segsize"},
		{200_000 * mult, 1 << 20, "fixed-segsize"},
		{400_000 * mult, 1 << 20, "fixed-segsize"},
		// Fixed records, varying segment count: isolates the per-segment cost.
		{400_000 * mult, 2 << 20, "fixed-records"},
		{400_000 * mult, 8 << 20, "fixed-records"},
		{400_000 * mult, 64 << 20, "fixed-records"},
	}

	var out []scaleResult
	for _, c := range cases {
		r := runScaleCase(t, c.records, c.segSize, ndv)
		r.group = c.group
		out = append(out, r)
	}

	t.Log("")
	t.Logf("%-15s %10s %8s %9s %10s %9s %9s %11s %10s %8s",
		"sweep", "records", "segSize", "segments", "disk", "reopen", "heap", "indexed", "scan", "speedup")
	for _, r := range out {
		speed := 0.0
		if r.indexedMS > 0 {
			speed = r.scanMS / r.indexedMS
		}
		t.Logf("%-15s %10d %8s %9d %10s %8.1fms %8.1fMB %10.2fms %8.0fms %7.1fx",
			r.group, r.records, humanSize(int64(r.segSize)), r.segments, humanSize(r.diskBytes),
			r.reopenMS, r.heapMB, r.indexedMS, r.scanMS, speed)
	}
	t.Log("")

	// The load-bearing assertion is about the GAP, not the slope. Both paths are linear in
	// archive size; what the index-resident path buys is a ~300x smaller constant, and that
	// is what must not silently regress (e.g. if the fast path stopped engaging and quietly
	// fell through to the scan, the speedup would collapse to ~1x while every result stayed
	// correct).
	// A tripwire for "the fast path stopped engaging", not a performance gate: a silent
	// fall-through to the scan shows up as ~1x while every result stays correct. Set well
	// below the ~270-450x observed, because run-to-run variance on a loaded machine is
	// large (identical configurations have differed by 8x here).
	const minSpeedup = 20
	for _, r := range out {
		if r.indexedMS <= 0 {
			continue
		}
		if got := r.scanMS / r.indexedMS; got < minSpeedup {
			t.Errorf("%s records=%d segSize=%s: indexed path only %.1fx faster than the scan "+
				"(want >= %dx) -- the index-resident path is probably not engaging",
				r.group, r.records, humanSize(int64(r.segSize)), got, minSpeedup)
		}
	}

	// Report the observed slopes rather than asserting a shape, since the shape is linear.
	var lo, hi *scaleResult
	for i := range out {
		if out[i].group != "fixed-segsize" {
			continue
		}
		if lo == nil || out[i].records < lo.records {
			lo = &out[i]
		}
		if hi == nil || out[i].records > hi.records {
			hi = &out[i]
		}
	}
	if lo != nil && hi != nil && lo != hi && lo.indexedMS > 0 {
		t.Logf("fixed segment size, %.0fx records (%.2fx segments): indexed %.2fx, scan %.2fx, reopen %.2fx",
			float64(hi.records)/float64(lo.records), float64(hi.segments)/float64(lo.segments),
			hi.indexedMS/lo.indexedMS, hi.scanMS/lo.scanMS, hi.reopenMS/lo.reopenMS)
	}
}

func runScaleCase(t *testing.T, records, segSize, ndv int) scaleResult {
	t.Helper()
	dir := t.TempDir()
	res := scaleResult{records: records, segSize: segSize}

	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Owner is categorically indexed; Owner2 carries the SAME values but is not indexed, so
	// grouping on it forces the scan over an identical corpus -- an honest A/B rather than a
	// comparison against a different query.
	hist, err := cat.CreateArchiveTable("history", ArchiveConfig{
		SegmentSize:      segSize,
		CategoricalAttrs: []string{"Owner"},
		ValueAttrs:       []string{"ClusterId"},
		ZoneAttrs:        []string{"CompletionDate", "EnteredHistoryTime"},
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := 0; i < records; i++ {
		ad := scaleAd(i, ndv) + fmt.Sprintf("Owner2 = %q\n", fmt.Sprintf("user%04d", i%ndv))
		if err := hist.AppendOld(ad); err != nil {
			t.Fatal(err)
		}
	}
	res.buildMS = float64(time.Since(start).Microseconds()) / 1000
	res.segments = hist.Stats().Segments
	res.diskBytes = dirBytes(t, dir)
	cat.Close()

	// Reopen from cold: recovery is documented as O(segments), and startup cost is what
	// makes a large segment count operationally expensive rather than merely untidy.
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	start = time.Now()
	cat2, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	h2, ok := cat2.ArchiveTable("history")
	if !ok {
		t.Fatal("archive not recovered")
	}
	res.reopenMS = float64(time.Since(start).Microseconds()) / 1000
	defer cat2.Close()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	// Go-heap only. Sidecars and segment data are mmap-backed and demand-paged, so they do
	// not appear here -- which is the point: what grows with segment count is the resident
	// catalog, not the index bytes.
	if after.HeapAlloc > before.HeapAlloc {
		res.heapMB = float64(after.HeapAlloc-before.HeapAlloc) / (1 << 20)
	}

	aggs := []AggSpec{{Func: AggCount, Arg: "*"}}
	start = time.Now()
	rows, err := h2.Aggregate("true", []string{"Owner"}, aggs)
	if err != nil {
		t.Fatal(err)
	}
	res.indexedMS = float64(time.Since(start).Microseconds()) / 1000
	res.groups = len(rows)
	if res.groups != ndv {
		t.Errorf("indexed GROUP BY produced %d groups, want %d", res.groups, ndv)
	}

	start = time.Now()
	rows2, err := h2.Aggregate("true", []string{"Owner2"}, aggs)
	if err != nil {
		t.Fatal(err)
	}
	res.scanMS = float64(time.Since(start).Microseconds()) / 1000
	if len(rows2) != ndv {
		t.Errorf("scan GROUP BY produced %d groups, want %d", len(rows2), ndv)
	}
	// Same corpus, same cardinality: the two paths must agree, or the speedup is measuring
	// a different answer.
	got := map[string]string{}
	for _, r := range rows {
		got[r.Group[0]] = r.Values[0]
	}
	for _, r := range rows2 {
		if got[r.Group[0]] != r.Values[0] {
			t.Errorf("group %q: indexed=%s scan=%s", r.Group[0], got[r.Group[0]], r.Values[0])
		}
	}
	return res
}

func dirBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var walk func(string, []os.DirEntry)
	walk = func(base string, es []os.DirEntry) {
		for _, e := range es {
			p := base + string(os.PathSeparator) + e.Name()
			if e.IsDir() {
				sub, err := os.ReadDir(p)
				if err == nil {
					walk(p, sub)
				}
				continue
			}
			if fi, err := e.Info(); err == nil {
				total += fi.Size()
			}
		}
	}
	walk(dir, entries)
	return total
}

func humanSize(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fM", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0fK", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%dB", b)
}
