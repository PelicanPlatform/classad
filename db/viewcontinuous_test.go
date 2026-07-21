package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

func jobTS(t *testing.T, qdate int64) *classad.ClassAd {
	t.Helper()
	ad, err := classad.ParseOld(fmt.Sprintf("QDate = %d", qdate))
	if err != nil {
		t.Fatal(err)
	}
	return ad
}

// continuousSpec: COUNT(*) grouped by time_bucket(QDate, 1h) -- one series per bucket.
func continuousSpec() ViewSpec {
	return ViewSpec{
		BaseTable:   "jobs",
		Groups:      []ViewGroupCol{{Attr: "QDate", Alias: "time", BucketWidth: 3600}},
		Metrics:     []ViewMetric{{Func: ViewCount, Arg: "*", Alias: "metric_jobs"}},
		Cardinality: 100,
	}
}

func archiveAds(t *testing.T, v *View) []*classad.ClassAd {
	t.Helper()
	seq, err := v.archive.Query("true")
	if err != nil {
		t.Fatal(err)
	}
	var out []*classad.ClassAd
	for ad := range seq {
		out = append(out, ad)
	}
	return out
}

func seal(v *View, now int64) {
	v.mu.Lock()
	v.sealAged(now)
	v.mu.Unlock()
}

func waitLateDrops(t *testing.T, v *View, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if v.LateDrops() == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("lateDrops = %d, want %d (timed out)", v.LateDrops(), want)
}

func TestContinuousAggregateSealAndEvict(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	base, _ := cat.CreateTable("jobs")
	base.Put("1", jobTS(t, 3600)) // bucket 3600
	base.Put("2", jobTS(t, 3700)) // bucket 3600
	base.Put("3", jobTS(t, 7200)) // bucket 7200
	if err := cat.CreateView("jobs_ts", continuousSpec()); err != nil {
		t.Fatal(err)
	}
	waitSeries(t, cat, "jobs_ts", 2)
	v, _ := cat.View("jobs_ts")

	// Seal bucket 3600 (now=7200 => seal starts <= 7200-3600-0 = 3600); 7200 stays live.
	seal(v, 7200)
	if v.SeriesCount() != 1 {
		t.Fatalf("series after first seal = %d, want 1", v.SeriesCount())
	}
	arch := archiveAds(t, v)
	if len(arch) != 1 {
		t.Fatalf("archive = %d, want 1", len(arch))
	}
	if ts, _ := arch[0].EvaluateAttrInt("time"); ts != 3600 {
		t.Errorf("sealed time = %d, want 3600", ts)
	}
	if n, _ := arch[0].EvaluateAttrInt("metric_jobs"); n != 2 {
		t.Errorf("sealed count = %d, want 2", n)
	}

	// Late data into the sealed bucket is dropped, not resurrected.
	base.Put("4", jobTS(t, 3650))
	waitLateDrops(t, v, 1)
	if v.SeriesCount() != 1 {
		t.Fatalf("late data resurrected a bucket: series = %d, want 1", v.SeriesCount())
	}

	// Seal bucket 7200 too.
	seal(v, 10800)
	if v.SeriesCount() != 0 {
		t.Fatalf("series after second seal = %d, want 0", v.SeriesCount())
	}
	if got := len(archiveAds(t, v)); got != 2 {
		t.Fatalf("archive = %d, want 2", got)
	}
}

func TestContinuousAggregateReloadNoDuplicate(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := cat.CreateTable("jobs")
	base.Put("1", jobTS(t, 3600))
	base.Put("2", jobTS(t, 3700))
	base.Put("3", jobTS(t, 7200))
	if err := cat.CreateView("jobs_ts", continuousSpec()); err != nil {
		t.Fatal(err)
	}
	waitSeries(t, cat, "jobs_ts", 2)
	v, _ := cat.View("jobs_ts")
	seal(v, 10800) // seal both buckets -> archive 2, watermark 7200
	if got := len(archiveAds(t, v)); got != 2 {
		t.Fatalf("archive before reload = %d, want 2", got)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: the watermark is loaded, so the rebuild's replay drops both sealed buckets;
	// the archive keeps exactly the two sealed samples (no duplicate appends).
	cat2, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat2.Close()
	v2, ok := cat2.View("jobs_ts")
	if !ok {
		t.Fatal("view did not survive reload")
	}
	if v2.SeriesCount() != 0 {
		t.Fatalf("reload live series = %d, want 0 (all buckets sealed)", v2.SeriesCount())
	}
	if got := len(archiveAds(t, v2)); got != 2 {
		t.Fatalf("archive after reload = %d, want 2 (no duplicate append)", got)
	}
}
