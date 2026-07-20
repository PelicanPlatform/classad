package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

func viewJob(t *testing.T, owner string, mem int) *classad.ClassAd {
	t.Helper()
	ad, err := classad.ParseOld(fmt.Sprintf("Owner = %q\nRequestMemory = %d", owner, mem))
	if err != nil {
		t.Fatal(err)
	}
	return ad
}

// clusterUsageSpec is the running example: COUNT(*), SUM(RequestMemory), AVG(RequestMemory)
// grouped by Owner.
func clusterUsageSpec() ViewSpec {
	return ViewSpec{
		BaseTable: "jobs",
		Groups:    []ViewGroupCol{{Attr: "Owner", Alias: "label_owner"}},
		Metrics: []ViewMetric{
			{Func: ViewCount, Arg: "*", Alias: "metric_jobs"},
			{Func: ViewSum, Arg: "RequestMemory", Alias: "metric_mem"},
			{Func: ViewAvg, Arg: "RequestMemory", Alias: "metric_avg"},
		},
		Cardinality: 100,
	}
}

// viewGroup reads the materialized row for a group key from the view backing.
func viewGroup(t *testing.T, cat *Catalog, view, groupKey string) (*classad.ClassAd, bool) {
	t.Helper()
	b, ok := cat.ViewBacking(view)
	if !ok {
		t.Fatalf("view %q has no backing", view)
	}
	return b.LookupClassAd(groupKey)
}

