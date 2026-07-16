package collections

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
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

// TestMatchSortedDeferredEqualsFull checks that MatchSorted with a limit (which
// defers ClassAd materialization and ranks survivors wire-native) returns exactly
// the same ranked top-N as ranking the full result and truncating. The corpus mixes
// literal Requirements slots (the wire-native survivor path) with non-literal ones
// (the full-decode fallback), and the job has a Rank, so both rank paths and the
// deferred/fallback record mix are exercised.
func TestMatchSortedDeferredEqualsFull(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4})
	for i := 0; i < 200; i++ {
		req := "true" // literal -> wire-native survivor path
		if i%3 == 0 {
			req = "TARGET.RequestMemory <= Memory" // non-literal -> fallback path
		}
		text := fmt.Sprintf(`[ Id=%d; Memory=%d; Arch="X86_64"; Cpus=%d; Requirements = %s ]`,
			i, ((i%16)+1)*1024, (i%32)+1, req)
		if err := c.Put([]byte(fmt.Sprintf("s%d", i)), mustAd(t, text)); err != nil {
			t.Fatal(err)
		}
	}
	job := mustAd(t, `[ RequestMemory = 4096;
		Requirements = TARGET.Memory >= RequestMemory && TARGET.Arch == "X86_64";
		Rank = TARGET.Cpus ]`)

	full := matchIDs(t, c.MatchSorted(job, 0)) // limit 0: materialize all, no deferral
	for _, k := range []int{1, 3, 10, 50} {
		limited := matchIDs(t, c.MatchSorted(job, k)) // limit k: deferred + wire-native rank
		want := full
		if k < len(want) {
			want = want[:k]
		}
		// Rank ties (equal Cpus) sort in an unspecified order, so compare the multiset
		// of ranks at each position rather than exact ids: the k-th best rank must match.
		if len(limited) != len(want) {
			t.Fatalf("limit=%d: got %d matches, want %d", k, len(limited), len(want))
		}
		if !sameRanks(t, c, job, limited, want) {
			t.Fatalf("limit=%d: deferred top-%d ranks differ from full top-%d\n got %v\nwant %v",
				k, k, k, limited, want)
		}
	}
}

// sameRanks reports whether two id lists have the same Cpus (== Rank) at each
// position -- a tie-tolerant equality for the ranked match results.
func sameRanks(t *testing.T, c *Collection, job *classad.ClassAd, a, b []int) bool {
	t.Helper()
	cpus := map[int]int64{}
	for ad := range c.Scan() {
		id, _ := ad.EvaluateAttrInt("Id")
		cp, _ := ad.EvaluateAttrInt("Cpus")
		cpus[int(id)] = cp
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if cpus[a[i]] != cpus[b[i]] {
			return false
		}
	}
	return true
}

