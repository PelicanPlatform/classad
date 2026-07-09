package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// schedQueue builds a collection with a maintained ordered index modelling the
// schedd's per-owner idle-job priority queue: members are idle jobs (JobStatus==1),
// partitioned by Owner, ordered by JobPrio descending then QDate ascending.
func schedQueue(t *testing.T) *Collection {
	t.Helper()
	return New(Options{
		Shards: 4,
		Ordered: []OrderSpec{{
			Partition: "Owner",
			Where:     "JobStatus == 1",
			Keys:      []SortKey{{Expr: "JobPrio", Desc: true}, {Expr: "QDate"}},
		}},
	})
}

func putJob(t *testing.T, c *Collection, key, owner string, status, prio, qdate int) {
	t.Helper()
	ad := mustAd(t, fmt.Sprintf(`[ Owner=%q; JobStatus=%d; JobPrio=%d; QDate=%d; Job=%q ]`,
		owner, status, prio, qdate, key))
	if err := c.Put([]byte(key), ad); err != nil {
		t.Fatal(err)
	}
}

// orderedJobs collects the "Job" attribute of one partition in order.
func orderedJobs(t *testing.T, c *Collection, owner string) []string {
	t.Helper()
	var got []string
	for r := range c.Ordered(0, classad.NewStringValue(owner), OrderCursor{}) {
		j, ok := r.Ad.EvaluateAttrString("Job")
		if !ok {
			t.Fatal("ordered ad missing Job")
		}
		got = append(got, j)
	}
	return got
}

func TestOrderedSchedQueue(t *testing.T) {
	t.Parallel()
	c := schedQueue(t)
	putJob(t, c, "a1", "alice", 1, 10, 100) // idle
	putJob(t, c, "a2", "alice", 1, 20, 100) // idle, higher prio
	putJob(t, c, "a3", "alice", 2, 5, 100)  // running -> excluded
	putJob(t, c, "b1", "bob", 1, 15, 100)   // different owner

	if got, want := orderedJobs(t, c, "alice"), []string{"a2", "a1"}; !eqStrings(got, want) {
		t.Fatalf("alice idle = %v, want %v (JobPrio desc; running a3 excluded)", got, want)
	}
	if got, want := orderedJobs(t, c, "bob"), []string{"b1"}; !eqStrings(got, want) {
		t.Fatalf("bob idle = %v, want %v", got, want)
	}

	// a3 starts idle: it joins the queue (lowest prio -> last).
	putJob(t, c, "a3", "alice", 1, 5, 100)
	if got, want := orderedJobs(t, c, "alice"), []string{"a2", "a1", "a3"}; !eqStrings(got, want) {
		t.Fatalf("after a3 idle = %v, want %v", got, want)
	}

	// a1 starts running: it leaves the queue (membership transition on update).
	putJob(t, c, "a1", "alice", 2, 10, 100)
	if got, want := orderedJobs(t, c, "alice"), []string{"a2", "a3"}; !eqStrings(got, want) {
		t.Fatalf("after a1 running = %v, want %v", got, want)
	}

	// Deleting a2 removes it.
	c.Delete([]byte("a2"))
	if got, want := orderedJobs(t, c, "alice"), []string{"a3"}; !eqStrings(got, want) {
		t.Fatalf("after delete a2 = %v, want %v", got, want)
	}
}

// TestOrderedTiebreakByQDateThenSeq: equal JobPrio falls to QDate, then to the order
// the job entered the queue (insertion sequence).
func TestOrderedTiebreakByQDateThenSeq(t *testing.T) {
	t.Parallel()
	c := schedQueue(t)
	putJob(t, c, "x", "u", 1, 10, 300)  // prio 10, qdate 300
	putJob(t, c, "y", "u", 1, 10, 200)  // prio 10, qdate 200 -> earlier
	putJob(t, c, "z1", "u", 1, 10, 200) // ties x-... actually ties y on (10,200); entered after y
	putJob(t, c, "z2", "u", 1, 10, 200) // ties on (10,200); entered last
	// Order: qdate asc within prio 10 -> {200 group before 300}; within 200, seq order y,z1,z2.
	if got, want := orderedJobs(t, c, "u"), []string{"y", "z1", "z2", "x"}; !eqStrings(got, want) {
		t.Fatalf("tiebreak = %v, want %v", got, want)
	}
}

