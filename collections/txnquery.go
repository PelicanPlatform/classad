package collections

import (
	"iter"
	"sort"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// Query returns the ads matching q as the transaction sees them: the committed rows,
// with the transaction's own buffered writes overlaid (read-your-writes). A row the
// transaction deleted is absent, a row it rewrote is matched and yielded in its
// rewritten form, and a row it created is included if it matches -- so a query inside a
// transaction observes the transaction's own work, which Collection.Query cannot.
//
// Consistency: the committed half is read live, not at the transaction's snapshot, so
// this is read-committed plus read-your-writes rather than full snapshot isolation. Get
// does read at the snapshot, so a point lookup and a scan in the same transaction can
// disagree about a row a *different* transaction committed in between. Making the scan
// snapshot-consistent needs a scan-at-sequence path the store does not have; the
// transaction's own writes -- the thing callers actually reach for a transactional query
// to see -- are exact either way.
//
// Cost: with no buffered writes this is exactly Collection.Query, index pushdown and
// all. Once the transaction has staged a write the committed half becomes a full scan,
// because the overlay has to know each row's key to tell whether the transaction
// superseded it, and the indexed query path yields ads without keys. Transactions exist
// to batch writes, so that trade -- pay only after you have written -- keeps the common
// BEGIN/SELECT/COMMIT shape at full speed.
func (tx *Txn) Query(q *vm.Query) iter.Seq[*classad.ClassAd] {
	if len(tx.writes) == 0 {
		return tx.c.Query(q)
	}
	return func(yield func(*classad.ClassAd) bool) {
		if !tx.scanCommitted(q, func(_ string, ad *classad.ClassAd) bool { return yield(ad) }) {
			return
		}
		tx.forEachBufferedMatch(q, func(_ string, ad *classad.ClassAd) bool { return yield(ad) })
	}
}

// KeysWhere returns the storage keys of the rows matching q as the transaction sees
// them, with the same overlay and the same caveats as Query. It is what lets an UPDATE
// or DELETE inside a transaction address rows the transaction itself created.
func (tx *Txn) KeysWhere(q *vm.Query) iter.Seq[string] {
	return func(yield func(string) bool) {
		if !tx.scanCommitted(q, func(key string, _ *classad.ClassAd) bool { return yield(key) }) {
			return
		}
		tx.forEachBufferedMatch(q, func(key string, _ *classad.ClassAd) bool { return yield(key) })
	}
}

// scanCommitted visits every committed row matching q whose key the transaction has NOT
// buffered -- those are represented by the buffered version, which forEachBufferedMatch
// yields instead. Returns false if the caller stopped early.
func (tx *Txn) scanCommitted(q *vm.Query, visit func(key string, ad *classad.ClassAd) bool) bool {
	ok := true
	tx.c.ForEachAd(func(key string, ad *classad.ClassAd) bool {
		if _, superseded := tx.writes[key]; superseded {
			return true
		}
		if !q.Matches(ad) {
			return true
		}
		ok = visit(key, ad)
		return ok
	})
	return ok
}

// forEachBufferedMatch visits the transaction's own buffered rows that match q, in key
// order. Deletes are skipped: the row is gone as far as this transaction is concerned,
// and scanCommitted already excluded its committed version.
//
// Key order rather than map order so a query's results are reproducible across runs --
// SQL callers order explicitly, but a non-deterministic iteration makes tests flaky and
// diffs noisy for no benefit.
func (tx *Txn) forEachBufferedMatch(q *vm.Query, visit func(key string, ad *classad.ClassAd) bool) {
	keys := make([]string, 0, len(tx.writes))
	for key, buf := range tx.writes {
		if buf.del || buf.ad == nil {
			continue
		}
		if IsSystemKey(key) {
			continue // internal system record: hidden from client iteration, as in ForEachAd
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		// Through Get, so a buffered row is yielded exactly as a point lookup in this
		// transaction would return it -- including the parent merge on a chained
		// collection, which reading tx.writes directly would skip.
		ad, ok := tx.Get([]byte(key))
		if !ok {
			continue
		}
		if !q.Matches(ad) {
			continue
		}
		if !visit(key, ad) {
			return
		}
	}
}
