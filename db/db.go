// Package db is an embedded ClassAd log: a persistent key->ClassAd store with
// optimistic multi-writer transactions, mirroring HTCondor's ClassAdLog
// (src/condor_utils/classad_log.h). It is the Go core that the cgo layer (package
// capi) exposes as C symbols for a C++ interface to sit on top of, and that the
// client/server module serves over CEDAR.
//
// It maps directly onto the collections store: the key->ClassAd table is a
// Collection, and each transaction is a collections.Txn (snapshot isolation,
// write-write conflicts, per-ad commit). Unlike classad_log.h -- which allows only
// one active transaction -- this supports any number of independent concurrent
// transactions, each a distinct *Txn.
package db

import (
	"fmt"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
)

// DB is an embedded ClassAd log. Safe for concurrent use.
type DB struct {
	c *collections.Collection
}

// Open opens a ClassAd log. A non-empty dir makes it persistent (memory-mapped
// arenas under dir, recovered on reopen); an empty dir is in-memory.
func Open(dir string) (*DB, error) {
	if dir == "" {
		return &DB{c: collections.New(collections.Options{})}, nil
	}
	c, err := collections.Open(collections.Options{Dir: dir})
	if err != nil {
		return nil, err
	}
	return &DB{c: c}, nil
}

// Close releases the log's resources.
func (db *DB) Close() error { return db.c.Close() }

// LookupClassAd returns the committed ad for key (the hash table, outside any
// transaction), or (nil, false).
func (db *DB) LookupClassAd(key string) (*classad.ClassAd, bool) {
	return db.c.Get([]byte(key))
}

// ForEach calls fn for every committed ad, in no particular order, until fn returns
// false. It reads a consistent snapshot (concurrent writers do not block it).
func (db *DB) ForEach(fn func(ad *classad.ClassAd) bool) {
	for ad := range db.c.Scan() {
		if !fn(ad) {
			return
		}
	}
}

// ConflictError reports the keys whose writes lost an optimistic write-write race at
// commit. The other writes in the transaction committed; the caller re-reads and
// retries the conflicted keys.
type ConflictError struct{ Keys []string }

func (e *ConflictError) Error() string {
	return fmt.Sprintf("classad-db: %d key(s) conflicted: %v", len(e.Keys), e.Keys)
}

// Txn is an independent optimistic transaction. Operations are buffered and applied
// at Commit under snapshot-isolation OCC. A *Txn is not safe for concurrent use by
// multiple goroutines; independent transactions are.
type Txn struct {
	tx   *collections.Txn
	db   *DB
	done bool
}

// Begin starts a new independent transaction.
func (db *DB) Begin() *Txn { return &Txn{tx: db.c.Begin(), db: db} }

// Commit applies the buffered operations. It returns a *ConflictError if any key was
// modified by another committer since this transaction's snapshot (the non-conflicted
// operations still committed), or nil on full success.
func (t *Txn) Commit() error {
	t.done = true
	res := t.tx.Commit()
	if res.Conflicted() {
		keys := make([]string, len(res.Conflicts))
		for i, k := range res.Conflicts {
			keys[i] = string(k)
		}
		return &ConflictError{Keys: keys}
	}
	return nil
}

// Abort discards the transaction's buffered operations. Nothing is written.
func (t *Txn) Abort() { t.done = true }

// NewClassAd stores ad under key (classad_log.h LogNewClassAd). An existing ad at
// key is replaced.
func (t *Txn) NewClassAd(key string, ad *classad.ClassAd) {
	t.tx.Put([]byte(key), ad)
}

// DestroyClassAd removes key (classad_log.h LogDestroyClassAd).
func (t *Txn) DestroyClassAd(key string) {
	t.tx.Delete([]byte(key))
}

// SetAttribute sets one attribute of key to the expression parsed from expr
// (classad_log.h LogSetAttribute) -- a read-modify-write within the transaction, so
// it composes with the transaction's own earlier writes to key. The ad is created if
// absent.
func (t *Txn) SetAttribute(key, name, expr string) error {
	e, err := classad.ParseExpr(expr)
	if err != nil {
		return fmt.Errorf("classad-db: SetAttribute %s[%s]: %w", key, name, err)
	}
	ad, ok := t.tx.Get([]byte(key))
	if !ok {
		ad = classad.New()
	}
	ad.InsertExpr(name, e)
	t.tx.Put([]byte(key), ad)
	return nil
}

// DeleteAttribute removes one attribute of key (classad_log.h LogDeleteAttribute).
// A no-op if key or the attribute is absent.
func (t *Txn) DeleteAttribute(key, name string) {
	ad, ok := t.tx.Get([]byte(key))
	if !ok {
		return
	}
	if ad.Delete(name) {
		t.tx.Put([]byte(key), ad)
	}
}

// LookupClassAd returns key's ad as the transaction sees it: its own buffered writes
// (read-your-writes) merged over the snapshot (classad_log.h Lookup + the
// LookupInTransaction overlay in one call).
func (t *Txn) LookupClassAd(key string) (*classad.ClassAd, bool) {
	return t.tx.Get([]byte(key))
}

// LookupAttr returns the unparsed expression of one attribute as the transaction
// sees it (classad_log.h LookupInTransaction), or ("", false).
func (t *Txn) LookupAttr(key, name string) (string, bool) {
	ad, ok := t.tx.Get([]byte(key))
	if !ok {
		return "", false
	}
	e, ok := ad.Lookup(name)
	if !ok {
		return "", false
	}
	return e.String(), true
}
