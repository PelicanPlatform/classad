package collections

import (
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// queryClusterIds runs the FULL-DECODE query and reads ClusterId off each ad, in the order the
// read yields them. Order is the whole point here, so this never sorts.
func queryClusterIds(t *testing.T, c *Collection, constraint string, limit int) []int64 {
	t.Helper()
	q, err := vm.Parse(constraint)
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for ad := range c.Query(q) {
		v, verr := ad.EvaluateAttr("ClusterId").IntValue()
		if verr != nil {
			t.Fatal(verr)
		}
		got = append(got, v)
		if limit > 0 && len(got) == limit {
			break
		}
	}
	return got
}

// The point of this path: a newest-first collection fans out AND stays newest-first.
//
// The counter is checked because declining to fan out is silent -- the read falls back and yields
// the same ads in the same order -- so without it this would pass having compared the serial path
// with itself.
func TestOrderedQueryFansOutNewestFirst(t *testing.T) {
	const n = 20000
	c := orderedFixture(t, n, 8)
	before := orderedQueryFanouts.Load()
	got := queryClusterIds(t, c, "JobStatus >= 0", 0)
	if orderedQueryFanouts.Load() == before {
		t.Fatal("the read did not fan out; this would have compared the serial path with itself")
	}
	if len(got) != n {
		t.Fatalf("%d ads, want %d", len(got), n)
	}
	for i, v := range got {
		if want := int64(n - 1 - i); v != want {
			t.Fatalf("ad %d: ClusterId %d, want %d (newest-first)", i, v, want)
		}
	}
}

// The ordered fan-out must agree with the serial walk as a SEQUENCE, not as a set: a fan-out that
// returned the right ads in the wrong order would pass a set comparison and break every consumer
// that reads an archive for its newest rows.
func TestOrderedQueryMatchesSerial(t *testing.T) {
	const n = 20000
	par := orderedFixture(t, n, 8)
	ser := orderedFixture(t, n, 1)
	for _, constraint := range []string{"JobStatus >= 0", "JobStatus == 3", "ProcId == 7 && JobStatus < 3", "false"} {
		before := orderedQueryFanouts.Load()
		gp := queryClusterIds(t, par, constraint, 0)
		fanned := orderedQueryFanouts.Load() != before
		gs := queryClusterIds(t, ser, constraint, 0)
		if !fanned && constraint != "false" {
			t.Fatalf("%q: did not fan out", constraint)
		}
		if len(gp) != len(gs) {
			t.Fatalf("%q: %d parallel ads, %d serial", constraint, len(gp), len(gs))
		}
		for i := range gp {
			if gp[i] != gs[i] {
				t.Fatalf("%q: ad %d: parallel %d, serial %d", constraint, i, gp[i], gs[i])
			}
		}
	}
}

// A consumer that stops early still gets the K NEWEST ads. That is what an archive's LIMIT rests
// on, and the thing a fan-out is most likely to break, since the workers behind the newest segment
// are already decoding while the consumer is still inside it.
func TestOrderedQueryEarlyStopIsNewest(t *testing.T) {
	const n = 20000
	c := orderedFixture(t, n, 8)
	for _, k := range []int{1, 5, 64, 200, 5000} {
		got := queryClusterIds(t, c, "JobStatus >= 0", k)
		if len(got) != k {
			t.Fatalf("limit %d: got %d ads", k, len(got))
		}
		for i, v := range got {
			if want := int64(n - 1 - i); v != want {
				t.Fatalf("limit %d, ad %d: ClusterId %d, want %d", k, i, v, want)
			}
		}
	}
}

// Stopping inside the prelude must not leave a worker running: the prelude walks the newest
// segment on the caller's goroutine precisely so a short read starts none. If the fan-out engaged
// for a read that stopped in the first segment, the counter would show it.
func TestOrderedQueryShortReadStartsNoWorkers(t *testing.T) {
	c := orderedFixture(t, 20000, 8)
	before := orderedQueryFanouts.Load()
	got := queryClusterIds(t, c, "JobStatus >= 0", 5)
	if len(got) != 5 {
		t.Fatalf("got %d ads, want 5", len(got))
	}
	if n := orderedQueryFanouts.Load() - before; n != 0 {
		t.Fatalf("a 5-ad read started the fan-out %d time(s); the prelude exists to avoid that", n)
	}
}
