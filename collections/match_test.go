package collections

import (
	"fmt"
	"sort"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// matchCorpus builds a collection of four slots exercising every match outcome
// against the job below, and returns the job.
//
//	job: needs Memory >= 4096 and Arch X86_64; ranks by the slot's Cpus.
//	slot 1: matches (rank 16)
//	slot 2: fails the JOB's requirement  (Memory 2048 < 4096)
//	slot 3: fails the SLOT's requirement (asymmetric: rejects the job)
//	slot 4: matches (rank 32)
func matchCorpus(t *testing.T) (*Collection, *classad.ClassAd) {
	t.Helper()
	c := New(Options{Shards: 4})
	slots := map[int]string{
		1: `[ Id=1; Memory=8192;  Arch="X86_64"; Cpus=16; Requirements = TARGET.RequestMemory <= Memory ]`,
		2: `[ Id=2; Memory=2048;  Arch="X86_64"; Cpus=8;  Requirements = true ]`,
		3: `[ Id=3; Memory=8192;  Arch="X86_64"; Cpus=4;  Requirements = TARGET.RequestMemory <= 1000 ]`,
		4: `[ Id=4; Memory=16384; Arch="X86_64"; Cpus=32; Requirements = true ]`,
	}
	for id, text := range slots {
		if err := c.Put([]byte(fmt.Sprintf("slot%d", id)), mustAd(t, text)); err != nil {
			t.Fatal(err)
		}
	}
	job := mustAd(t, `[ RequestMemory = 4096;
		Requirements = TARGET.Memory >= RequestMemory && TARGET.Arch == "X86_64";
		Rank = TARGET.Cpus ]`)
	return c, job
}

func matchIDs(t *testing.T, ads []*classad.ClassAd) []int {
	t.Helper()
	var ids []int
	for _, ad := range ads {
		id, ok := ad.EvaluateAttrInt("Id")
		if !ok {
			t.Fatal("match result missing Id")
		}
		ids = append(ids, int(id))
	}
	return ids
}

func TestMatchSymmetric(t *testing.T) {
	t.Parallel()
	c, job := matchCorpus(t)
	var got []int
	for ad := range c.Match(job) {
		id, _ := ad.EvaluateAttrInt("Id")
		got = append(got, int(id))
	}
	sort.Ints(got)
	if want := []int{1, 4}; !equalInts(got, want) {
		t.Fatalf("Match = %v, want %v (2 rejected: job-req and slot-req)", got, want)
	}
}

func TestMatchSortedByRank(t *testing.T) {
	t.Parallel()
	c, job := matchCorpus(t)
	got := matchIDs(t, c.MatchSorted(job, 0))
	// slot 4 (Cpus 32) ranks above slot 1 (Cpus 16).
	if want := []int{4, 1}; !equalInts(got, want) {
		t.Fatalf("MatchSorted = %v, want %v (by descending Rank)", got, want)
	}
}

func TestMatchSortedTopK(t *testing.T) {
	t.Parallel()
	c, job := matchCorpus(t)
	got := matchIDs(t, c.MatchSorted(job, 1))
	if want := []int{4}; !equalInts(got, want) {
		t.Fatalf("MatchSorted top-1 = %v, want %v", got, want)
	}
}

// TestMatchDoesNotMutateJob checks the caller's job ad is left as it was found.
func TestMatchDoesNotMutateJob(t *testing.T) {
	t.Parallel()
	c, job := matchCorpus(t)
	before := job.GetTarget()
	for range c.Match(job) {
	}
	c.MatchSorted(job, 0)
	if job.GetTarget() != before {
		t.Fatal("Match/MatchSorted left a target set on the caller's job ad")
	}
}

// TestMatchNoRequirements: a slot without a Requirements attribute cannot match
// (symmetry needs both sides true).
func TestMatchNoRequirements(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 2})
	_ = c.Put([]byte("s1"), mustAd(t, `[ Id=1; Memory=8192; Arch="X86_64"; Cpus=8 ]`)) // no Requirements
	job := mustAd(t, `[ RequestMemory=4096; Requirements = TARGET.Memory >= RequestMemory; Rank = TARGET.Cpus ]`)
	n := 0
	for range c.Match(job) {
		n++
	}
	if n != 0 {
		t.Fatalf("a slot without Requirements should not match; got %d matches", n)
	}
}

// TestMatchNilJob is a defensive no-op.
func TestMatchNilJob(t *testing.T) {
	t.Parallel()
	c, _ := matchCorpus(t)
	for range c.Match(nil) {
		t.Fatal("Match(nil) should yield nothing")
	}
	if got := c.MatchSorted(nil, 0); got != nil {
		t.Fatalf("MatchSorted(nil) = %v, want nil", got)
	}
}