// waitSeries polls until the view reports want series (or fails after a timeout) -- live
// maintenance is asynchronous.
func waitSeries(t *testing.T, cat *Catalog, view string, want int) {
	t.Helper()
	v, ok := cat.View(view)
	if !ok {
		t.Fatalf("no view %q", view)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if v.SeriesCount() == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("view %q series = %d, want %d (timed out)", view, v.SeriesCount(), want)
}

func TestViewBuildAndQuery(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	base, err := cat.CreateTable("jobs")
	if err != nil {
		t.Fatal(err)
	}
	// alice: 2 jobs (100, 300); bob: 1 job (200).
	if err := base.Put("1", viewJob(t, "alice", 100)); err != nil {
		t.Fatal(err)
	}
	if err := base.Put("2", viewJob(t, "alice", 300)); err != nil {
		t.Fatal(err)
	}
	if err := base.Put("3", viewJob(t, "bob", 200)); err != nil {
		t.Fatal(err)
	}

	if err := cat.CreateView("cluster_usage", clusterUsageSpec()); err != nil {
		t.Fatalf("CreateView: %v", err)
	}

	// Build is synchronous, so the two groups are materialized immediately.
	v, _ := cat.View("cluster_usage")
	if got := v.SeriesCount(); got != 2 {
		t.Fatalf("series = %d, want 2", got)
	}
	alice, ok := viewGroup(t, cat, "cluster_usage", "alice")
	if !ok {
		t.Fatal("missing alice group")
	}
	if n, _ := alice.EvaluateAttrInt("metric_jobs"); n != 2 {
		t.Errorf("alice jobs = %d, want 2", n)
	}
	if s, _ := alice.EvaluateAttrReal("metric_mem"); s != 400 {
		t.Errorf("alice mem = %v, want 400", s)
	}
	if a, _ := alice.EvaluateAttrReal("metric_avg"); a != 200 {
		t.Errorf("alice avg = %v, want 200", a)
	}
	if o, _ := alice.EvaluateAttrString("label_owner"); o != "alice" {
		t.Errorf("alice label = %q", o)
	}
	if st, _ := v.State(); st != ViewActive {
		t.Errorf("state = %v, want active", st)
	}
}

func TestViewLiveUpsertUpdateDelete(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	base, _ := cat.CreateTable("jobs")
	base.Put("1", viewJob(t, "alice", 100))
	base.Put("2", viewJob(t, "bob", 200))
	if err := cat.CreateView("cluster_usage", clusterUsageSpec()); err != nil {
		t.Fatal(err)
	}
	waitSeries(t, cat, "cluster_usage", 2)

	// Insert a new alice job -> alice count 1->2, mem 100->400.
	base.Put("3", viewJob(t, "alice", 300))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a, ok := viewGroup(t, cat, "cluster_usage", "alice"); ok {
			if n, _ := a.EvaluateAttrInt("metric_jobs"); n == 2 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if a, _ := viewGroup(t, cat, "cluster_usage", "alice"); func() int64 { n, _ := a.EvaluateAttrInt("metric_jobs"); return n }() != 2 {
		t.Fatal("live insert did not update alice count to 2")
	}

	// Update job 2's owner bob->carol: bob group evicted, carol appears (series stays 2, so
	// poll for the actual transition rather than the count).
	base.Put("2", viewJob(t, "carol", 200))
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, bobOK := viewGroup(t, cat, "cluster_usage", "bob")
		_, carolOK := viewGroup(t, cat, "cluster_usage", "carol")
		if !bobOK && carolOK {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := viewGroup(t, cat, "cluster_usage", "bob"); ok {
		t.Error("bob group should be gone after its only member moved to carol")
	}
	if _, ok := viewGroup(t, cat, "cluster_usage", "carol"); !ok {
		t.Error("carol group should exist after the update")
	}

	// Delete carol's only job: carol evicted -> 1 series.
	if _, err := base.Delete("2"); err != nil {
		t.Fatal(err)
	}
	waitSeries(t, cat, "cluster_usage", 1)
	if _, ok := viewGroup(t, cat, "cluster_usage", "carol"); ok {
		t.Error("carol group should be evicted after deleting its last member")
	}
}

func TestViewCardinalityHardError(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	base, _ := cat.CreateTable("jobs")
	for i := 0; i < 5; i++ {
		base.Put(fmt.Sprintf("%d", i), viewJob(t, fmt.Sprintf("owner%d", i), 10))
	}
	spec := clusterUsageSpec()
	spec.Cardinality = 3 // 5 distinct owners > 3
	if err := cat.CreateView("cluster_usage", spec); err == nil {
		t.Fatal("CreateView should fail when the base exceeds the cardinality limit")
	}
	if _, ok := cat.View("cluster_usage"); ok {
		t.Fatal("a view that failed its cardinality build must not be registered")
	}
}

func TestViewReloadRebuilds(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := cat.CreateTable("jobs")
	base.Put("1", viewJob(t, "alice", 100))
	base.Put("2", viewJob(t, "bob", 200))
	if err := cat.CreateView("cluster_usage", clusterUsageSpec()); err != nil {
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: the view definition is persisted; its data is rebuilt from the base table.
	cat2, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat2.Close()
	if names := cat2.Views(); len(names) != 1 || names[0] != "cluster_usage" {
		t.Fatalf("views after reload = %v, want [cluster_usage]", names)
	}
	v, _ := cat2.View("cluster_usage")
	if st, _ := v.State(); st != ViewActive {
		t.Fatalf("reloaded view state = %v, want active", st)
	}
	if v.SeriesCount() != 2 {
		t.Fatalf("reloaded series = %d, want 2", v.SeriesCount())
	}
	if a, ok := viewGroup(t, cat2, "cluster_usage", "alice"); !ok || func() int64 { n, _ := a.EvaluateAttrInt("metric_jobs"); return n }() != 1 {
		t.Fatal("reloaded alice group missing/wrong")
	}
}

func TestViewSpecValidate(t *testing.T) {
	good := clusterUsageSpec()
	if err := good.Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	bad := []ViewSpec{
		{BaseTable: "", Groups: good.Groups, Metrics: good.Metrics, Cardinality: 1},
		{BaseTable: "jobs", Metrics: good.Metrics, Cardinality: 1},                                                                  // no groups
		{BaseTable: "jobs", Groups: good.Groups, Cardinality: 1},                                                                    // no metrics
		{BaseTable: "jobs", Groups: good.Groups, Metrics: good.Metrics},                                                             // cardinality 0
		{BaseTable: "jobs", Groups: good.Groups, Metrics: []ViewMetric{{Func: "min", Arg: "x", Alias: "metric_x"}}, Cardinality: 1}, // MIN unsupported
	}
	for i, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("bad spec %d passed validation", i)
		}
	}
}
