package db

import (
	"fmt"
	"iter"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// These bound the optimistic-retry loops so a pathological amount of concurrent
// churn on the same keys cannot spin forever. They are generous: a well-behaved
// workload commits on the first attempt.
const (
	// deleteBatch caps how many keys one DeleteWhere transaction stages, bounding
	// transaction size (and thus conflict blast radius) on a large match set.
	deleteBatch = 8192
	// maxDeleteRounds bounds DeleteWhere's scan/delete rounds. Each normal round
	// removes up to deleteBatch ads, so this covers a very large store; it is only
	// reached if the match set is continually refreshed out from under the sweep.
	maxDeleteRounds = 10000
	// maxWriteAttempts bounds a single-key write's optimistic retries.
	maxWriteAttempts = 32
)

// Put inserts or replaces the ad at key in its own optimistic transaction,
// retrying on a write-write conflict (another committer touched key since this
// attempt's snapshot) until it lands or maxWriteAttempts is exhausted. A blind
// overwrite that loses the optimistic race would otherwise be silently dropped;
// Put is the retrying convenience for the common single-ad upsert so callers do
// not each reimplement the loop. Equivalent to Begin + NewClassAd + Commit.
func (db *DB) Put(key string, ad *classad.ClassAd) error {
	return db.withWriteRetry(func(t *Txn) { t.NewClassAd(key, ad) })
}

// Delete removes the ad at key in its own optimistic transaction, retrying on a
// write-write conflict. It reports whether an ad was present to remove.
// Equivalent to Begin + DestroyClassAd + Commit with retry.
func (db *DB) Delete(key string) (bool, error) {
	present := false
	err := db.withWriteRetry(func(t *Txn) {
		if _, ok := t.LookupClassAd(key); ok {
			present = true
			t.DestroyClassAd(key)
		} else {
			present = false
		}
	})
	return present, err
}

// withWriteRetry runs stage in a fresh transaction and commits it, retrying the
// whole (re-snapshotted) transaction on a *ConflictError. stage must be
// idempotent across retries (it is re-run on the new snapshot each attempt),
// which every blind put/delete is. Non-conflict commit errors are returned
// immediately.
func (db *DB) withWriteRetry(stage func(*Txn)) error {
	var last *ConflictError
	for attempt := 0; attempt < maxWriteAttempts; attempt++ {
		t := db.Begin()
		stage(t)
		err := t.Commit()
		if err == nil {
			return nil
		}
		if ce, ok := err.(*ConflictError); ok {
			last = ce
			continue // another committer won this key; re-snapshot and retry
		}
		return err
	}
	return fmt.Errorf("classad-db: write did not commit after %d optimistic attempts (last conflict on %v)", maxWriteAttempts, last.Keys)
}

// DeleteWhere removes every ad matching constraint, in batched optimistic
// transactions, and returns the number removed. It is the server-side pushdown
// for a bulk invalidation or a time-based expiry sweep (express expiry as a
// constraint, e.g. "<now> > LastHeardFrom + Lifetime"), replacing a client's
// per-key query-then-delete loop with one call executed where the data lives.
//
// The match set is snapshotted ONCE, up front, and only that fixed set of keys is
// deleted -- rows that begin matching only after the scan are not chased. This is what
// guarantees termination: the working set can only shrink (a deleted or spared key is
// dropped; only a key whose delete lost an optimistic race is retried), so a writer that
// keeps producing new matches can no longer make the sweep spin. It is also the correct
// DELETE semantics -- the statement acts on the rows present when it began, like SQL --
// which the previous re-scan-every-round design violated (and which let a continuously
// refreshed match set, e.g. a hot "JobStatus is undefined", never converge).
//
// Within the deleting transaction each key is still re-checked against the live ad before
// removal, so a row edited out of the match set before it is reached is spared; a row
// rewritten concurrently conflicts on commit and is retried (bounded by maxDeleteRounds,
// now only ever reached under relentless rewrite of the SAME already-matched keys).
//
// It errors on a malformed constraint, a non-conflict commit failure, or if some already-
// matched keys still conflict after maxDeleteRounds; the returned count reflects what was
// removed so far.
// deleteWhereAfterScanHook, when non-nil, is invoked once right after DeleteWhere snapshots
// the match set. Test-only (nil in production); a test uses it to insert a new matching row
// after the snapshot and assert that row is NOT deleted (proving the snapshot-once semantics).
var deleteWhereAfterScanHook func()

func (db *DB) DeleteWhere(constraint string) (int, error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return 0, fmt.Errorf("classad-db: bad constraint %q: %w", constraint, err)
	}
	pending := db.matchingKeysAll(q) // one scan: the rows matching at statement start
	if deleteWhereAfterScanHook != nil {
		deleteWhereAfterScanHook()
	}
	total := 0
	for round := 0; len(pending) > 0 && round < maxDeleteRounds; round++ {
		var conflicted []string
		for i := 0; i < len(pending); i += deleteBatch {
			end := min(i+deleteBatch, len(pending))
			deleted, conf, derr := db.deleteMatching(q, pending[i:end])
			if derr != nil {
				return total, derr
			}
			total += deleted
			conflicted = append(conflicted, conf...)
		}
		pending = conflicted // retry ONLY keys whose delete lost a race; never re-scan for new matches
	}
	if len(pending) > 0 {
		return total, fmt.Errorf("classad-db: DeleteWhere left %d key(s) after %d rounds of write conflicts for %q (removed %d)", len(pending), maxDeleteRounds, constraint, total)
	}
	return total, nil
}