// TestOrderedResumeCursor iterates a partition across two calls using the cursor.
func TestOrderedResumeCursor(t *testing.T) {
	t.Parallel()
	c := schedQueue(t)
	putJob(t, c, "p1", "u", 1, 30, 0)
	putJob(t, c, "p2", "u", 1, 20, 0)
	putJob(t, c, "p3", "u", 1, 10, 0)

	// First call: take one job, remember the cursor after it.
	var first []string
	var cursor OrderCursor
	for r := range c.Ordered(0, classad.NewStringValue("u"), OrderCursor{}) {
		j, _ := r.Ad.EvaluateAttrString("Job")
		first = append(first, j)
		cursor = r.Cursor
		break // stop after the first (negotiator ran out of slots)
	}
	if want := []string{"p1"}; !eqStrings(first, want) {
		t.Fatalf("first batch = %v, want %v", first, want)
	}
	// Resume: the rest, in order, exactly once.
	var rest []string
	for r := range c.Ordered(0, classad.NewStringValue("u"), cursor) {
		j, _ := r.Ad.EvaluateAttrString("Job")
		rest = append(rest, j)
	}
	if want := []string{"p2", "p3"}; !eqStrings(rest, want) {
		t.Fatalf("resumed batch = %v, want %v", rest, want)
	}
}

// TestOrderedMalformedSpecPanics: a bad expression in a spec is a config error.
func TestOrderedMalformedSpecPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("New with a malformed Ordered spec should panic")
		}
	}()
	New(Options{Ordered: []OrderSpec{{Keys: []SortKey{{Expr: "this ("}}}}})
}

// TestOrderedRecoveryRebuild: a persistent collection's ordered index is derived
// state, rebuilt on reopen. Write jobs, Close, reopen, and confirm Ordered() is
// correct without re-Putting anything.
func TestOrderedRecoveryRebuild(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("mmap unsupported")
	}
	dir := t.TempDir()
	opts := Options{
		Dir:    dir,
		Shards: 4,
		Ordered: []OrderSpec{{
			Partition: "Owner",
			Where:     "JobStatus == 1",
			Keys:      []SortKey{{Expr: "JobPrio", Desc: true}},
		}},
	}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	putJob(t, c, "j1", "alice", 1, 10, 0) // idle
	putJob(t, c, "j2", "alice", 1, 20, 0) // idle, higher prio
	putJob(t, c, "j3", "alice", 2, 99, 0) // running -> excluded
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if got, want := orderedJobs(t, c2, "alice"), []string{"j2", "j1"}; !eqStrings(got, want) {
		t.Fatalf("after reopen: alice = %v, want %v (index rebuilt on recovery)", got, want)
	}
}

