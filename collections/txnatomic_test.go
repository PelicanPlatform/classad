package collections

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// All-or-nothing commits (Txn.SetAtomic). The default commit path applies each key
// independently; these tests pin the opt-in behaviour AND pin that the default did not move.

// atomicKeysInDistinctShards returns n keys that each land in a DIFFERENT shard of c, so a test
// can be sure it is exercising the cross-shard path and not accidentally one shard.
func atomicKeysInDistinctShards(t *testing.T, c *Collection, n int) [][]byte {
	t.Helper()
	seen := map[int]bool{}
	var out [][]byte
	for i := 0; len(out) < n && i < 10000; i++ {
		k := []byte(fmt.Sprintf("k%d", i))
		idx := c.shardOf(k, c.h.Hash(k))
		if !seen[idx] {
			seen[idx] = true
			out = append(out, k)
		}
	}
	if len(out) < n {
		t.Fatalf("found keys in only %d distinct shards, want %d", len(out), n)
	}
	return out
}

// atomicShardOf is the shard index a key commits to.
func atomicShardOf(c *Collection, key []byte) int { return c.shardOf(key, c.h.Hash(key)) }

// mustN fails unless key reads back with N == want.
func mustN(t *testing.T, c *Collection, key []byte, want int64) {
	t.Helper()
	ad, ok := c.Get(key)
	if !ok {
		t.Fatalf("%s: missing", key)
	}
	if v, _ := ad.EvaluateAttrInt("N"); v != want {
		t.Fatalf("%s: N = %d, want %d", key, v, want)
	}
}

// hasKey reports whether a Conflicts/Unapplied slice contains key.
func hasKey(ks [][]byte, key []byte) bool {
	for _, k := range ks {
		if bytes.Equal(k, key) {
			return true
		}
	}
	return false
}

// TestTxnAtomicAbortsWholeShardOnOneConflict: level 1 (intra-shard). Two keys in ONE shard, one
// of them raced. The atomic commit must apply neither -- including the key that would have
// committed fine on its own.
func TestTxnAtomicAbortsWholeShardOnOneConflict(t *testing.T) {
	c := New(Options{Shards: 1})
	_ = c.Put([]byte("a"), mustAd(t, `[ N = 0 ]`))
	_ = c.Put([]byte("b"), mustAd(t, `[ N = 0 ]`))

	tx := c.Begin()
	tx.SetAtomic(true)
	tx.Put([]byte("a"), mustAd(t, `[ N = 1 ]`))
	tx.Put([]byte("b"), mustAd(t, `[ N = 1 ]`))

	// Another committer takes "b" out from under the transaction's snapshot.
	_ = c.Put([]byte("b"), mustAd(t, `[ N = 99 ]`))

	res := tx.Commit()
	if res.Committed != 0 {
		t.Fatalf("Committed = %d, want 0 (atomic abort)", res.Committed)
	}
	if !res.Conflicted() || !hasKey(res.Conflicts, []byte("b")) {
		t.Fatalf("Conflicts = %q, want it to name b", res.Conflicts)
	}
	if hasKey(res.Conflicts, []byte("a")) {
		t.Fatalf("Conflicts = %q, want a NOT named: a did not conflict, the batch was refused", res.Conflicts)
	}
	mustN(t, c, []byte("a"), 0) // never applied, though it was conflict-free
	mustN(t, c, []byte("b"), 99)
}

