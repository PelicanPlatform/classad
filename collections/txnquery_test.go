package collections

import (
	"slices"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// txnAd builds an ad with an Owner and a numeric Cpus, the shape these tests query on.
func txnAd(owner string, cpus int64) *classad.ClassAd {
	ad := classad.New()
	ad.InsertAttrString("Owner", owner)
	ad.InsertAttr("Cpus", cpus)
	return ad
}

// owners collects the Owner attribute of each yielded ad, sorted for comparison.
func owners(seq func(func(*classad.ClassAd) bool)) []string {
	var out []string
	for ad := range seq {
		s, _ := ad.EvaluateAttr("Owner").StringValue()
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

func newTxnColl(t *testing.T) *Collection {
	t.Helper()
	return New(Options{Shards: 4})
}

// commit writes ads under the given keys and commits, so later tests query committed state.
func txnCommit(t *testing.T, c *Collection, ads map[string]*classad.ClassAd) {
	t.Helper()
	tx := c.Begin()
	for k, ad := range ads {
		tx.Put([]byte(k), ad)
	}
	if res := tx.Commit(); res.Conflicted() {
		t.Fatalf("unexpected conflicts: %v", res.Conflicts)
	}
}

// With nothing buffered the transaction sees exactly the committed state -- and takes the
// fast path straight to Collection.Query.
func TestTxnQueryWithoutWritesMatchesCollectionQuery(t *testing.T) {
	c := newTxnColl(t)
	txnCommit(t, c, map[string]*classad.ClassAd{
		"a": txnAd("alice", 4),
		"b": txnAd("bob", 8),
	})

	tx := c.Begin()
	q := mustQuery(t, "Cpus >= 4")
	if got, want := owners(tx.Query(q)), []string{"alice", "bob"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// The point of the whole change: a row the transaction created is visible to its own query.
func TestTxnQuerySeesOwnInsert(t *testing.T) {
	c := newTxnColl(t)
	txnCommit(t, c, map[string]*classad.ClassAd{"a": txnAd("alice", 4)})

	tx := c.Begin()
	tx.Put([]byte("b"), txnAd("bob", 8))

	q := mustQuery(t, "Cpus >= 4")
	if got, want := owners(tx.Query(q)), []string{"alice", "bob"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v (the transaction's own insert is missing)", got, want)
	}
	// ... and is still invisible to a committed-state query until commit.
	if got, want := owners(c.Query(q)), []string{"alice"}; !slices.Equal(got, want) {
		t.Errorf("committed query got %v, want %v", got, want)
	}
}

// A row the transaction deleted is gone from its own query, even though it is still
// committed.
func TestTxnQueryHidesOwnDelete(t *testing.T) {
	c := newTxnColl(t)
	txnCommit(t, c, map[string]*classad.ClassAd{
		"a": txnAd("alice", 4),
		"b": txnAd("bob", 8),
	})

	tx := c.Begin()
	tx.Delete([]byte("b"))

	q := mustQuery(t, "Cpus >= 4")
	if got, want := owners(tx.Query(q)), []string{"alice"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v (the deleted row is still visible)", got, want)
	}
}

// A rewritten row is matched and yielded in its rewritten form -- not once per version,
// and not by its committed values.
func TestTxnQueryUsesRewrittenValues(t *testing.T) {
	c := newTxnColl(t)
	txnCommit(t, c, map[string]*classad.ClassAd{"a": txnAd("alice", 4)})

	tx := c.Begin()
	tx.Put([]byte("a"), txnAd("alice-renamed", 16))

	if got, want := owners(tx.Query(mustQuery(t, "Cpus >= 4"))), []string{"alice-renamed"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// The rewrite moved the row out of this constraint's match set.
	if got := owners(tx.Query(mustQuery(t, "Cpus == 4"))); len(got) != 0 {
		t.Errorf("got %v, want no rows (the committed version should not match)", got)
	}
	// ... and into this one, which the committed version never matched.
	if got, want := owners(tx.Query(mustQuery(t, "Cpus == 16"))), []string{"alice-renamed"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A buffered row that does not match is not yielded just because it is buffered.
func TestTxnQueryFiltersBufferedRows(t *testing.T) {
	c := newTxnColl(t)
	tx := c.Begin()
	tx.Put([]byte("a"), txnAd("alice", 2))
	tx.Put([]byte("b"), txnAd("bob", 8))

	if got, want := owners(tx.Query(mustQuery(t, "Cpus >= 4"))), []string{"bob"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Stopping early must stop the scan in both halves of the overlay, not just the first.
func TestTxnQueryStopsEarly(t *testing.T) {
	c := newTxnColl(t)
	txnCommit(t, c, map[string]*classad.ClassAd{"a": txnAd("alice", 4), "b": txnAd("bob", 4)})

	tx := c.Begin()
	tx.Put([]byte("c"), txnAd("carol", 4))
	tx.Put([]byte("d"), txnAd("dave", 4))

	n := 0
	for range tx.Query(mustQuery(t, "Cpus >= 4")) {
		n++
		break
	}
	if n != 1 {
		t.Errorf("yielded %d rows after break, want 1", n)
	}

	// Break inside the buffered half (after the two committed rows are consumed).
	n = 0
	for range tx.Query(mustQuery(t, "Cpus >= 4")) {
		n++
		if n == 3 {
			break
		}
	}
	if n != 3 {
		t.Errorf("yielded %d rows, want 3", n)
	}
}

// Committing makes the transaction's view the committed view -- the overlay was not a
// separate universe.
func TestTxnQueryMatchesCommittedStateAfterCommit(t *testing.T) {
	c := newTxnColl(t)
	txnCommit(t, c, map[string]*classad.ClassAd{"a": txnAd("alice", 4)})

	tx := c.Begin()
	tx.Put([]byte("b"), txnAd("bob", 8))
	tx.Delete([]byte("a"))
	want := owners(tx.Query(mustQuery(t, "Cpus >= 1")))
	if res := tx.Commit(); res.Conflicted() {
		t.Fatalf("unexpected conflicts: %v", res.Conflicts)
	}

	if got := owners(c.Query(mustQuery(t, "Cpus >= 1"))); !slices.Equal(got, want) {
		t.Errorf("after commit the committed state is %v, but the transaction saw %v", got, want)
	}
}

func TestTxnKeysWhereOverlay(t *testing.T) {
	c := newTxnColl(t)
	txnCommit(t, c, map[string]*classad.ClassAd{
		"a": txnAd("alice", 4),
		"b": txnAd("bob", 8),
	})

	tx := c.Begin()
	tx.Put([]byte("c"), txnAd("carol", 16)) // new
	tx.Delete([]byte("b"))                  // removed
	tx.Put([]byte("a"), txnAd("alice", 1))  // moved out of the match set

	var keys []string
	for k := range tx.KeysWhere(mustQuery(t, "Cpus >= 4")) {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"c"}; !slices.Equal(keys, want) {
		t.Errorf("got %v, want %v", keys, want)
	}
}

// System keys stay hidden from a transactional scan, matching ForEachAd and Query.
func TestTxnQuerySkipsSystemKeys(t *testing.T) {
	c := newTxnColl(t)
	tx := c.Begin()
	tx.Put([]byte(SystemKey("internal")), txnAd("system", 8))
	tx.Put([]byte("visible"), txnAd("alice", 8))

	if got, want := owners(tx.Query(mustQuery(t, "Cpus >= 4"))), []string{"alice"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v (a system key leaked into the result)", got, want)
	}
}

// Buffered rows are yielded in key order, so a result set is reproducible run to run.
func TestTxnQueryBufferedRowsAreOrdered(t *testing.T) {
	c := newTxnColl(t)
	tx := c.Begin()
	for _, k := range []string{"z", "m", "a", "q", "b"} {
		tx.Put([]byte(k), txnAd(k, 8))
	}

	var first []string
	for range 5 { // repeat: map iteration order varies per range, sorting must mask it
		var got []string
		for k := range tx.KeysWhere(mustQuery(t, "Cpus >= 4")) {
			got = append(got, k)
		}
		if first == nil {
			first = got
			continue
		}
		if !slices.Equal(got, first) {
			t.Fatalf("iteration order varies: %v then %v", first, got)
		}
	}
	if want := []string{"a", "b", "m", "q", "z"}; !slices.Equal(first, want) {
		t.Errorf("got %v, want %v", first, want)
	}
}

// A row another transaction commits after this one captured its snapshot is invisible to
// this transaction's scan -- the property that makes a scan and a point lookup agree.
// Before ForEachAdAt the scan read the live store and would have seen it.
func TestTxnQueryIsSnapshotIsolated(t *testing.T) {
	c := newTxnColl(t)
	txnCommit(t, c, map[string]*classad.ClassAd{"a": txnAd("alice", 8)})

	tx := c.Begin()
	// Capture the snapshot: a buffered write touches (and pins) its shard, and the first
	// scan pins the rest.
	tx.Put([]byte("staged"), txnAd("staged", 8))
	if got, want := owners(tx.Query(mustQuery(t, "Cpus >= 4"))), []string{"alice", "staged"}; !slices.Equal(got, want) {
		t.Fatalf("before the concurrent commit: got %v, want %v", got, want)
	}

	// Another writer lands a row after this transaction's snapshot.
	txnCommit(t, c, map[string]*classad.ClassAd{"b": txnAd("bob", 8)})

	if got, want := owners(tx.Query(mustQuery(t, "Cpus >= 4"))), []string{"alice", "staged"}; !slices.Equal(got, want) {
		t.Errorf("after the concurrent commit: got %v, want %v (bob leaked into the snapshot)", got, want)
	}
	// A fresh transaction does see it, so the row really did commit.
	fresh := c.Begin()
	if got, want := owners(fresh.Query(mustQuery(t, "Cpus >= 4"))), []string{"alice", "bob"}; !slices.Equal(got, want) {
		t.Errorf("fresh transaction got %v, want %v", got, want)
	}
}

// The scan and a point lookup in one transaction must agree about a row a concurrent
// writer changed: both read at the transaction's snapshot.
func TestTxnQueryAgreesWithGet(t *testing.T) {
	c := newTxnColl(t)
	txnCommit(t, c, map[string]*classad.ClassAd{"a": txnAd("alice", 8)})

	tx := c.Begin()
	tx.Put([]byte("staged"), txnAd("staged", 8)) // force the overlay path
	// Read "a" first, so its shard's snapshot is captured BEFORE the concurrent write.
	// Without that the transaction would capture the snapshot after the fact and the two
	// reads would agree trivially, proving nothing.
	if _, ok := tx.Get([]byte("a")); !ok {
		t.Fatal("seed row missing")
	}

	// A concurrent writer moves "a" out of the match set.
	txnCommit(t, c, map[string]*classad.ClassAd{"a": txnAd("alice", 1)})

	scanned := owners(tx.Query(mustQuery(t, "Cpus >= 4")))
	ad, ok := tx.Get([]byte("a"))
	if !ok {
		t.Fatal("Get lost the row entirely")
	}
	cpus, _ := ad.EvaluateAttr("Cpus").IntValue()
	inScan := slices.Contains(scanned, "alice")
	if inScan != (cpus >= 4) {
		t.Errorf("scan says alice matches=%v but Get reports Cpus=%d: the two disagree", inScan, cpus)
	}
}

// A transaction that never writes still reads at its snapshot, so repeated scans are
// stable even while other writers commit.
func TestTxnQueryRepeatableWithoutWrites(t *testing.T) {
	c := newTxnColl(t)
	txnCommit(t, c, map[string]*classad.ClassAd{"a": txnAd("alice", 8)})

	tx := c.Begin()
	first := owners(tx.Query(mustQuery(t, "Cpus >= 4")))

	txnCommit(t, c, map[string]*classad.ClassAd{"b": txnAd("bob", 8)})

	if second := owners(tx.Query(mustQuery(t, "Cpus >= 4"))); !slices.Equal(first, second) {
		t.Errorf("repeated scan changed under a concurrent commit: %v then %v", first, second)
	}
}

// KeysWhere reads at the snapshot too -- an UPDATE or DELETE inside a transaction must
// not pick up rows that appeared after it started.
func TestTxnKeysWhereIsSnapshotIsolated(t *testing.T) {
	c := newTxnColl(t)
	txnCommit(t, c, map[string]*classad.ClassAd{"a": txnAd("alice", 8)})

	tx := c.Begin()
	var before []string
	for k := range tx.KeysWhere(mustQuery(t, "Cpus >= 4")) {
		before = append(before, k)
	}
	txnCommit(t, c, map[string]*classad.ClassAd{"b": txnAd("bob", 8)})

	var after []string
	for k := range tx.KeysWhere(mustQuery(t, "Cpus >= 4")) {
		after = append(after, k)
	}
	slices.Sort(before)
	slices.Sort(after)
	if !slices.Equal(before, after) {
		t.Errorf("key set changed under a concurrent commit: %v then %v", before, after)
	}
}