// KeysWhere returns an iterator over the storage keys of every row whose ad matches constraint.
// Unlike a query, it yields the DB keys themselves, so a caller can address matched rows for
// UPDATE/DELETE without depending on any self-reported key attribute in the ad. The constraint is
// parsed eagerly (a parse error returns before any scan); the scan is lazy and stops early if the
// caller's yield returns false. It matches against decoded ads (a full scan, like DeleteWhere).
func (db *DB) KeysWhere(constraint string) (iter.Seq[string], error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("classad-db: bad constraint %q: %w", constraint, err)
	}
	return func(yield func(string) bool) {
		db.c.ForEachAd(func(key string, ad *classad.ClassAd) bool {
			if q.Matches(ad) {
				return yield(key)
			}
			return true
		})
	}, nil
}

// matchingKeysAll collects every key whose ad matches q at the time of the scan. Unbounded
// by design: DeleteWhere snapshots the whole match set once (keys are small strings), rather
// than re-scanning per round, so the sweep cannot chase matches that appear after it starts.
func (db *DB) matchingKeysAll(q *vm.Query) []string {
	var keys []string
	db.c.ForEachAd(func(key string, ad *classad.ClassAd) bool {
		if q.Matches(ad) {
			keys = append(keys, key)
		}
		return true
	})
	return keys
}

// deleteMatching removes, in one optimistic transaction, the given candidate keys
// whose ads still match q as of the transaction's snapshot. Re-checking within
// the transaction spares an ad refreshed out of the match set before the
// snapshot; the optimistic commit spares one refreshed after it (its key
// conflicts and is reported, not removed). Returns the number actually removed and
// the keys that conflicted (concurrently rewritten) so the caller can retry just those.
func (db *DB) deleteMatching(q *vm.Query, keys []string) (int, []string, error) {
	t := db.Begin()
	staged := 0
	for _, k := range keys {
		ad, ok := t.LookupClassAd(k)
		if !ok {
			continue // already gone
		}
		if !q.Matches(ad) {
			continue // refreshed out of the match set since the scan: spare it
		}
		t.DestroyClassAd(k)
		staged++
	}
	if staged == 0 {
		t.Abort()
		return 0, nil, nil
	}
	err := t.Commit()
	if err == nil {
		return staged, nil, nil
	}
	if ce, ok := err.(*ConflictError); ok {
		// Partial commit: the non-conflicted deletes landed; the conflicted keys
		// (concurrently rewritten) did not, and are returned for a bounded retry.
		return staged - len(ce.Keys), ce.Keys, nil
	}
	return 0, nil, err
}