// TestTxnAtomicAbortsAcrossShards: level 2 (cross-shard). Writes spread over several shards, a
// conflict in exactly one of them, and nothing anywhere is applied.
func TestTxnAtomicAbortsAcrossShards(t *testing.T) {
	c := New(Options{Shards: 8})
	keys := atomicKeysInDistinctShards(t, c, 4)
	for _, k := range keys {
		_ = c.Put(k, mustAd(t, `[ N = 0 ]`))
	}
	// Snapshot each shard's commit sequence; an aborted batch must not move any of them.
	seqBefore := map[int]uint64{}
	for _, k := range keys {
		idx := atomicShardOf(c, k)
		sh := c.shards[idx]
		sh.mu.RLock()
		seqBefore[idx] = sh.commitSeq
		sh.mu.RUnlock()
	}

	tx := c.Begin()
	tx.SetAtomic(true)
	for _, k := range keys {
		tx.Put(k, mustAd(t, `[ N = 1 ]`))
	}
	// Race only the LAST key. Sorted ascending by shard index it may be checked first or last;
	// either way the whole batch must go.
	raced := keys[len(keys)-1]
	_ = c.Put(raced, mustAd(t, `[ N = 99 ]`))

	res := tx.Commit()
	if res.Committed != 0 {
		t.Fatalf("Committed = %d, want 0", res.Committed)
	}
	if len(res.Conflicts) != 1 || !hasKey(res.Conflicts, raced) {
		t.Fatalf("Conflicts = %q, want exactly [%s]", res.Conflicts, raced)
	}
	for _, k := range keys {
		if bytes.Equal(k, raced) {
			mustN(t, c, k, 99)
			continue
		}
		mustN(t, c, k, 0)
		idx := atomicShardOf(c, k)
		sh := c.shards[idx]
		sh.mu.RLock()
		got := sh.commitSeq
		sh.mu.RUnlock()
		if got != seqBefore[idx] {
			t.Fatalf("shard %d commitSeq moved %d -> %d under an aborted atomic commit", idx, seqBefore[idx], got)
		}
	}
}

// TestTxnAtomicReportsEveryConflict: the check pass runs to the end, so a caller learns its whole
// conflicting working set from one aborted commit rather than one key per attempt.
func TestTxnAtomicReportsEveryConflict(t *testing.T) {
	c := New(Options{Shards: 8})
	keys := atomicKeysInDistinctShards(t, c, 4)
	for _, k := range keys {
		_ = c.Put(k, mustAd(t, `[ N = 0 ]`))
	}
	tx := c.Begin()
	tx.SetAtomic(true)
	for _, k := range keys {
		tx.Put(k, mustAd(t, `[ N = 1 ]`))
	}
	// Race three of the four.
	for _, k := range keys[1:] {
		_ = c.Put(k, mustAd(t, `[ N = 99 ]`))
	}
	res := tx.Commit()
	if res.Committed != 0 {
		t.Fatalf("Committed = %d, want 0", res.Committed)
	}
	if len(res.Conflicts) != 3 {
		t.Fatalf("Conflicts = %q, want all 3 raced keys named, not just the first", res.Conflicts)
	}
	for _, k := range keys[1:] {
		if !hasKey(res.Conflicts, k) {
			t.Fatalf("Conflicts = %q, missing %s", res.Conflicts, k)
		}
	}
}

// TestTxnAtomicCommitsCleanBatch: the happy path still commits, across shards, and every write
// is visible afterwards.
func TestTxnAtomicCommitsCleanBatch(t *testing.T) {
	c := New(Options{Shards: 8})
	keys := atomicKeysInDistinctShards(t, c, 5)
	tx := c.Begin()
	tx.SetAtomic(true)
	if !tx.Atomic() {
		t.Fatal("SetAtomic(true) did not take")
	}
	for _, k := range keys {
		tx.Put(k, mustAd(t, `[ N = 7 ]`))
	}
	res := tx.Commit()
	if res.Conflicted() || res.HasUnapplied() || res.Committed != len(keys) {
		t.Fatalf("commit = %+v, want %d committed clean", res, len(keys))
	}
	for _, k := range keys {
		mustN(t, c, k, 7)
	}
}

