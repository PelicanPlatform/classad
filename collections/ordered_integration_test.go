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
	for ad := range c.Ordered(0, classad.NewStringValue(owner), OrderCursor{}) {
		j, ok := ad.EvaluateAttrString("Job")
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
	for ad, cur := range c.Ordered(0, classad.NewStringValue("u"), OrderCursor{}) {
		j, _ := ad.EvaluateAttrString("Job")
		first = append(first, j)
		cursor = cur
		break // stop after the first (negotiator ran out of slots)
	}
	if want := []string{"p1"}; !eqStrings(first, want) {
		t.Fatalf("first batch = %v, want %v", first, want)
	}
	// Resume: the rest, in order, exactly once.
	var rest []string
	for ad := range c.Ordered(0, classad.NewStringValue("u"), cursor) {
		j, _ := ad.EvaluateAttrString("Job")
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
