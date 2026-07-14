package collections

import (
	"fmt"
	"sync"
	"testing"
)

func txnGetInt(t *testing.T, tx *Txn, key, attr string) int64 {
	t.Helper()
	ad, ok := tx.Get([]byte(key))
	if !ok {
		t.Fatalf("txn Get(%q) missing", key)
	}
	v, _ := ad.EvaluateAttrInt(attr)
	return v
}

func TestTxnBasicAndReadYourWrites(t *testing.T) {
	c := New(Options{Shards: 4})
	tx := c.Begin()
	tx.Put([]byte("k1"), mustAd(t, `[ N = 1 ]`))
	// Read-your-writes: visible inside the txn before commit.
	if v := txnGetInt(t, tx, "k1", "N"); v != 1 {
		t.Fatalf("read-your-writes N = %d, want 1", v)
	}
	// Not visible to the store until commit.
	if _, ok := c.Get([]byte("k1")); ok {
		t.Fatal("uncommitted write visible in the store")
	}
	res := tx.Commit()
	if res.Conflicted() || res.Committed != 1 {
		t.Fatalf("commit = %+v, want 1 committed, no conflicts", res)
	}
	if ad, ok := c.Get([]byte("k1")); !ok {
		t.Fatal("committed write not visible")
	} else if v, _ := ad.EvaluateAttrInt("N"); v != 1 {
		t.Fatalf("committed N = %d, want 1", v)
	}
}

func TestTxnSnapshotIsolationRead(t *testing.T) {
	c := New(Options{Shards: 4})
	_ = c.Put([]byte("k"), mustAd(t, `[ N = 1 ]`))
	tx := c.Begin()
	if v := txnGetInt(t, tx, "k", "N"); v != 1 { // snapshot pins N=1
		t.Fatalf("snapshot N = %d, want 1", v)
	}
	// A concurrent committer bumps N; the open txn must still see its snapshot.
	_ = c.Put([]byte("k"), mustAd(t, `[ N = 2 ]`))
	if v := txnGetInt(t, tx, "k", "N"); v != 1 {
		t.Fatalf("after concurrent write, snapshot N = %d, want 1 (SI)", v)
	}
}

func TestTxnWriteWriteConflict(t *testing.T) {
	c := New(Options{Shards: 4})
	_ = c.Put([]byte("k"), mustAd(t, `[ N = 0 ]`))
	// Two transactions both read k at the same snapshot and write it.
	a := c.Begin()
	b := c.Begin()
	_ = txnGetInt(t, a, "k", "N")
	_ = txnGetInt(t, b, "k", "N")
	a.Put([]byte("k"), mustAd(t, `[ N = 10 ]`))
	b.Put([]byte("k"), mustAd(t, `[ N = 20 ]`))
	// First committer wins.
	if r := a.Commit(); r.Conflicted() {
		t.Fatalf("first commit conflicted: %+v", r)
	}
	if r := b.Commit(); !r.Conflicted() || len(r.Conflicts) != 1 {
		t.Fatalf("second commit = %+v, want 1 conflict", r)
	}
	if ad, _ := c.Get([]byte("k")); func() int64 { v, _ := ad.EvaluateAttrInt("N"); return v }() != 10 {
		t.Fatal("loser's write should not have applied")
	}
}

func TestTxnPerAdPartialCommit(t *testing.T) {
	c := New(Options{Shards: 4})
	for _, k := range []string{"a", "b", "c"} {
		_ = c.Put([]byte(k), mustAd(t, `[ N = 0 ]`))
	}
	tx := c.Begin()
	_ = txnGetInt(t, tx, "a", "N") // snapshot the shards
	_ = txnGetInt(t, tx, "b", "N")
	_ = txnGetInt(t, tx, "c", "N")
	tx.Put([]byte("a"), mustAd(t, `[ N = 1 ]`))
	tx.Put([]byte("b"), mustAd(t, `[ N = 1 ]`))
	tx.Put([]byte("c"), mustAd(t, `[ N = 1 ]`))
	// Another committer modifies b out from under the txn.
	_ = c.Put([]byte("b"), mustAd(t, `[ N = 99 ]`))
	res := tx.Commit()
	if res.Committed != 2 || len(res.Conflicts) != 1 || string(res.Conflicts[0]) != "b" {
		t.Fatalf("partial commit = %+v, want a,c committed and b conflicted", res)
	}
	// a and c applied; b kept the concurrent writer's value.
	get := func(k string) int64 { ad, _ := c.Get([]byte(k)); v, _ := ad.EvaluateAttrInt("N"); return v }
	if get("a") != 1 || get("c") != 1 || get("b") != 99 {
		t.Fatalf("values a=%d b=%d c=%d, want 1/99/1", get("a"), get("b"), get("c"))
	}
}