// TestTxnAtomicDeletesAreAtomicToo: a batch of deletes aborts whole, leaving every key present.
func TestTxnAtomicDeletesAreAtomicToo(t *testing.T) {
	c := New(Options{Shards: 4})
	keys := atomicKeysInDistinctShards(t, c, 3)
	for _, k := range keys {
		_ = c.Put(k, mustAd(t, `[ N = 0 ]`))
	}
	tx := c.Begin()
	tx.SetAtomic(true)
	for _, k := range keys {
		tx.Delete(k)
	}
	_ = c.Put(keys[0], mustAd(t, `[ N = 5 ]`)) // race one
	res := tx.Commit()
	if res.Committed != 0 || len(res.Conflicts) != 1 {
		t.Fatalf("commit = %+v, want 0 committed and 1 conflict", res)
	}
	for _, k := range keys {
		if _, ok := c.Get(k); !ok {
			t.Fatalf("%s was deleted by an aborted atomic commit", k)
		}
	}
}

// TestTxnNonAtomicStillPartiallyApplies: the DEFAULT is unchanged. Same shape as the atomic
// tests, without SetAtomic: the clean key commits and only the raced one is reported.
func TestTxnNonAtomicStillPartiallyApplies(t *testing.T) {
	c := New(Options{Shards: 1})
	_ = c.Put([]byte("a"), mustAd(t, `[ N = 0 ]`))
	_ = c.Put([]byte("b"), mustAd(t, `[ N = 0 ]`))

	tx := c.Begin() // no SetAtomic
	if tx.Atomic() {
		t.Fatal("a fresh transaction must not be atomic")
	}
	tx.Put([]byte("a"), mustAd(t, `[ N = 1 ]`))
	tx.Put([]byte("b"), mustAd(t, `[ N = 1 ]`))
	_ = c.Put([]byte("b"), mustAd(t, `[ N = 99 ]`))

	res := tx.Commit()
	if res.Committed != 1 {
		t.Fatalf("Committed = %d, want 1 (default is partial application)", res.Committed)
	}
	if len(res.Conflicts) != 1 || !hasKey(res.Conflicts, []byte("b")) {
		t.Fatalf("Conflicts = %q, want exactly [b]", res.Conflicts)
	}
	mustN(t, c, []byte("a"), 1) // the clean key DID commit -- the behaviour callers depend on
	mustN(t, c, []byte("b"), 99)
}

// TestTxnNonAtomicPartialAcrossShards: the default also still applies per shard.
func TestTxnNonAtomicPartialAcrossShards(t *testing.T) {
	c := New(Options{Shards: 8})
	keys := atomicKeysInDistinctShards(t, c, 4)
	for _, k := range keys {
		_ = c.Put(k, mustAd(t, `[ N = 0 ]`))
	}
	tx := c.Begin()
	for _, k := range keys {
		tx.Put(k, mustAd(t, `[ N = 1 ]`))
	}
	raced := keys[0]
	_ = c.Put(raced, mustAd(t, `[ N = 99 ]`))
	res := tx.Commit()
	if res.Committed != len(keys)-1 {
		t.Fatalf("Committed = %d, want %d", res.Committed, len(keys)-1)
	}
	for _, k := range keys {
		if bytes.Equal(k, raced) {
			mustN(t, c, k, 99)
			continue
		}
		mustN(t, c, k, 1)
	}
}