// TestOrderedChainedInheritedPartition: on a chained collection the ordered index
// evaluates over the inherited view -- Owner lives on the cluster (parent) ad, yet
// partitions the child procs; structural cluster ads are never members.
func TestOrderedChainedInheritedPartition(t *testing.T) {
	t.Parallel()
	c := New(Options{
		Shards:       8,
		ParentKeyFor: jobParentKey,
		IsStructural: jobStructural,
		Ordered: []OrderSpec{{
			Partition: "Owner",
			Where:     "JobStatus == 1",
			Keys:      []SortKey{{Expr: "JobPrio", Desc: true}},
		}},
	})
	// Cluster (structural parent) carries Owner; procs carry the dynamic attrs.
	if err := c.Put([]byte("1.-1"), mustAd(t, `[ Owner="alice" ]`)); err != nil {
		t.Fatal(err)
	}
	putProc := func(key string, status, prio int) {
		ad := mustAd(t, fmt.Sprintf(`[ JobStatus=%d; JobPrio=%d; Job=%q ]`, status, prio, key))
		if err := c.Put([]byte(key), ad); err != nil {
			t.Fatal(err)
		}
	}
	putProc("1.0", 1, 10) // idle
	putProc("1.1", 1, 20) // idle, higher prio
	putProc("1.2", 2, 99) // running -> excluded

	// Owner is inherited from the cluster, so all idle procs land in alice's queue.
	if got, want := orderedJobs(t, c, "alice"), []string{"1.1", "1.0"}; !eqStrings(got, want) {
		t.Fatalf("chained alice = %v, want %v (Owner inherited from cluster)", got, want)
	}
	// The structural cluster ad itself is never a member (it has no JobStatus, and is
	// hidden regardless).
	putProc("1.0", 2, 10) // 1.0 starts running -> leaves
	if got, want := orderedJobs(t, c, "alice"), []string{"1.1"}; !eqStrings(got, want) {
		t.Fatalf("after 1.0 running = %v, want %v", got, want)
	}
}

// clusterQueue orders idle jobs by QDate and computes a cluster signature over the
// request attributes -- the schedd's RRL grouping key.
func clusterQueue(t *testing.T) *Collection {
	t.Helper()
	return New(Options{
		Shards: 4,
		Ordered: []OrderSpec{{
			Partition: "Owner",
			Where:     "JobStatus == 1",
			Keys:      []SortKey{{Expr: "QDate"}},
			Cluster:   []string{"RequestCpus", "RequestMemory"},
		}},
	})
}

func putCJob(t *testing.T, c *Collection, key, owner string, qdate, cpus, mem int) {
	t.Helper()
	ad := mustAd(t, fmt.Sprintf(`[ Owner=%q; JobStatus=1; QDate=%d; RequestCpus=%d; RequestMemory=%d; Job=%q ]`,
		owner, qdate, cpus, mem, key))
	if err := c.Put([]byte(key), ad); err != nil {
		t.Fatal(err)
	}
}

// rleRuns folds a partition's ordered stream into runs of equal cluster signature --
// exactly the schedd's RRL construction -- and returns the run count and the per-ad
// signatures in order.
func rleRuns(t *testing.T, c *Collection, owner string) (runs int, sigs []uint64) {
	t.Helper()
	var prev uint64
	have := false
	for r := range c.Ordered(0, classad.NewStringValue(owner), OrderCursor{}) {
		sigs = append(sigs, r.Signature)
		if !have || r.Signature != prev {
			runs++
			prev = r.Signature
			have = true
		}
	}
	return runs, sigs
}

// TestOrderedClusterSignature: adjacent equal-cluster jobs fold into one RRL; the same
// jobs interleaved fold into many. This is the A,A,B,B -> 2 vs A,B,A,B -> 4 example.
func TestOrderedClusterSignature(t *testing.T) {
	t.Parallel()
	grouped := clusterQueue(t)
	putCJob(t, grouped, "a1", "u", 1, 1, 100) // cluster A
	putCJob(t, grouped, "a2", "u", 2, 1, 100) // cluster A
	putCJob(t, grouped, "b1", "u", 3, 2, 200) // cluster B
	putCJob(t, grouped, "b2", "u", 4, 2, 200) // cluster B
	runs, sigs := rleRuns(t, grouped, "u")
	if len(sigs) != 4 {
		t.Fatalf("got %d members, want 4", len(sigs))
	}
	if sigs[0] != sigs[1] || sigs[2] != sigs[3] {
		t.Fatal("equal clustering attributes must hash to equal signatures")
	}
	if sigs[0] == sigs[2] {
		t.Fatal("different clustering attributes must hash to different signatures")
	}
	if runs != 2 {
		t.Fatalf("grouped runs = %d, want 2", runs)
	}

	interleaved := clusterQueue(t)
	putCJob(t, interleaved, "a1", "u", 1, 1, 100) // A
	putCJob(t, interleaved, "b1", "u", 2, 2, 200) // B
	putCJob(t, interleaved, "a2", "u", 3, 1, 100) // A
	putCJob(t, interleaved, "b2", "u", 4, 2, 200) // B
	if runs, _ := rleRuns(t, interleaved, "u"); runs != 4 {
		t.Fatalf("interleaved runs = %d, want 4", runs)
	}
}

