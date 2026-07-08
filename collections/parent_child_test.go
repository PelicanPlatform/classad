package collections

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// jobParentKey maps a job key "cluster.proc" to its cluster ad "cluster.-1"
// (returning nil for a cluster ad, which has no parent).
func jobParentKey(key []byte) []byte {
	s := string(key)
	dot := strings.LastIndexByte(s, '.')
	if dot < 0 {
		return nil
	}
	if s[dot+1:] == "-1" {
		return nil
	}
	return []byte(s[:dot] + ".-1")
}

// jobStructural marks cluster ads (proc == -1) structural.
func jobStructural(key []byte) bool { return strings.HasSuffix(string(key), ".-1") }

func chainedJobsCollection(t *testing.T) *Collection {
	t.Helper()
	c := New(Options{Shards: 8, ParentKeyFor: jobParentKey, IsStructural: jobStructural})
	// Two DAGs. DAGManJobId and Owner live only on the cluster (parent) ads.
	c.Put([]byte("1.-1"), mustAd(t, `[DAGManJobId=42; Owner="alice"; Cmd="/bin/sleep"]`))
	c.Put([]byte("1.0"), mustAd(t, `[ProcId=0; JobStatus=1]`))
	c.Put([]byte("1.1"), mustAd(t, `[ProcId=1; JobStatus=2]`))
	c.Put([]byte("2.-1"), mustAd(t, `[DAGManJobId=7; Owner="bob"]`))
	c.Put([]byte("2.0"), mustAd(t, `[ProcId=0; JobStatus=1]`))
	return c
}

// TestChainedColocation verifies a cluster ad and its procs land in one shard.
func TestChainedColocation(t *testing.T) {
	c := chainedJobsCollection(t)
	sc := c.shardOf([]byte("1.-1"), c.h.Hash([]byte("1.-1")))
	for _, k := range []string{"1.0", "1.1"} {
		if s := c.shardOf([]byte(k), c.h.Hash([]byte(k))); s != sc {
			t.Errorf("job %s in shard %d, cluster in shard %d (not co-located)", k, s, sc)
		}
	}
}

// TestChainedQueryFilter verifies a query on a parent-only attribute resolves
// through the chain: DAGManJobId (only on the cluster ad) selects that cluster's
// procs, and cluster ads themselves are hidden.
func TestChainedQueryFilter(t *testing.T) {
	c := chainedJobsCollection(t)

	q, err := vm.Parse(`DAGManJobId == 42`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for ad := range c.Query(q) {
		cl, _ := ad.EvaluateAttrInt("ClusterId") // absent; use key-free identity via ProcId+DAGManJobId
		p, _ := ad.EvaluateAttrInt("ProcId")
		d, _ := ad.EvaluateAttrInt("DAGManJobId")
		if d != 42 {
			t.Errorf("matched ad has DAGManJobId=%d, want 42 (chain broken)", d)
		}
		_ = cl
		got = append(got, fmt.Sprintf("proc%d", p))
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "proc0" || got[1] != "proc1" {
		t.Errorf("DAGManJobId==42 matched %v, want the two procs of cluster 1 (no cluster ad)", got)
	}
}

// TestChainedChildOverride verifies a child attribute overrides the parent's.
func TestChainedChildOverride(t *testing.T) {
	c := chainedJobsCollection(t)
	// Give proc 1.0 its own DAGManJobId, overriding the cluster's 42.
	c.Put([]byte("1.0"), mustAd(t, `[ProcId=0; DAGManJobId=99]`))

	count := func(expr string) int {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for range c.Query(q) {
			n++
		}
		return n
	}
	if n := count(`DAGManJobId == 99`); n != 1 {
		t.Errorf("DAGManJobId==99 matched %d, want 1 (the overriding proc)", n)
	}
	if n := count(`DAGManJobId == 42`); n != 1 {
		t.Errorf("DAGManJobId==42 matched %d, want 1 (proc 1.1 still inherits; 1.0 overrode)", n)
	}
}

// TestChainedStructuralHidden verifies structural (cluster) ads are excluded from
// Scan, and TestChainedGet verifies Get resolves inherited attributes.
func TestChainedStructuralHidden(t *testing.T) {
	c := chainedJobsCollection(t)
	total := 0
	for range c.Scan() {
		total++
	}
	if total != 3 {
		t.Errorf("Scan returned %d ads, want 3 jobs (cluster ads hidden)", total)
	}
}

func TestChainedGet(t *testing.T) {
	c := chainedJobsCollection(t)
	ad, ok := c.Get([]byte("1.0"))
	if !ok {
		t.Fatal("Get 1.0 missing")
	}
	if d, _ := ad.EvaluateAttrInt("DAGManJobId"); d != 42 {
		t.Errorf("Get(1.0) DAGManJobId=%d, want 42 (inherited from cluster)", d)
	}
	if o, _ := ad.EvaluateAttrString("Owner"); o != "alice" {
		t.Errorf("Get(1.0) Owner=%q, want alice (inherited)", o)
	}
}