// TestTxnAtomicUnappliedAbortsAndIsNotAConflict: a write that can NEVER succeed (ErrTooDeep)
// aborts an atomic batch -- otherwise the batch would quietly apply everything but that write,
// which is exactly the partial application atomic mode exists to prevent -- and it is reported
// as Unapplied, never as a Conflict, so a conflict-retry loop does not spin on it.
func TestTxnAtomicUnappliedAbortsAndIsNotAConflict(t *testing.T) {
	c := New(Options{Shards: 4})
	_ = c.Put([]byte("keep"), mustAd(t, `[ N = 0 ]`))

	tx := c.Begin()
	tx.SetAtomic(true)
	tx.Put([]byte("keep"), mustAd(t, `[ N = 1 ]`))
	tx.Put([]byte("deep"), chainAd(t, overTerms))

	res := tx.Commit()
	if res.Committed != 0 {
		t.Fatalf("Committed = %d, want 0: an unapplied write aborts an atomic batch", res.Committed)
	}
	if res.Conflicted() {
		t.Fatalf("Conflicts = %q, want empty: an unapplied write is not a conflict and retrying it is futile", res.Conflicts)
	}
	if !res.HasUnapplied() || !hasKey(res.Unapplied, []byte("deep")) {
		t.Fatalf("Unapplied = %q, want it to name deep", res.Unapplied)
	}
	mustN(t, c, []byte("keep"), 0) // the companion write did not sneak through
	if _, ok := c.Get([]byte("deep")); ok {
		t.Fatal("the too-deep key was stored")
	}

	// And the DEFAULT path still applies the rest, as it always did.
	tx2 := c.Begin()
	tx2.Put([]byte("keep"), mustAd(t, `[ N = 2 ]`))
	tx2.Put([]byte("deep2"), chainAd(t, overTerms))
	res2 := tx2.Commit()
	if res2.Committed != 1 || !res2.HasUnapplied() {
		t.Fatalf("non-atomic commit = %+v, want 1 committed and 1 unapplied", res2)
	}
	mustN(t, c, []byte("keep"), 2)
}

// orderedNs lists the N values of the one global partition of index 0, in sort order.
func orderedNs(c *Collection) []int64 {
	var out []int64
	for r := range c.Ordered(0, classad.Value{}, OrderCursor{}) {
		v, _ := r.Ad.EvaluateAttrInt("N")
		out = append(out, v)
	}
	return out
}

// TestTxnAtomicAbortTouchesNoOrderedIndex: nothing is applied, so the ordered index and the
// watch stream must not see the batch either. Both are driven off the per-write ok flag, which
// an abort leaves false everywhere -- this test is what keeps that true.
func TestTxnAtomicAbortTouchesNoOrderedIndex(t *testing.T) {
	c := New(Options{
		Shards:  4,
		Ordered: []OrderSpec{{Keys: []SortKey{{Expr: "N"}}}},
	})
	_ = c.Put([]byte("a"), mustAd(t, `[ N = 1 ]`))
	_ = c.Put([]byte("b"), mustAd(t, `[ N = 2 ]`))
	before := orderedNs(c)
	if len(before) != 2 {
		t.Fatalf("ordered index holds %v, want 2 members", before)
	}

	tx := c.Begin()
	tx.SetAtomic(true)
	tx.Put([]byte("a"), mustAd(t, `[ N = 30 ]`))
	tx.Delete([]byte("b"))
	_ = c.Put([]byte("a"), mustAd(t, `[ N = 1 ]`)) // race a
	res := tx.Commit()
	if res.Committed != 0 {
		t.Fatalf("Committed = %d, want 0", res.Committed)
	}
	if after := orderedNs(c); !equalInt64s(before, after) {
		t.Fatalf("ordered index changed under an aborted atomic commit: %v -> %v", before, after)
	}
	if _, ok := c.Get([]byte("b")); !ok {
		t.Fatal("b was deleted by an aborted atomic commit")
	}
}

