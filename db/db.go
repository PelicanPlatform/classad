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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// DB is an embedded ClassAd log. Safe for concurrent use.
type DB struct {
	c        *collections.Collection
	id       string // stable database identity (persisted; random for in-memory)
	instance string // this open's instance identity (fresh each Open)
}

// OrderSpec, SortKey, OrderedAd, OrderCursor configure and drive maintained ordered
// indexes (the schedd priority-queue / resource-request-list pattern). Re-exported
// from collections so callers of db need not import it.
type (
	OrderSpec   = collections.OrderSpec
	SortKey     = collections.SortKey
	OrderedAd   = collections.OrderedAd
	OrderCursor = collections.OrderCursor
)

// Config opens a DB with indexing and ordered-index configuration. Dir empty is
// in-memory; a non-empty Dir is persistent.
type Config struct {
	Dir string
	// Ordered configures maintained, filtered, sorted indexes -- e.g. the negotiator's
	// resource-request lists (partition by Owner, sort by JobPrio then QDate), iterated
	// in order via Ordered. Optional.
	Ordered []OrderSpec
	// HotAttrs / CategoricalAttrs / ValueAttrs / MatchClosureRoots tune storage and
	// query/match push-down (see collections.Options). Optional.
	HotAttrs                     []string
	CategoricalAttrs, ValueAttrs []string
	MatchClosureRoots            []string
}

// Open opens a ClassAd log with default configuration. A non-empty dir makes it
// persistent (memory-mapped arenas under dir, recovered on reopen); an empty dir is
// in-memory. See OpenConfig for indexing / ordered-index configuration.
func Open(dir string) (*DB, error) { return OpenConfig(Config{Dir: dir}) }

// OpenConfig opens a ClassAd log with the given configuration. Every DB is stamped
// with a stable DB id (persisted alongside a persistent store, so it survives reopen)
// and a fresh instance id for this open. Until high-availability DB servers exist they
// are effectively the same identity, but a follower/replica shares the DB id while
// carrying its own instance id.
func OpenConfig(cfg Config) (*DB, error) {
	opts := collections.Options{
		Dir:               cfg.Dir,
		WatchHistory:      4096, // enables Watch
		Ordered:           cfg.Ordered,
		HotAttrs:          cfg.HotAttrs,
		CategoricalAttrs:  cfg.CategoricalAttrs,
		ValueAttrs:        cfg.ValueAttrs,
		MatchClosureRoots: cfg.MatchClosureRoots,
	}
	var c *collections.Collection
	if cfg.Dir == "" {
		c = collections.New(opts)
	} else {
		var err error
		if c, err = collections.Open(opts); err != nil {
			return nil, err
		}
	}
	return &DB{c: c, id: loadOrCreateDBID(cfg.Dir), instance: randID()}, nil
}

// ID is the stable database identity (same across reopens of a persistent store).
func (db *DB) ID() string { return db.id }

// InstanceID is this open's identity (fresh each Open). Equal in spirit to ID until
// HA replicas exist, when replicas of one DB share ID but differ by InstanceID.
func (db *DB) InstanceID() string { return db.instance }

func randID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// loadOrCreateDBID reads dir/dbid, creating it with a fresh id on first open. An
// in-memory DB (empty dir) gets a random, non-persistent id.
func loadOrCreateDBID(dir string) string {
	if dir == "" {
		return randID()
	}
	p := filepath.Join(dir, "dbid")
	if data, err := os.ReadFile(p); err == nil {
		if s := strings.TrimSpace(string(data)); s != "" {
			return s
		}
	}
	id := randID()
	_ = os.WriteFile(p, []byte(id+"\n"), 0o644)
	return id
}

// Close releases the log's resources.
func (db *DB) Close() error { return db.c.Close() }

// StartMaintenance starts the background self-tuning goroutines (compression
// dictionary retrain + hot-attribute refresh) on the given interval, returning a stop
// function. A server owns this rather than the caller polling. See
// collections.StartAutoRetrain.
func (db *DB) StartMaintenance(interval time.Duration) (stop func()) {
	return db.c.StartAutoRetrain(interval, 4096, 32)
}

// SuggestIndexes samples the store and returns attributes that queries filter on but
// are not yet indexed (advisory; a server may log or auto-apply them).
func (db *DB) SuggestIndexes(sampleMax int) []collections.IndexSuggestion {
	return db.c.SuggestIndexes(sampleMax)
}

// Len returns the number of committed ads.
func (db *DB) Len() int { return db.c.Len() }

// LookupClassAd returns the committed ad for key (the hash table, outside any
// transaction), or (nil, false).
func (db *DB) LookupClassAd(key string) (*classad.ClassAd, bool) {
	return db.c.Get([]byte(key))
}

// Keys returns every committed key at a consistent snapshot, in no particular
// order. Useful for administrative enumeration and for a replica that must clear
// its keyspace before a full re-sync (see the leader-follower replicator).
func (db *DB) Keys() []string { return db.c.Keys() }

// Diagnostic and management types, re-exported from collections for callers that
// only import db.
type (
	Stats           = collections.Stats
	IndexSuggestion = collections.IndexSuggestion
	DropSuggestion  = collections.DropSuggestion
	QueryExplain    = collections.QueryExplain
)

// Stats returns a snapshot of the store's storage (ad count, segment/arena/dead
// bytes) for observability.
func (db *DB) Stats() Stats { return db.c.Stats() }