// TestMatchClosureDecodeWideAds drives the closure-decode path (wide ads, non-literal
// slot Requirements, deferred MatchSorted): each slot carries >64 attributes but the
// match reads a small closure. The deferred top-N (which decodes only the closure per
// candidate) must equal the full ranked result, and the returned ads must be fully
// materialized (all attributes present).
func TestMatchClosureDecodeWideAds(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4})
	for i := 0; i < 300; i++ {
		var b strings.Builder
		fmt.Fprintf(&b, `Id=%d; Cpus=%d; Memory=%d; Arch="X86_64"; State="Unclaimed"; `,
			i, (i%16)+1, ((i%16)+1)*1024)
		// Non-literal slot Requirements referencing a couple of its own attributes and
		// the job (TARGET), plus a Start-like indirection.
		fmt.Fprintf(&b, `Start = (Cpus > 0) && (TARGET.RequestMemory <= Memory); `)
		fmt.Fprintf(&b, `Requirements = Start && (State == "Unclaimed"); `)
		// Pad to a wide ad so AttrCount clears the closure-decode threshold.
		for k := 0; k < 80; k++ {
			fmt.Fprintf(&b, `Pad%d = %d; `, k, i*100+k)
		}
		if err := c.Put([]byte(fmt.Sprintf("s%d", i)), mustAd(t, "["+b.String()+"]")); err != nil {
			t.Fatal(err)
		}
	}
	job := mustAd(t, `[ RequestMemory = 4096;
		Requirements = TARGET.Arch == "X86_64" && TARGET.Cpus >= 2;
		Rank = TARGET.Cpus + TARGET.Memory / 1024 ]`)

	full := matchIDs(t, c.MatchSorted(job, 0)) // limit 0: full decode, no deferral
	if len(full) == 0 {
		t.Fatal("expected matches")
	}
	for _, k := range []int{1, 5, 20} {
		limited := c.MatchSorted(job, k) // deferred + closure decode
		want := full
		if k < len(want) {
			want = want[:k]
		}
		if len(limited) != len(want) {
			t.Fatalf("limit=%d: got %d, want %d", k, len(limited), len(want))
		}
		if !sameRanks(t, c, job, matchIDs(t, limited), want) {
			t.Fatalf("limit=%d: deferred closure-decode ranks differ from full", k)
		}
		// Returned ads are fully materialized, not the partial closure ad.
		for _, ad := range limited {
			if _, ok := ad.EvaluateAttrInt("Pad0"); !ok {
				t.Fatalf("limit=%d: returned ad missing padding attr (not fully materialized)", k)
			}
		}
	}
}