func equalInt64s(a, b []int64) bool {
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

// TestTxnAtomicAbortPublishesNothing: a watcher must not see an aborted batch. The cursor is
// taken after the racing write and before the commit, so anything the catch-up replays came
// from the commit -- and nothing should have.
func TestTxnAtomicAbortPublishesNothing(t *testing.T) {
	c := New(Options{Shards: 4, WatchHistory: 64})
	_ = c.Put([]byte("a"), mustAd(t, `[ N = 0 ]`))
	_ = c.Put([]byte("b"), mustAd(t, `[ N = 0 ]`))

	tx := c.Begin()
	tx.SetAtomic(true)
	tx.Put([]byte("a"), mustAd(t, `[ N = 1 ]`))
	tx.Put([]byte("b"), mustAd(t, `[ N = 1 ]`))
	_ = c.Put([]byte("b"), mustAd(t, `[ N = 99 ]`)) // the race

	cursor, err := c.WatchCursor()
	if err != nil {
		t.Fatal(err)
	}
	res := tx.Commit()
	if res.Committed != 0 {
		t.Fatalf("Committed = %d, want 0", res.Committed)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ns, _, _ := drainCatchup(t, mustWatch(t, c, ctx, cursor))
	if len(ns) != 0 {
		t.Fatalf("an aborted atomic commit published %v, want no events", ns)
	}
}

// TestTxnAtomicFastPathChecksNothing: an uncontended atomic commit must pay NO per-key conflict
// check -- the single-writer fast path short-circuits the check pass exactly as it short-circuits
// the interleaved loop. This is the schedd's common case and the reason the fast path exists.
func TestTxnAtomicFastPathChecksNothing(t *testing.T) {
	c := New(Options{Shards: 4})
	keys := atomicKeysInDistinctShards(t, c, 4)
	before := ConflictChecks()
	for i := range 20 {
		tx := c.Begin()
		tx.SetAtomic(true)
		for _, k := range keys {
			tx.Put(k, mustAd(t, fmt.Sprintf(`[ N = %d ]`, i)))
		}
		if res := tx.Commit(); res.Committed != len(keys) {
			t.Fatalf("iteration %d: %+v", i, res)
		}
	}
	if got := ConflictChecks() - before; got != 0 {
		t.Fatalf("single-writer atomic commits performed %d conflict checks, want 0", got)
	}
}

// TestTxnAtomicConcurrentOppositeOrdersNoDeadlock: two goroutines run atomic commits over the
// SAME two shards, buffering their keys in opposite order. The lock order is the shard index,
// not the order the keys were buffered, so neither can hold what the other waits on. It fails by
// hanging; the watchdog turns a hang into a failure instead of a package-wide timeout.
func TestTxnAtomicConcurrentOppositeOrdersNoDeadlock(t *testing.T) {
	c := New(Options{Shards: 16})
	pair := atomicKeysInDistinctShards(t, c, 2)
	k1, k2 := pair[0], pair[1]
	_ = c.Put(k1, mustAd(t, `[ N = 0 ]`))
	_ = c.Put(k2, mustAd(t, `[ N = 0 ]`))

	const iters = 400
	var wg sync.WaitGroup
	// Each worker buffers in its own order; worker 1 writes k1 then k2, worker 2 the reverse.
	run := func(first, second []byte) {
		defer wg.Done()
		for i := range iters {
			tx := c.Begin()
			tx.SetAtomic(true)
			tx.Put(first, mustAd(t, fmt.Sprintf(`[ N = %d ]`, i)))
			tx.Put(second, mustAd(t, fmt.Sprintf(`[ N = %d ]`, i)))
			res := tx.Commit()
			// Either outcome is legal under contention; the point is that it RETURNS, and that
			// it never lands in between.
			if res.Committed != 0 && res.Committed != 2 {
				t.Errorf("atomic commit applied %d of 2 writes -- partial application", res.Committed)
				return
			}
		}
	}
	wg.Add(2)
	go run(k1, k2)
	go run(k2, k1)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("concurrent atomic commits deadlocked")
	}
}

// TestTxnAtomicConcurrentNeverTorn: under contention an atomic commit's two keys always move
// together. Every committed state has them equal, and a snapshot read sees only committed
// states, so a reader that ever finds them unequal has observed a half-applied batch.
//
// Both keys are deliberately in ONE shard. A transaction captures its snapshot lazily, per
// shard, on first touch (Txn.snapOf), so a transaction reading two keys in two shards takes its
// two snapshots at two different instants and can straddle an unrelated commit. That is a
// property of the read path, not of this commit mode, and it predates it -- an earlier draft of
// this test read across shards and caught exactly that, not a torn write. Within one shard the
// snapshot is a single sequence and the read IS consistent, which is what makes this assertion
// meaningful rather than flaky. The cross-shard case is covered deterministically by
// TestTxnAtomicAbortsAcrossShards and under contention by the quiescent check below.
func TestTxnAtomicConcurrentNeverTorn(t *testing.T) {
	c := New(Options{Shards: 1})
	k1, k2 := []byte("k1"), []byte("k2")
	_ = c.Put(k1, mustAd(t, `[ N = 0 ]`))
	_ = c.Put(k2, mustAd(t, `[ N = 0 ]`))

	var writers sync.WaitGroup
	for w := range 4 {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			for i := 1; i <= 300; i++ {
				v := w*1000 + i
				tx := c.Begin()
				tx.SetAtomic(true)
				tx.Put(k1, mustAd(t, fmt.Sprintf(`[ N = %d ]`, v)))
				tx.Put(k2, mustAd(t, fmt.Sprintf(`[ N = %d ]`, v)))
				tx.Commit()
			}
		}(w)
	}

	stop := make(chan struct{})
	var reader sync.WaitGroup
	reader.Add(1)
	go func() {
		defer reader.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			tx := c.Begin()
			a, ok1 := tx.Get(k1)
			b, ok2 := tx.Get(k2)
			if !ok1 || !ok2 {
				continue
			}
			av, _ := a.EvaluateAttrInt("N")
			bv, _ := b.EvaluateAttrInt("N")
			if av != bv {
				t.Errorf("torn atomic commit observed: %s=%d %s=%d", k1, av, k2, bv)
				return
			}
		}
	}()

	done := make(chan struct{})
	go func() { writers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		close(stop)
		reader.Wait()
		t.Fatal("contended atomic commits did not finish")
	}
	close(stop)
	reader.Wait()
}