// HotAttrs returns the current hot attributes (front-loaded in each ad's hot
// header for cheap access).
func (db *DB) HotAttrs() []string { return db.c.HotAttrNames() }

// IndexedAttrs returns the currently-indexed attribute names, split into
// categorical (string equality/membership) and value (numeric + range) indexes.
func (db *DB) IndexedAttrs() (categorical, value []string) { return db.c.IndexedAttrs() }

// SuggestDrops recommends indexes to drop (unused or low-cardinality) from
// observed demand and a sample of up to sampleMax live ads.
func (db *DB) SuggestDrops(sampleMax int) []DropSuggestion { return db.c.SuggestDrops(sampleMax) }

// Explain reports how the store would execute a constraint query -- which
// conjuncts are index-usable and the resulting access path.
func (db *DB) Explain(constraint string) (QueryExplain, error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return QueryExplain{}, fmt.Errorf("classad-db: bad constraint %q: %w", constraint, err)
	}
	return db.c.ExplainQuery(q), nil
}

// AddIndex adds categorical and/or value indexes at runtime, returning whether
// the configuration changed. Newly-indexed attributes are backfilled.
func (db *DB) AddIndex(categorical, value []string) bool { return db.c.AddIndex(categorical, value) }

// DropIndex removes the named attributes from the configured indexes, returning
// whether the configuration changed.
func (db *DB) DropIndex(names ...string) bool { return db.c.DropIndex(names...) }

// Reindex rebuilds all configured indexes from the live ads.
func (db *DB) Reindex() { db.c.Reindex() }

// AddHotAttrs pins the named attributes into the hot set and returns the
// resulting hot attributes.
func (db *DB) AddHotAttrs(names ...string) []string { return db.c.AddHotAttrs(names...) }

// RefreshHotSet recomputes the hot set as the topN most frequent attributes from
// a sample of up to sampleMax live ads, returning how many were chosen.
func (db *DB) RefreshHotSet(sampleMax, topN int) int { return db.c.RefreshHotSet(sampleMax, topN) }

// Compact reclaims dead space in shards whose dead-byte ratio warrants it,
// returning the number of shards compacted.
func (db *DB) Compact() int { return db.c.Compact() }

// Rewrite re-encodes every live ad with the current hot set (so a changed hot
// set applies to existing ads) and force-compacts, returning the number of ads
// rewritten. A maintenance operation -- see collections.Collection.Rewrite.
func (db *DB) Rewrite() int { return db.c.Rewrite() }

// ForEach calls fn for every committed ad, in no particular order, until fn returns
// false. It reads a consistent snapshot (concurrent writers do not block it).
func (db *DB) ForEach(fn func(ad *classad.ClassAd) bool) {
	for ad := range db.c.Scan() {
		if !fn(ad) {
			return
		}
	}
}

// Query returns the committed ads matching the constraint expression (an "old
// ClassAd" boolean expression over each ad, e.g. `JobStatus == 2 && Owner == "alice"`).
// The store pushes the filter down -- indexed constraints visit only candidates -- so
// this is far cheaper than ForEach + client-side filtering. Errors only on a malformed
// constraint.
func (db *DB) Query(constraint string) (iter.Seq[*classad.ClassAd], error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("classad-db: bad constraint %q: %w", constraint, err)
	}
	return db.c.Query(q), nil
}

// QueryProject returns, for each ad matching the constraint, just the named
// attributes' values (aligned with attrs), read wire-native where possible so an
// aggregate or projection does not pay the full-ad decode Query costs. The
// yielded slice is reused across iterations; copy any value to retain it past the
// next step. Errors only on a malformed constraint.
func (db *DB) QueryProject(constraint string, attrs []string) (iter.Seq[[]classad.Value], error) {
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("classad-db: bad constraint %q: %w", constraint, err)
	}
	return db.c.QueryProject(q, attrs), nil
}

// Match returns the ads that symmetrically match job (bilateral Requirements), pushed
// down to the store. For the negotiator's pick-best pattern, prefer MatchSorted.
func (db *DB) Match(job *classad.ClassAd) iter.Seq[*classad.ClassAd] {
	return db.c.Match(job)
}

// MatchSorted returns job's matches ranked by the job's Rank, best first, at most
// limit (<=0 = all) -- the negotiator resource-request path, with the store's deferred
// materialization so only the returned top-N are built.
func (db *DB) MatchSorted(job *classad.ClassAd, limit int) []*classad.ClassAd {
	return db.c.MatchSorted(job, limit)
}

// Ordered iterates one partition of the index-th configured ordered index in sort
// order (Config.Ordered), yielding each member ad with a resume cursor and its cluster
// signature (for run-length folding into resource-request lists). partition selects the
// run (e.g. an Owner); it is ignored for an index with no Partition. A zero resume
// starts at the beginning. The snapshot is O(1) and stable under concurrent churn.
func (db *DB) Ordered(index int, partition string, resume OrderCursor) iter.Seq[OrderedAd] {
	return db.c.Ordered(index, classad.NewStringValue(partition), resume)
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

// CommitNondurable is Commit that defers the disk durability sync (classad_log.h
// CommitNondurableTransaction): the writes are visible immediately but their flush is
// batched to a later durable commit. On an in-memory DB it is identical to Commit.
func (t *Txn) CommitNondurable() error {
	t.tx.SetDurable(false)
	return t.Commit()
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
