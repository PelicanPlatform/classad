package collections

import (
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// memberJobsFixture is a jobs-shaped table: several procs per cluster, a handful of statuses, owners
// in mixed case. Every few hundred records it plants a record whose ClusterId or Owner is something
// other than the column's type -- a real, a boolean, a string, an integer past 2^53, an expression,
// UNDEFINED, ERROR, or nothing at all -- since those are where an index, a zone or a vector kernel
// can disagree with evaluation.
//
// clean=true plants none of them, for a test about pruning, which an escaped value in a block
// (rightly) disables.
//
// It returns two stores over the same records, one with ClusterId value-indexed and Owner
// categorically indexed and one with no index, both with the columnar accelerator built, plus each
// record's source ad (keyed by its ID) for the reference evaluation.
func memberJobsFixture(tb testing.TB, n int, clean bool) (indexed, plain *Collection, src map[int]*classad.ClassAd) {
	tb.Helper()
	cd, err := NewZSTDCodec(nil)
	if err != nil {
		tb.Fatal(err)
	}
	indexed = New(Options{Shards: 1, SegmentSize: 1 << 16, Codec: cd,
		ValueAttrs: []string{"ClusterId"}, CategoricalAttrs: []string{"Owner"}})
	plain = New(Options{Shards: 1, SegmentSize: 1 << 16, Codec: cd})
	src = map[int]*classad.ClassAd{}
	odd := []string{
		"1003.0", "true", `"1003"`, "9007199254740993", "9007199254740992.0", "Base + 3", "undefined",
		"error", "-0.0",
	}
	for i := 0; i < n; i++ {
		cluster := strconv.Itoa(1000 + i/7)
		owner := strconv.Quote(fmt.Sprintf("user%d", (i/7)%40))
		if i%5 == 0 {
			owner = strings.ToUpper(owner)
		}
		switch {
		case clean:
		case i%301 == 17:
			cluster = odd[(i/301)%len(odd)]
		case i%307 == 29:
			owner = []string{"42", "undefined", "Base"}[(i/307)%3]
		}
		text := fmt.Sprintf("ID = %d\nClusterId = %s\nProcId = %d\nJobStatus = %d\nOwner = %s\nBase = 1000",
			i, cluster, i%7, 1+i%5, owner)
		if !clean && i%409 == 51 {
			text = fmt.Sprintf("ID = %d\nProcId = 0\nJobStatus = 1", i) // no ClusterId, no Owner
		}
		ad := mustAdOld(tb, text)
		src[i] = ad
		key := []byte(fmt.Sprintf("k%d", i))
		if err := indexed.Put(key, ad); err != nil {
			tb.Fatal(err)
		}
		if err := plain.Put(key, ad); err != nil {
			tb.Fatal(err)
		}
	}
	for _, c := range []*Collection{indexed, plain} {
		c.Reindex()
		if !c.BuildAndEnableSchemaScan(4000, 8) {
			tb.Skip("no sealed segments")
		}
	}
	return indexed, plain, src
}

// refMatchIDs is the reference: the tree-walking evaluator over each record's source ad.
func refMatchIDs(q *vm.Query, src map[int]*classad.ClassAd) []int {
	var out []int
	for id, ad := range src {
		v := classad.NewEvaluator(ad).Evaluate(q.Expr())
		if b, err := v.BoolValue(); err == nil && b {
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out
}

func memberQueryIDs(tb testing.TB, c *Collection, q *vm.Query) []int {
	tb.Helper()
	var out []int
	for ad := range c.Query(q) {
		id, ok := ad.EvaluateAttrInt("ID")
		if !ok {
			tb.Fatalf("a result without an ID")
		}
		out = append(out, int(id))
	}
	sort.Ints(out)
	return out
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// memberQueries draws member constraints over the fixture: cluster lists of 1, 10 and 300 entries
// (some ids absent), the production shape with its status filter, identicalMember, string lists in
// mixed case, mixed-type lists, and member under negation and disjunction.
func memberQueries(rng *rand.Rand, maxCluster int) []string {
	clusters := func(k int) string {
		parts := make([]string, k)
		for i := range parts {
			parts[i] = strconv.Itoa(1000 + rng.Intn(maxCluster-1000+50)) // some beyond the last cluster
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	out := []string{
		`member(ClusterId, {1003})`, `member(ClusterId, {1003.0})`, `member(ClusterId, {true, 1})`,
		`member(ClusterId, {"1003"})`, `member(ClusterId, {1003, "1003"})`, `identicalMember(ClusterId, {1003})`,
		`identicalMember(ClusterId, {1003.0, "1003", true})`, `member(ClusterId, {9007199254740992})`,
		`member(ClusterId, {9007199254740993})`, `identicalMember(ClusterId, {9007199254740993})`,
		`member(ClusterId, {undefined, 1004})`, `identicalMember(ClusterId, {undefined})`,
		`identicalMember(ClusterId, {error})`, `member(ClusterId, {0})`, `member(ClusterId, {})`,
		`member(Owner, {"user3", "USER4"})`, `identicalMember(Owner, {"user3", "USER4"})`,
		`member(Owner, {"user3", 42})`, `member(Owner, {42})`, `identicalMember(Owner, {undefined})`,
		`!member(ClusterId, {1003, 1004})`, `member(ClusterId, {1003}) || JobStatus == 4`,
		`member(ClusterId, {1003, 1004}) && member(Owner, {"user0", "user1", "USER0"})`,
		`member(ClusterId, {1001, 1002, 1003}) && ClusterId > 1001`,
	}
	for _, k := range []int{1, 10, 300} {
		for r := 0; r < 3; r++ {
			l := clusters(k)
			out = append(out,
				"member(ClusterId, "+l+")",
				"member(ClusterId, "+l+") && (JobStatus == 1 || JobStatus == 2)",
				"identicalMember(ClusterId, "+l+") && JobStatus != 3",
			)
		}
	}
	return out
}

// TestMemberPathsAgree is the differential test for member across every path that can answer it:
// the indexed row path, the unindexed row path (with zone pruning and the columnar prefilter), the
// vectorized count, the per-record columnar count, and CountQuery's routing -- each against the
// tree-walking evaluator over the source ads. It also checks the paths it means to exercise were
// taken: the index plans the cluster lists, and the vector executor serves blocks rather than
// declining them.
func TestMemberPathsAgree(t *testing.T) {
	const n = 8000
	indexed, plain, src := memberJobsFixture(t, n, false)
	defer indexed.Close()
	defer plain.Close()
	rng := rand.New(rand.NewSource(11))
	vectorized := 0
	for _, qs := range memberQueries(rng, 1000+n/7) {
		q, err := vm.Parse(qs)
		if err != nil {
			t.Fatalf("parse %q: %v", qs, err)
		}
		want := refMatchIDs(q, src)
		if got := memberQueryIDs(t, indexed, q); !sameInts(got, want) {
			t.Errorf("%s: indexed query %d records, reference %d", qs, len(got), len(want))
		}
		if got := memberQueryIDs(t, plain, q); !sameInts(got, want) {
			t.Errorf("%s: unindexed query %d records, reference %d", qs, len(got), len(want))
		}
		for _, c := range []*Collection{indexed, plain} {
			if got, ok := c.VectorEvalCount(q); !ok || got != len(want) {
				t.Errorf("%s: VectorEvalCount = %d (ok=%v), reference %d", qs, got, ok, len(want))
			}
			if split := lastVecSplit.Load(); split.vecBlocks > 0 {
				vectorized++
			}
			if got, ok := c.ColumnarEvalCount(q); !ok || got != len(want) {
				t.Errorf("%s: ColumnarEvalCount = %d (ok=%v), reference %d", qs, got, ok, len(want))
			}
			if got, ok := c.CountQuery(q); ok && got != len(want) {
				t.Errorf("%s: CountQuery = %d, reference %d", qs, got, len(want))
			}
		}
	}
	if vectorized == 0 {
		t.Fatal("no member query was vectorized; the vector comparison is vacuous")
	}
}

// TestMemberIndexPlan checks the planner side of the production shape: a member over an indexed
// attribute plans as an index probe, ExplainQuery says so, and the probe's candidates are what the
// cluster list selects.
func TestMemberIndexPlan(t *testing.T) {
	indexed, plain, _ := memberJobsFixture(t, 4000, false)
	defer indexed.Close()
	defer plain.Close()
	q, err := vm.Parse(`member(ClusterId, {1003, 1004, 1200}) && (JobStatus == 1 || JobStatus == 2)`)
	if err != nil {
		t.Fatal(err)
	}
	ex := indexed.ExplainQuery(q)
	if ex.Plan != "indexed" || ex.IndexUsable == 0 || !ex.Native {
		t.Fatalf("plan %q, %d usable probes, native %v; want an indexed native plan (%s)",
			ex.Plan, ex.IndexUsable, ex.Native, ex.Reason)
	}
	if !strings.Contains(ex.Reason, "ClusterId") {
		t.Errorf("reason %q does not name the ClusterId probe", ex.Reason)
	}
	var sawIn bool
	for _, p := range ex.Probes {
		if p.Attr == "ClusterId" && p.Op == "in" && p.Indexed {
			sawIn = true
			// 3 clusters x 7 procs plus each segment's exception records (the odd ClusterIds, which
			// every equality must re-verify); an estimate near the table size means the probe was
			// not applied.
			if p.HasSelectivity && p.EstCandidates > 400 {
				t.Errorf("ClusterId probe estimates %d candidates for 3 clusters", p.EstCandidates)
			}
		}
	}
	if !sawIn {
		t.Errorf("no indexed ClusterId in-probe in %+v", ex.Probes)
	}
	// Without the index there is nothing to plan with, but the probe is still recognized: the
	// explanation must not claim the filter has no usable conjunct.
	if ex := plain.ExplainQuery(q); strings.Contains(ex.Reason, "no conjunct an index or zone map can use") {
		t.Errorf("unindexed explain: %q", ex.Reason)
	}
}

// TestMemberZonePrunesBlocks checks that a member over a numeric column rules out blocks by their
// zones: the fixture's ClusterId rises with the record order, so a short list of clusters lies inside
// few blocks' ranges and the vector scan must skip most of the rest without reading them.
func TestMemberZonePrunesBlocks(t *testing.T) {
	_, plain, src := memberJobsFixture(t, 8000, true)
	defer plain.Close()
	q, err := vm.Parse(`member(ClusterId, {1003, 1004, 1005})`)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := plain.VectorEvalCount(q)
	if want := len(refMatchIDs(q, src)); !ok || got != want {
		t.Fatalf("VectorEvalCount = %d (ok=%v), reference %d", got, ok, want)
	}
	split := lastVecSplit.Load()
	if split.prunedBlocks == 0 {
		t.Fatalf("no block pruned (%d vectorized, %d declined); member is not reaching the zones",
			split.vecBlocks, split.scopeBlocks)
	}
	if split.vecBlocks+split.scopeBlocks > split.prunedBlocks {
		t.Errorf("only %d blocks pruned against %d read for a three-cluster list",
			split.prunedBlocks, split.vecBlocks+split.scopeBlocks)
	}
}

// BenchmarkMemberQuery is the production shape -- `member(ClusterId, {...}) && (JobStatus == 1 ||
// JobStatus == 2)` over a ~50k-record jobs table -- with cluster lists of 1, 10 and 300 entries,
// through the row path with ClusterId indexed and without, and as a count over the columns
// (VectorEvalCount, falling back to counting the row path's results where it declines).
func BenchmarkMemberQuery(b *testing.B) {
	const n = 50000
	indexed, plain, _ := memberJobsFixture(b, n, true)
	defer indexed.Close()
	defer plain.Close()
	rng := rand.New(rand.NewSource(3))
	for _, k := range []int{1, 10, 300} {
		parts := make([]string, k)
		for i := range parts {
			parts[i] = strconv.Itoa(1000 + rng.Intn(n/7))
		}
		qs := "member(ClusterId, {" + strings.Join(parts, ", ") + "}) && (JobStatus == 1 || JobStatus == 2)"
		q, err := vm.Parse(qs)
		if err != nil {
			b.Fatal(err)
		}
		for _, arm := range []struct {
			name string
			c    *Collection
		}{{"indexed", indexed}, {"scan", plain}} {
			b.Run(fmt.Sprintf("%s/list=%d", arm.name, k), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					for range arm.c.Query(q) {
					}
				}
			})
		}
		b.Run(fmt.Sprintf("count/list=%d", k), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, ok := plain.VectorEvalCount(q); ok {
					continue
				}
				for range plain.Query(q) {
				}
			}
		})
	}
}