// TestTxnAtomicConcurrentCrossShardQuiesces: the cross-shard version of the above, checked once
// the writers are done. A reader cannot sample two shards consistently (see the note on
// TestTxnAtomicConcurrentNeverTorn), but if any contended atomic commit had applied in halves
// the two keys would be left disagreeing, and every commit asserts it applied 0 or 2 writes.
func TestTxnAtomicConcurrentCrossShardQuiesces(t *testing.T) {
	c := New(Options{Shards: 16})
	pair := atomicKeysInDistinctShards(t, c, 2)
	k1, k2 := pair[0], pair[1]
	if atomicShardOf(c, k1) == atomicShardOf(c, k2) {
		t.Fatal("test wants two shards")
	}
	_ = c.Put(k1, mustAd(t, `[ N = 0 ]`))
	_ = c.Put(k2, mustAd(t, `[ N = 0 ]`))

	var writers sync.WaitGroup
	for w := range 4 {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			for i := 1; i <= 300; i++ {
				v := w*1000 + i
				tx := c.Begin()
				tx.SetAtomic(true)
				tx.Put(k1, mustAd(t, fmt.Sprintf(`[ N = %d ]`, v)))
				tx.Put(k2, mustAd(t, fmt.Sprintf(`[ N = %d ]`, v)))
				if res := tx.Commit(); res.Committed != 0 && res.Committed != 2 {
					t.Errorf("atomic commit applied %d of 2 writes -- partial application", res.Committed)
					return
				}
			}
		}(w)
	}
	done := make(chan struct{})
	go func() { writers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("contended cross-shard atomic commits did not finish")
	}
	a, ok1 := c.Get(k1)
	b, ok2 := c.Get(k2)
	if !ok1 || !ok2 {
		t.Fatal("keys missing after contended atomic commits")
	}
	av, _ := a.EvaluateAttrInt("N")
	bv, _ := b.EvaluateAttrInt("N")
	if av != bv {
		t.Fatalf("after quiescing, %s=%d %s=%d -- some atomic commit applied in halves", k1, av, k2, bv)
	}
}
