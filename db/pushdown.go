package db

import (
	"fmt"

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
// Each round re-scans on a fresh snapshot and, inside the deleting transaction,
// re-checks the constraint against the ad as of that snapshot before removing it.
// Two properties fall out of that:
//   - Self-healing under concurrency: an ad concurrently modified so that it no
//     longer matches (a re-advertised ad whose expiry constraint no longer holds)
//     is spared, not deleted; and a partial commit's conflicted keys are simply
//     re-evaluated on the next round.
//   - Idempotent and safe to run alongside writers or another sweep.
//
// It errors on a malformed constraint, a non-conflict commit failure, or if the
// sweep fails to converge within maxDeleteRounds (only reachable under relentless
// churn of the match set); the returned count reflects what was removed so far.
func (db *DB) DeleteWhere(constraint string) (int, error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return 0, fmt.Errorf("classad-db: bad constraint %q: %w", constraint, err)
	}
	total := 0
	for round := 0; round < maxDeleteRounds; round++ {
		keys := db.matchingKeys(q, deleteBatch)
		if len(keys) == 0 {
			return total, nil
		}
		deleted, err := db.deleteMatching(q, keys)
		if err != nil {
			return total, err
		}
		total += deleted
	}
	return total, fmt.Errorf("classad-db: DeleteWhere did not converge in %d rounds for %q (removed %d)", maxDeleteRounds, constraint, total)
}

// matchingKeys collects up to limit keys whose ads currently match q.
func (db *DB) matchingKeys(q *vm.Query, limit int) []string {
	keys := make([]string, 0, min(limit, 256))
	db.c.ForEachAd(func(key string, ad *classad.ClassAd) bool {
		if q.Matches(ad) {
			keys = append(keys, key)
		}
		return len(keys) < limit
	})
	return keys
}

// deleteMatching removes, in one optimistic transaction, the given candidate keys
// whose ads still match q as of the transaction's snapshot. Re-checking within
// the transaction spares an ad refreshed out of the match set before the
// snapshot; the optimistic commit spares one refreshed after it (its key
// conflicts and is reported, not removed). Returns the number actually removed.
func (db *DB) deleteMatching(q *vm.Query, keys []string) (int, error) {
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
		return 0, nil
	}
	err := t.Commit()
	if err == nil {
		return staged, nil
	}
	if ce, ok := err.(*ConflictError); ok {
		// Partial commit: the non-conflicted deletes landed; the conflicted keys
		// (concurrently rewritten) did not, and are re-evaluated next round.
		return staged - len(ce.Keys), nil
	}
	return 0, err
}