// TestMatchHotClosure exercises the hot-header match fast path: with MatchClosureRoots
// configured, wide slot ads are flagged closure-complete and matching reads the closure
// from the hot header. Verifies the flag is set and MatchSorted results equal a plain
// (unconfigured) collection's full-decode results.
func TestMatchHotClosure(t *testing.T) {
	t.Parallel()
	build := func(roots []string) *Collection {
		c := New(Options{Shards: 4, MatchClosureRoots: roots})
		for i := 0; i < 300; i++ {
			var b strings.Builder
			fmt.Fprintf(&b, `Id=%d; Cpus=%d; Memory=%d; Arch="X86_64"; State="Unclaimed"; `,
				i, (i%16)+1, ((i%16)+1)*1024)
			fmt.Fprintf(&b, `Start = (Cpus > 0) && (TARGET.RequestMemory <= Memory); `)
			fmt.Fprintf(&b, `Requirements = Start && (State == "Unclaimed"); `)
			for k := 0; k < 80; k++ {
				fmt.Fprintf(&b, `Pad%d = %d; `, k, i*100+k)
			}
			if err := c.Put([]byte(fmt.Sprintf("s%d", i)), mustAd(t, "["+b.String()+"]")); err != nil {
				t.Fatal(err)
			}
		}
		return c
	}
	hot := build([]string{"Requirements"})
	plain := build(nil)

	// The encoder must flag a wide ad closure-complete when match roots are configured,
	// and not otherwise.
	sample := mustAd(t, `[ Cpus=8; Memory=8192; State="Unclaimed";
		Start = (Cpus > 0) && (TARGET.RequestMemory <= Memory);
		Requirements = Start && (State == "Unclaimed"); Pad=1 ]`)
	if !wire.Ad(hot.encodeAd(sample.AST())).HotClosureComplete() {
		t.Fatal("expected encoded ad flagged HotClosureComplete with MatchClosureRoots")
	}
	if wire.Ad(plain.encodeAd(sample.AST())).HotClosureComplete() {
		t.Fatal("plain collection must not flag HotClosureComplete")
	}

	job := mustAd(t, `[ RequestMemory = 4096;
		Requirements = TARGET.Arch == "X86_64" && TARGET.Cpus >= 2;
		Rank = TARGET.Cpus + TARGET.Memory / 1024 ]`)
	for _, k := range []int{1, 5, 20, 0} {
		gotHot := matchIDs(t, hot.MatchSorted(job, k))
		gotPlain := matchIDs(t, plain.MatchSorted(job, k))
		if !sameRanks(t, plain, job, gotHot, gotPlain) {
			t.Fatalf("limit=%d: hot-closure result differs from plain full-decode\n hot=%v\nplain=%v", k, gotHot, gotPlain)
		}
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

// largeMatchCorpus fills c with n slots; even-Id slots match the job below
// (Memory 8192 >= 4096, Arch X86_64, symmetric Requirements), odd-Id slots fail.
func largeMatchCorpus(t *testing.T, c *Collection, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		mem, arch := 8192, "X86_64"
		if i%2 == 1 {
			mem = 1024 // fails the job's Memory requirement
		}
		text := fmt.Sprintf(`[ Id=%d; Memory=%d; Arch=%q; Cpus=%d; Requirements = TARGET.RequestMemory <= Memory ]`,
			i, mem, arch, i) // Cpus = i ⇒ unique Rank ⇒ deterministic sort order
		if err := c.Put([]byte(fmt.Sprintf("s%d", i)), mustAd(t, text)); err != nil {
			t.Fatal(err)
		}
	}
}

func bigMatchJob(t *testing.T) *classad.ClassAd {
	return mustAd(t, `[ RequestMemory=4096;
		Requirements = TARGET.Memory >= RequestMemory && TARGET.Arch == "X86_64";
		Rank = TARGET.Cpus ]`)
}

// TestMatchParallelEqualsSerial forces the fan-out path on a large pool and checks it
// yields exactly the same matches (and Rank order) as the serial path. Run with -race.
func TestMatchParallelEqualsSerial(t *testing.T) {
	t.Parallel()
	const n = 3000
	serial := New(Options{Shards: 4, QueryParallelism: 1})
	par := New(Options{Shards: 4, SegmentSize: 1 << 12, QueryParallelism: 8})
	par.parallelMinBytes = 0 // force fan-out
	largeMatchCorpus(t, serial, n)
	largeMatchCorpus(t, par, n)

	segs := 0
	for _, sh := range par.shards {
		segs += len(sh.segs)
	}
	if segs < 8 {
		t.Fatalf("expected many segments to fan out over, got %d", segs)
	}

	setOf := func(c *Collection) []int {
		var ids []int
		seen := map[int]bool{}
		for ad := range c.Match(bigMatchJob(t)) {
			id, _ := ad.EvaluateAttrInt("Id")
			if seen[int(id)] {
				t.Fatalf("duplicate match id %d", id)
			}
			seen[int(id)] = true
			ids = append(ids, int(id))
		}
		sort.Ints(ids)
		return ids
	}
	sIDs, pIDs := setOf(serial), setOf(par)
	if !equalInts(sIDs, pIDs) {
		t.Fatalf("parallel matches (%d) != serial matches (%d)", len(pIDs), len(sIDs))
	}
	if len(sIDs) != n/2 {
		t.Fatalf("expected %d matches (even Ids), got %d", n/2, len(sIDs))
	}

	// MatchSorted order must agree too (top by Rank = Cpus).
	sSorted := matchIDs(t, serial.MatchSorted(bigMatchJob(t), 10))
	pSorted := matchIDs(t, par.MatchSorted(bigMatchJob(t), 10))
	if !equalInts(sSorted, pSorted) {
		t.Fatalf("top-10 order differs: serial %v vs parallel %v", sSorted, pSorted)
	}
}

func benchmarkMatch(b *testing.B, par int) {
	c := New(Options{Shards: 8, SegmentSize: 1 << 14, QueryParallelism: par})
	if par >= 2 {
		c.parallelMinBytes = 0
	}
	for i := 0; i < 40000; i++ {
		mem := 8192
		if i%2 == 1 {
			mem = 1024
		}
		_ = c.Put([]byte(fmt.Sprintf("s%d", i)),
			mustAd(b, fmt.Sprintf(`[ Id=%d; Memory=%d; Arch="X86_64"; Cpus=%d; Requirements = TARGET.RequestMemory <= Memory ]`, i, mem, i)))
	}
	job := mustAd(b, `[ RequestMemory=4096; Requirements = TARGET.Memory >= RequestMemory && TARGET.Arch == "X86_64"; Rank = TARGET.Cpus ]`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		for range c.Match(job) {
			n++
		}
	}
}

func BenchmarkMatchPar1(b *testing.B) { benchmarkMatch(b, 1) }
func BenchmarkMatchPar4(b *testing.B) { benchmarkMatch(b, 4) }
func BenchmarkMatchPar8(b *testing.B) { benchmarkMatch(b, 8) }

// TestMatchNonNativeJob exercises the fallback when the job's Requirements is not
// wire-native evaluable (a function call), so every slot is fully decoded. The result
// must still be correct.
func TestMatchNonNativeJob(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 2})
	_ = c.Put([]byte("a"), mustAd(t, `[ Id=1; Memory=8192; Requirements = TARGET.RequestMemory <= Memory ]`))
	_ = c.Put([]byte("b"), mustAd(t, `[ Id=2; Memory=1024; Requirements = true ]`))
	job := mustAd(t, `[ RequestMemory=4096;
		Requirements = ifThenElse(TARGET.Memory >= RequestMemory, true, false);
		Rank = 0 ]`)
	var got []int
	for ad := range c.Match(job) {
		id, _ := ad.EvaluateAttrInt("Id")
		got = append(got, int(id))
	}
	sort.Ints(got)
	if want := []int{1}; !equalInts(got, want) {
		t.Fatalf("non-native job match = %v, want %v", got, want)
	}
}

