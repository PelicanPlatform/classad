package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// buildMatchPool builds an indexed slot pool for match benchmarks: n slots across
// 8 shards, indexed on the attributes a job Requirements constrains.
func buildMatchPool(tb testing.TB, n int) *Collection {
	tb.Helper()
	c := New(Options{Shards: 8, CategoricalAttrs: []string{"Arch"}, ValueAttrs: []string{"Cpus", "Memory"}})
	for i := 0; i < n; i++ {
		ad, err := classad.Parse(fmt.Sprintf(
			`[ Arch="X86_64"; Cpus=%d; Memory=%d; State="Unclaimed"; Requirements=true ]`,
			(i%16)+1, ((i%32)+1)*512))
		if err != nil {
			tb.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("slot%d", i)), ad); err != nil {
			tb.Fatal(err)
		}
	}
	c.Reindex()
	return c
}

// homogeneousJob is the identical job every iteration matches — the condor_submit
// "queue 10000" case: same Requirements and same resource requests across the batch.
func homogeneousJob(tb testing.TB) *classad.ClassAd {
	tb.Helper()
	job, err := classad.Parse(
		`[ RequestCpus=8; RequestMemory=4096; Owner="alice";
		   Requirements = TARGET.Arch=="X86_64" && TARGET.Cpus>=RequestCpus && TARGET.Memory>=RequestMemory ]`)
	if err != nil {
		tb.Fatal(err)
	}
	return job
}

// BenchmarkMatchHomogeneous matches the same job against the pool repeatedly. With
// -cpuprofile it shows where a per-job match spends time: the once-per-job compile
// + plan (cacheable across a homogeneous batch) vs jobValues + the candidate
// scan/verify (not cacheable).
func BenchmarkMatchHomogeneous(b *testing.B) {
	c := buildMatchPool(b, 20000)
	job := homogeneousJob(b)
	b.ReportAllocs()
	b.ResetTimer()
	var matches int
	for i := 0; i < b.N; i++ {
		matches = 0
		for range c.Match(job) {
			matches++
		}
	}
	b.StopTimer()
	if matches == 0 {
		b.Fatal("expected matches")
	}
	b.ReportMetric(float64(matches), "matches")
}

// selectiveJob constrains an indexed value so only a small slice of the pool is a
// candidate — the extreme where the candidate scan is small and per-job planning
// is its largest possible share of a match.
func selectiveJob(tb testing.TB, minCpus int) *classad.ClassAd {
	tb.Helper()
	job, err := classad.Parse(fmt.Sprintf(
		`[ RequestCpus=%d; Owner="alice";
		   Requirements = TARGET.Arch=="X86_64" && TARGET.Cpus>=RequestCpus ]`, minCpus))
	if err != nil {
		tb.Fatal(err)
	}
	return job
}

// BenchmarkMatchSelective matches a job that only ~1/16 of slots (Cpus==16) can
// satisfy, so the index narrows hard and the candidate scan is small — the best
// case for a plan cache to matter.
func BenchmarkMatchSelective(b *testing.B) {
	c := buildMatchPool(b, 20000)
	job := selectiveJob(b, 16)
	b.ReportAllocs()
	b.ResetTimer()
	var matches int
	for i := 0; i < b.N; i++ {
		matches = 0
		for range c.Match(job) {
			matches++
		}
	}
	b.StopTimer()
	if matches == 0 {
		b.Fatal("expected matches")
	}
	b.ReportMetric(float64(matches), "matches")
}

// BenchmarkMatchSortedLimit1 is the negotiator's shape: pick the single best slot.
// The job has no Rank, so all that is needed is any one survivor -- but MatchSorted
// currently materializes every survivor (FromAST) before truncating to the limit.
func BenchmarkMatchSortedLimit1(b *testing.B) {
	c := buildMatchPool(b, 20000)
	job := homogeneousJob(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := c.MatchSorted(job, 1); len(got) != 1 {
			b.Fatalf("want 1 match, got %d", len(got))
		}
	}
}

// BenchmarkMatchPlanOnly isolates just the per-job planning (compileJobSide +
// matchIndexPlan) with no candidate scan, so the cacheable fraction is measured
// directly against BenchmarkMatchHomogeneous.
func BenchmarkMatchPlanOnly(b *testing.B) {
	c := buildMatchPool(b, 20000)
	job := homogeneousJob(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, jobVals := c.compileJobSide(job)
		_ = c.matchIndexPlan(job, jobVals)
	}
}