// TestOrderedSignatureInPlaceUpdate: changing only a clustering attribute (sort
// position unchanged) refreshes the surfaced signature without relocating the entry.
func TestOrderedSignatureInPlaceUpdate(t *testing.T) {
	t.Parallel()
	c := clusterQueue(t)
	putCJob(t, c, "j", "u", 5, 1, 100)
	_, sigs := rleRuns(t, c, "u")
	putCJob(t, c, "j", "u", 5, 1, 200) // same QDate (position), different RequestMemory
	n, sigs2 := rleRuns(t, c, "u")
	if n != 1 || len(sigs2) != 1 {
		t.Fatalf("expected 1 member after in-place update, got %d", len(sigs2))
	}
	if sigs[0] == sigs2[0] {
		t.Fatal("signature should change when a clustering attribute changes")
	}
}

// TestOrderedNoClusterSignatureZero: without OrderSpec.Cluster, the signature is 0.
func TestOrderedNoClusterSignatureZero(t *testing.T) {
	t.Parallel()
	c := schedQueue(t) // no Cluster configured
	putJob(t, c, "j1", "u", 1, 10, 0)
	for r := range c.Ordered(0, classad.NewStringValue("u"), OrderCursor{}) {
		if r.Signature != 0 {
			t.Fatalf("unconfigured cluster signature = %d, want 0", r.Signature)
		}
	}
}

// TestOrderedStreamingWalk models the negotiator consuming RRLs one at a time over a
// long period (network I/O between steps): the walk holds a snapshot while writers
// churn the index. It must not block; the snapshot's order/membership stays frozen at
// the call, while ad content is fetched live; a fresh walk then reflects every change.
func TestOrderedStreamingWalk(t *testing.T) {
	t.Parallel()
	c := schedQueue(t)
	putJob(t, c, "p1", "u", 1, 50, 0)
	putJob(t, c, "p2", "u", 1, 40, 0)
	putJob(t, c, "p3", "u", 1, 30, 0)

	var walked []string
	mutated := false
	for r := range c.Ordered(0, classad.NewStringValue("u"), OrderCursor{}) {
		j, _ := r.Ad.EvaluateAttrString("Job")
		walked = append(walked, j)
		if !mutated {
			mutated = true
			// Concurrent-style churn mid-walk. None of this blocks the in-flight walk,
			// nor alters the snapshot's order/membership.
			putJob(t, c, "p0", "u", 1, 99, 0) // new highest-priority idle job
			putJob(t, c, "p2", "u", 2, 40, 0) // p2 starts running (leaves the set)
			c.Delete([]byte("p3"))            // p3 removed entirely
		}
		if j == "p2" {
			// Ad content is live: p2 was set running after the snapshot was taken.
			if st, _ := r.Ad.EvaluateAttrInt("JobStatus"); st != 2 {
				t.Fatalf("expected live ad content (p2 JobStatus=2), got %d", st)
			}
		}
	}
	// Frozen order/membership: p1,p2,p3 as of T0. p0 (added after) is absent; p2 is
	// still walked though now running; p3 is walked-then-skipped (deleted -> Get miss).
	if want := []string{"p1", "p2"}; !eqStrings(walked, want) {
		t.Fatalf("streaming walk = %v, want %v", walked, want)
	}
	// A fresh walk reflects every committed change: p0 and p1 are idle, p2 runs, p3 gone.
	if want := []string{"p0", "p1"}; !eqStrings(orderedJobs(t, c, "u"), want) {
		t.Fatalf("post-walk fresh scan = %v, want %v", orderedJobs(t, c, "u"), want)
	}
}