// matchIDSet collects the sorted, deduped Id set of a Match.
func matchIDSet(t *testing.T, c *Collection, job *classad.ClassAd) []int {
	t.Helper()
	seen := map[int]bool{}
	var ids []int
	for ad := range c.Match(job) {
		id, _ := ad.EvaluateAttrInt("Id")
		if seen[int(id)] {
			t.Fatalf("duplicate match id %d", id)
		}
		seen[int(id)] = true
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	return ids
}

// TestMatchIndexedEqualsFullScan: with the slot attributes indexed, Match uses the
// index candidate pre-filter (A2); it must return exactly the same matches as the
// unindexed full scan. Run with -race (indexedMatches fans out across shards).
func TestMatchIndexedEqualsFullScan(t *testing.T) {
	t.Parallel()
	const n = 3000
	plain := New(Options{Shards: 4})
	indexed := New(Options{Shards: 4, ValueAttrs: []string{"Memory"}, CategoricalAttrs: []string{"Arch"}})
	largeMatchCorpus(t, plain, n)
	largeMatchCorpus(t, indexed, n)
	indexed.Reindex()

	// The job must actually drive the indexed path.
	if got := indexed.matchIndexPlan(bigMatchJob(t), jobValues(bigMatchJob(t))); len(got) == 0 {
		t.Fatal("expected TARGET.Memory/TARGET.Arch to yield usable index probes")
	}

	pIDs := matchIDSet(t, plain, bigMatchJob(t))
	iIDs := matchIDSet(t, indexed, bigMatchJob(t))
	if !equalInts(pIDs, iIDs) {
		t.Fatalf("indexed matches (%d) != full-scan matches (%d)", len(iIDs), len(pIDs))
	}
	if len(pIDs) != n/2 {
		t.Fatalf("expected %d matches (even Ids), got %d", n/2, len(pIDs))
	}
	// Rank order must agree too.
	if s, i := matchIDs(t, plain.MatchSorted(bigMatchJob(t), 10)), matchIDs(t, indexed.MatchSorted(bigMatchJob(t), 10)); !equalInts(s, i) {
		t.Fatalf("indexed top-10 %v != full-scan %v", i, s)
	}
}

// TestMatchIndexedPartialProbe: when only some of Requirements is index-probeable
// (an opaque conjunct the rewrite must poison), the pre-filter widens rather than
// wrongly rejecting -- the full match still yields exactly the full-scan result.
func TestMatchIndexedPartialProbe(t *testing.T) {
	t.Parallel()
	const n = 2000
	plain := New(Options{Shards: 4})
	indexed := New(Options{Shards: 4, ValueAttrs: []string{"Memory"}, CategoricalAttrs: []string{"Arch"}})
	largeMatchCorpus(t, plain, n)
	largeMatchCorpus(t, indexed, n)
	indexed.Reindex()

	// Arch is probeable; the Memory test is hidden inside ifThenElse (a function),
	// so the rewrite poisons it and only Arch drives candidate selection.
	job := mustAd(t, `[ RequestMemory=4096;
		Requirements = TARGET.Arch == "X86_64" && ifThenElse(TARGET.Memory >= RequestMemory, true, false);
		Rank = 0 ]`)
	if got := indexed.matchIndexPlan(job, jobValues(job)); len(got) != 1 {
		t.Fatalf("expected exactly the Arch probe, got %d probes", len(got))
	}
	if p, i := matchIDSet(t, plain, job), matchIDSet(t, indexed, job); !equalInts(p, i) {
		t.Fatalf("partial-probe indexed %d != full-scan %d", len(i), len(p))
	} else if len(p) != n/2 {
		t.Fatalf("expected %d matches, got %d", n/2, len(p))
	}
}

// TestMatchIndexedTargetDependentConst: a job attribute the pre-filter would bake is
// TARGET-dependent, so it is undefined with no target and its conjunct is dropped
// from the probes rather than baked as a wrong constant (which could wrongly exclude
// real matches). Match must still return every true match.
func TestMatchIndexedTargetDependentConst(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 2, ValueAttrs: []string{"Memory"}, CategoricalAttrs: []string{"Arch"}})
	_ = c.Put([]byte("s1"), mustAd(t, `[ Id=1; Memory=8192; Arch="X86_64"; Requirements = true ]`))
	_ = c.Put([]byte("s2"), mustAd(t, `[ Id=2; Memory=1024; Arch="X86_64"; Requirements = true ]`))
	c.Reindex()
	// RequestMemory depends on TARGET, so it is undefined at plan time: the Memory
	// conjunct is dropped (not baked), only Arch probes. Per slot the full match then
	// evaluates Memory >= Memory/2, true for both -- so both must match. A pre-filter
	// that baked a wrong constant would wrongly drop one.
	job := mustAd(t, `[ RequestMemory = TARGET.Memory / 2;
		Requirements = TARGET.Memory >= RequestMemory && TARGET.Arch == "X86_64";
		Rank = 0 ]`)
	if got, want := matchIDSet(t, c, job), []int{1, 2}; !equalInts(got, want) {
		t.Fatalf("target-dependent-const match = %v, want %v", got, want)
	}
}