func TestTxnDeleteConflict(t *testing.T) {
	c := New(Options{Shards: 2})
	_ = c.Put([]byte("k"), mustAd(t, `[ N = 1 ]`))
	tx := c.Begin()
	_ = txnGetInt(t, tx, "k", "N")
	tx.Delete([]byte("k"))
	// Concurrent update to k after the snapshot -> the delete must conflict.
	_ = c.Put([]byte("k"), mustAd(t, `[ N = 2 ]`))
	if r := tx.Commit(); !r.Conflicted() {
		t.Fatalf("delete of a concurrently-modified key should conflict: %+v", r)
	}
	if _, ok := c.Get([]byte("k")); !ok {
		t.Fatal("key should still exist (delete conflicted)")
	}
}

// TestTxnSingleWriterFastPath: when nothing commits between a transaction's
// snapshot and its commit, no per-key conflict check is performed.
func TestTxnSingleWriterFastPath(t *testing.T) {
	c := New(Options{Shards: 4})
	for i := 0; i < 50; i++ {
		_ = c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, `[ N = 0 ]`))
	}
	before := ConflictChecks()
	tx := c.Begin()
	for i := 0; i < 50; i++ { // read-modify-write across all shards, single writer
		k := fmt.Sprintf("k%d", i)
		v := txnGetInt(t, tx, k, "N")
		tx.Put([]byte(k), mustAd(t, fmt.Sprintf(`[ N = %d ]`, v+1)))
	}
	res := tx.Commit()
	if res.Conflicted() || res.Committed != 50 {
		t.Fatalf("single-writer commit = %+v, want 50 committed", res)
	}
	if got := ConflictChecks() - before; got != 0 {
		t.Fatalf("single-writer commit did %d conflict checks, want 0 (fast path)", got)
	}
	// A conflicting scenario DOES perform checks (guards the counter itself).
	a, b := c.Begin(), c.Begin()
	_ = txnGetInt(t, a, "k0", "N")
	_ = txnGetInt(t, b, "k0", "N")
	a.Put([]byte("k0"), mustAd(t, `[ N = 100 ]`))
	b.Put([]byte("k0"), mustAd(t, `[ N = 200 ]`))
	a.Commit()
	mid := ConflictChecks()
	b.Commit() // b's shard advanced since its snapshot -> slow path
	if ConflictChecks()-mid == 0 {
		t.Fatal("contended commit should have performed a conflict check")
	}
}

// TestTxnConcurrentIncrementConverges is the OCC correctness check: many goroutines
// increment the same counter via read-modify-write transactions, retrying on
// conflict. The final value must equal the total number of increments -- no lost
// updates.
func TestTxnConcurrentIncrementConverges(t *testing.T) {
	c := New(Options{Shards: 8})
	_ = c.Put([]byte("counter"), mustAd(t, `[ N = 0 ]`))
	const workers, perWorker = 8, 200
	var wg sync.WaitGroup
	var retries int64
	var rmu sync.Mutex
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := 0
			for i := 0; i < perWorker; i++ {
				for { // retry until this increment commits
					tx := c.Begin()
					v := txnGetInt(t, tx, "counter", "N")
					tx.Put([]byte("counter"), mustAd(t, fmt.Sprintf(`[ N = %d ]`, v+1)))
					if !tx.Commit().Conflicted() {
						break
					}
					local++
				}
			}
			rmu.Lock()
			retries += int64(local)
			rmu.Unlock()
		}()
	}
	wg.Wait()
	ad, _ := c.Get([]byte("counter"))
	got, _ := ad.EvaluateAttrInt("N")
	if want := int64(workers * perWorker); got != want {
		t.Fatalf("counter = %d, want %d (lost updates)", got, want)
	}
	t.Logf("converged to %d with %d conflict-retries", got, retries)
}

// TestTxnIndependentKeysNoConflict: concurrent writers to DISTINCT keys never
// conflict (the common HTCondor pattern of per-ad independent edits).
func TestTxnIndependentKeysNoConflict(t *testing.T) {
	c := New(Options{Shards: 8})
	const n = 500
	for i := 0; i < n; i++ {
		_ = c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, `[ N = 0 ]`))
	}
	var wg sync.WaitGroup
	var conflicts int64
	var mu sync.Mutex
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := base; i < n; i += 8 {
				tx := c.Begin()
				k := []byte(fmt.Sprintf("k%d", i))
				v := txnGetInt(t, tx, fmt.Sprintf("k%d", i), "N")
				tx.Put(k, mustAd(t, fmt.Sprintf(`[ N = %d ]`, v+1)))
				if tx.Commit().Conflicted() {
					mu.Lock()
					conflicts++
					mu.Unlock()
				}
			}
		}(w)
	}
	wg.Wait()
	if conflicts != 0 {
		t.Fatalf("independent-key writers hit %d conflicts, want 0", conflicts)
	}
}