func benchmarkMatchSelective(b *testing.B, indexed bool) {
	opts := Options{Shards: 8, SegmentSize: 1 << 14, QueryParallelism: 8}
	if indexed {
		opts.ValueAttrs = []string{"Cpus"}
	}
	c := New(opts)
	c.parallelMinBytes = 0
	for i := 0; i < 40000; i++ {
		_ = c.Put([]byte(fmt.Sprintf("s%d", i)),
			mustAd(b, fmt.Sprintf(`[ Id=%d; Cpus=%d; Arch="X86_64"; Requirements = true ]`, i, i)))
	}
	if indexed {
		c.Reindex()
	}
	// Only the top ~2.5% (Cpus >= 39000) qualify.
	job := mustAd(b, `[ MinCpus=39000; Requirements = TARGET.Cpus >= MinCpus; Rank = TARGET.Cpus ]`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		for range c.Match(job) {
			n++
		}
	}
}

func BenchmarkMatchSelectiveFullScan(b *testing.B) { benchmarkMatchSelective(b, false) }
func BenchmarkMatchSelectiveIndexed(b *testing.B)  { benchmarkMatchSelective(b, true) }

// TestMatchRecordsResourceDemand verifies that matchmaking records the slot-side
// probes (the attributes a job's Requirements constrain on the slot) as resource-side
// index demand -- even with no index configured -- so SuggestIndexes can recommend
// indexing exactly those attributes to speed the match.
func TestMatchRecordsResourceDemand(t *testing.T) {
	machines := New(Options{Shards: 2}) // no indexes configured
	for i := 0; i < 40; i++ {
		machines.Put([]byte(fmt.Sprintf("m%d", i)),
			mustAd(t, fmt.Sprintf(`[ Name="m%d"; Arch="X86_64"; Memory=%d; Requirements=true ]`, i, (i%8+1)*1024)))
	}
	job := mustAd(t, `[ RequestMemory=2048; Requirements = (TARGET.Arch == "X86_64") && (TARGET.Memory >= RequestMemory); Rank = TARGET.Memory ]`)
	_ = machines.MatchSortedRanked(job, 4)

	byAttr := map[string]IndexSuggestion{}
	for _, s := range machines.SuggestIndexes(1000) {
		byAttr[s.Attr] = s
	}
	if a, ok := byAttr["Arch"]; !ok || a.Kind != "categorical" || a.QueriesEq == 0 {
		t.Errorf("want a categorical Arch suggestion from the match, got %+v", byAttr["Arch"])
	}
	if m, ok := byAttr["Memory"]; !ok || m.Kind != "value" || m.QueriesRange == 0 {
		t.Errorf("want a value Memory suggestion from the match, got %+v", byAttr["Memory"])
	}
}

// TestExplainMatch verifies the match-plan explanation: the job's Requirements are
// rewritten over the slot (job constants baked in) and each resulting probe reports
// whether a resource index covers it.
func TestExplainMatch(t *testing.T) {
	machines := New(Options{Shards: 4, ValueAttrs: []string{"Memory"}}) // Memory indexed, Arch not
	for i := 0; i < 100; i++ {
		machines.Put([]byte(fmt.Sprintf("m%d", i)),
			mustAd(t, fmt.Sprintf(`[ Name="m%d"; Arch="X86_64"; Memory=%d; Requirements=true ]`, i, (i%8+1)*1024)))
	}
	job := mustAd(t, `[ RequestMemory=7168; Requirements = (TARGET.Arch == "X86_64") && (TARGET.Memory >= RequestMemory) ]`)
	ex := machines.ExplainMatch(job)

	if !ex.HasRequirements {
		t.Fatal("expected HasRequirements")
	}
	// The rewrite bakes RequestMemory to 2048 and drops the TARGET scope.
	if !strings.Contains(ex.SlotPredicate, "Memory >= 7168") || !strings.Contains(ex.SlotPredicate, `Arch == "X86_64"`) {
		t.Errorf("slot predicate = %q, want it to contain the baked Memory/Arch constraints", ex.SlotPredicate)
	}
	if ex.Plan != "indexed" {
		t.Errorf("plan = %q, want indexed (Memory is indexed)", ex.Plan)
	}
	byAttr := map[string]ProbeExplain{}
	for _, p := range ex.Probes {
		byAttr[p.Attr] = p
	}
	if p, ok := byAttr["Memory"]; !ok || !p.Indexed || p.Kind != "value" {
		t.Errorf("Memory probe = %+v, want indexed value", p)
	}
	if p, ok := byAttr["Arch"]; !ok || p.Indexed {
		t.Errorf("Arch probe = %+v, want present but not indexed", p)
	}
}

// TestExplainMatchNoRequirements: a job without Requirements matches every slot.
func TestExplainMatchNoRequirements(t *testing.T) {
	machines := New(Options{Shards: 2})
	machines.Put([]byte("m1"), mustAd(t, `[ Name="m1"; Requirements=true ]`))
	ex := machines.ExplainMatch(mustAd(t, `[ Foo=1 ]`))
	if ex.HasRequirements || ex.Plan == "indexed" {
		t.Errorf("no-Requirements job: got HasRequirements=%v plan=%q, want false / a scan", ex.HasRequirements, ex.Plan)
	}
}

// TestMatchUndefinedGuardExceptionEqualsFullScan validates the DNF match pushdown for
// a WithinResourceLimits-style disk term guarded by an undefined check. Slots without
// `catalogs` need Disk >= RequestDisk; slots WITH `catalogs` have a looser bound
// (RequestDisk - catalogSize). The rewrite bakes RequestDisk, assumes catalogs
// undefined (folding the guard to the fast-path `Disk >= RequestDisk`), and adds the
// `catalogs isnt undefined` exception -- so the looser-bound slots are still visited.
// The indexed result must equal the full scan; without the exception it would miss
// the catalog slots with Disk in [RequestDisk-catalogSize, RequestDisk).
func TestMatchUndefinedGuardExceptionEqualsFullScan(t *testing.T) {
	t.Parallel()
	const n = 4000
	build := func(indexed bool) *Collection {
		opts := Options{Shards: 4}
		if indexed {
			opts.ValueAttrs = []string{"Disk"}
			opts.CategoricalAttrs = []string{"catalogs"}
		}
		c := New(opts)
		for i := 0; i < n; i++ {
			disk := 400 + (i*2654435761)%1200 // 400..1599
			var text string
			if i%50 == 0 { // ~2% advertise catalogs (looser disk bound applies)
				text = fmt.Sprintf(`[ Id=%d; Disk=%d; catalogs="c%d"; Requirements=true ]`, i, disk, i)
			} else {
				text = fmt.Sprintf(`[ Id=%d; Disk=%d; Requirements=true ]`, i, disk)
			}
			c.Put([]byte(fmt.Sprintf("m%d", i)), mustAd(t, text))
		}
		if indexed {
			c.Reindex()
		}
		return c
	}
	// RequestDisk=1000, catalog reservation=500 (so catalog slots match at Disk>=500).
	job := mustAd(t, `[ RequestDisk=1000;
		Requirements = TARGET.Disk >= (RequestDisk - ifThenElse(catalogs is undefined, 0, 500));
		Rank = 0 ]`)

	plain, indexed := build(false), build(true)
	// The plan must be prunable and disjunctive (fast path + catalogs exception).
	groups, prunable := indexed.slotMatchPlan(job, jobValues(job))
	if !prunable || len(groups) < 2 {
		t.Fatalf("expected a prunable disjunctive plan (fast path + exception), got prunable=%v groups=%d", prunable, len(groups))
	}
	p, i := matchIDSet(t, plain, job), matchIDSet(t, indexed, job)
	if !equalInts(p, i) {
		t.Fatalf("indexed match (%d) != full scan (%d) -- exception disjunct unsound/missing", len(i), len(p))
	}
	if len(p) == 0 {
		t.Fatal("expected some matches")
	}
}

// TestMatchFiniteDomainMaterialization validates that an opaque pure function of a
// low-cardinality categorical attribute (versionGE over CondorVersion) is materialized
// into a membership probe, so `versionGE(...) || (Disk >= X)` becomes an indexable DNF
// union -- and that the indexed match equals the full scan.
func TestMatchFiniteDomainMaterialization(t *testing.T) {
	t.Parallel()
	versions := []string{
		`$CondorVersion: 25.12.0 2026-06-25 $`, // passing (>= 25.12.0)
		`$CondorVersion: 25.13.0 2026-07-01 $`, // passing
		`$CondorVersion: 24.0.0 2025-01-01 $`,  // failing
		`$CondorVersion: 23.5.0 2024-06-01 $`,  // failing
	}
	const n = 4000
	build := func(indexed bool) *Collection {
		opts := Options{Shards: 4}
		if indexed {
			opts.ValueAttrs = []string{"Disk"}
			opts.CategoricalAttrs = []string{"CondorVersion"}
		}
		c := New(opts)
		for i := 0; i < n; i++ {
			disk := 400 + (i*2654435761)%1200
			ad := mustAd(t, fmt.Sprintf(`[ Id=%d; Disk=%d; CondorVersion=%q; Arch="X86_64"; Requirements=true ]`,
				i, disk, versions[i%len(versions)]))
			c.Put([]byte(fmt.Sprintf("m%d", i)), ad)
		}
		if indexed {
			c.Reindex()
		}
		return c
	}
	// A passing-version slot matches regardless of disk; a failing one needs Disk>=1000.
	job := mustAd(t, `[ RequestDisk=1000;
		Requirements = (versionGE(split(TARGET.CondorVersion)[1], "25.12.0") || (TARGET.Disk >= RequestDisk)) && (TARGET.Arch == "X86_64");
		Rank = 0 ]`)

	plain, indexed := build(false), build(true)
	groups, prunable := indexed.slotMatchPlan(job, jobValues(job))
	if !prunable || len(groups) < 2 {
		t.Fatalf("expected a prunable disjunctive plan from materialization, got prunable=%v groups=%d", prunable, len(groups))
	}
	p, i := matchIDSet(t, plain, job), matchIDSet(t, indexed, job)
	if !equalInts(p, i) {
		t.Fatalf("indexed match (%d) != full scan (%d) -- materialization unsound", len(i), len(p))
	}
	if len(p) == 0 || len(p) == n {
		t.Fatalf("expected a partial match set, got %d of %d", len(p), n)
	}
}

// TestMatchPlanCostGate: when the slot probes are unselective (match nearly every
// slot), the planner skips the pushdown (a scan is cheaper than visiting ~all
// candidates), while still returning the correct matches.
func TestMatchPlanCostGate(t *testing.T) {
	t.Parallel()
	const n = 4000
	// Every slot is X86_64 with Cpus in {1,2} -- an Arch/Cpus probe barely prunes.
	c := New(Options{Shards: 4, CategoricalAttrs: []string{"Arch"}, ValueAttrs: []string{"Cpus"}})
	plain := New(Options{Shards: 4})
	for i := 0; i < n; i++ {
		ad := mustAd(t, fmt.Sprintf(`[ Id=%d; Arch="X86_64"; Cpus=%d; Requirements=true ]`, i, 1+i%2))
		c.Put([]byte(fmt.Sprintf("m%d", i)), ad)
		plain.Put([]byte(fmt.Sprintf("m%d", i)), mustAd(t, fmt.Sprintf(`[ Id=%d; Arch="X86_64"; Cpus=%d; Requirements=true ]`, i, 1+i%2)))
	}
	c.Reindex()
	job := mustAd(t, `[ RequestCpus=1; Requirements = (TARGET.Arch == "X86_64") && (TARGET.Cpus >= RequestCpus); Rank = 0 ]`)

	// Arch==X86_64 is 100% and Cpus>=1 is 100% -> the plan barely prunes -> gated off.
	if _, prunable := c.slotMatchPlan(job, jobValues(job)); prunable {
		t.Errorf("expected the unselective plan to be gated off (scan), but it was prunable")
	}
	// Result is still correct (all slots match).
	if p, i := matchIDSet(t, plain, job), matchIDSet(t, c, job); !equalInts(p, i) || len(i) != n {
		t.Fatalf("gated match wrong: indexed %d vs full %d (want %d)", len(i), len(p), n)
	}
}
