package db

import (
	"errors"
	"fmt"
	"iter"
	"strings"

	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// ErrRawWireUnsupported reports that a table cannot serve the wire-form relay scan --
// today, that it is in-memory rather than persistent. Callers fall back to a text row
// stream, which every table can serve.
var ErrRawWireUnsupported = errors.New("classad-db: table does not support wire-form rows")

// UpdateOld ingests an ad from old-ClassAd wire text under key, skipping the AST
// build -- the wire-native ingest path (as collections.UpdateOld). It writes
// through the same shard storage, change log, and watch feed as a committed Put,
// but bypasses the optimistic-concurrency layer (last-writer-wins), which suits
// high-rate single-key upserts where per-key write-write races do not occur (a
// collector re-advertising its own ad).
//
// An encrypted store no longer takes the AST path wholesale. The wire-native encoder cannot seal, so the
// ingest defers to the sealing path for an ad that HAS something to seal and streams the rest (see
// collections.encodeOld) -- for job and history ads, nearly all of them.
func (db *DB) UpdateOld(key, text string) error {
	// The DB-wide lock held shared, exactly as Commit does, so a direct write is
	// atomic against an exclusive Truncate/Restore.
	db.snapMu.RLock()
	defer db.snapMu.RUnlock()
	return db.c.UpdateOld([]collections.OldAdUpdate{{Key: []byte(key), Text: text}})
}

// OldAdText is one keyed ad in old-ClassAd wire text, for UpdateOldBatch.
type OldAdText struct {
	Key  string
	Text string
}

// UpdateOldBatch ingests many ads (key + old-ClassAd text) in one shard-commit
// batch -- the wire-native bulk ingest, so a burst of upserts costs one commit
// instead of one per ad. Bypasses the optimistic-concurrency layer
// (last-writer-wins) like UpdateOld. An encrypted store keeps the batch: sealing is decided per ad inside
// the ingest (see UpdateOld), so one commit still covers the whole batch.
func (db *DB) UpdateOldBatch(items []OldAdText) error {
	if len(items) == 0 {
		return nil
	}
	batch := make([]collections.OldAdUpdate, len(items))
	for i, it := range items {
		batch[i] = collections.OldAdUpdate{Key: []byte(it.Key), Text: it.Text}
	}
	db.snapMu.RLock()
	defer db.snapMu.RUnlock()
	return db.c.UpdateOld(batch)
}

// QueryRaw yields each matching ad as a collections.RawAd -- the wire-form
// attribute strings decoded straight from the stored representation with no AST,
// for a persistent (inline) store as well as an in-memory one -- so a whole-ad
// result set can be relayed without materializing and re-encoding each ad. Errors
// only on a malformed constraint.
func (db *DB) QueryRaw(constraint string) (iter.Seq[collections.RawAd], error) {
	if s := strings.TrimSpace(constraint); s == "" || strings.EqualFold(s, "true") {
		return db.c.ScanRaw(), nil // match-all: full raw scan
	}
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("classad-db: bad constraint %q: %w", constraint, err)
	}
	return db.c.QueryRaw(q), nil
}

// QueryRawRedacted is QueryRaw with private (secret) attributes stripped inside
// the collection's decode walk -- an unprivileged consumer's whole-ad query pays
// no per-attribute re-classification and never renders a private value (see
// collections.ScanRawRedacted).
func (db *DB) QueryRawRedacted(constraint string) (iter.Seq[collections.RawAd], error) {
	if s := strings.TrimSpace(constraint); s == "" || strings.EqualFold(s, "true") {
		return db.c.ScanRawRedacted(), nil
	}
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("classad-db: bad constraint %q: %w", constraint, err)
	}
	return db.c.QueryRawRedacted(q), nil
}

// QueryRawProjected is QueryRaw restricted to the projected attribute names,
// applied inside the collection's decode walk: a non-projected attribute is
// skipped before any name resolution or value rendering, and a hot-header-
// covered projection is served from the hot header alone (see
// collections.ScanRawProjected). redact additionally strips private attributes.
// An empty projection means no attribute filter.
func (db *DB) QueryRawProjected(constraint string, projection []string, redact bool) (iter.Seq[collections.RawAd], error) {
	if s := strings.TrimSpace(constraint); s == "" || strings.EqualFold(s, "true") {
		return db.c.ScanRawProjected(projection, false, redact), nil
	}
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("classad-db: bad constraint %q: %w", constraint, err)
	}
	return db.c.QueryRawProjected(q, projection, false, redact), nil
}

// QueryRawProjectedRefs is QueryRawProjected that also carries the attributes the
// projected expressions reference, transitively, so each yielded ad EVALUATES
// self-contained (collections chaseRefs).
//
// The distinction matters whenever a projected attribute holds an expression rather than
// a literal, which is the norm for HTCondor data -- Requirements, Rank and friends are
// expressions over sibling attributes. Projecting to exactly the requested names drops
// those siblings, so the expression evaluates to undefined at the far end: asking for
// Requirements alone answers undefined where asking for the whole ad answers true.
//
// QueryRawProjected remains the right call for a relay that must reproduce HTCondor's
// query protocol, which specifies exactly the requested attributes and nothing more. Use
// this one when the recipient is going to EVALUATE what it receives.
func (db *DB) QueryRawProjectedRefs(constraint string, projection []string, redact bool) (iter.Seq[collections.RawAd], error) {
	if s := strings.TrimSpace(constraint); s == "" || strings.EqualFold(s, "true") {
		return db.c.ScanRawProjected(projection, true, redact), nil
	}
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("classad-db: bad constraint %q: %w", constraint, err)
	}
	return db.c.QueryRawProjected(q, projection, true, redact), nil
}

// QueryRawProjectedRefsStats is QueryRawProjectedRefs that also fills stats (may be nil) with the
// per-scan work breakdown, for EXPLAIN ANALYZE. An empty/true constraint takes the no-WHERE scan
// path and leaves stats zero.
func (db *DB) QueryRawProjectedRefsStats(constraint string, projection []string, redact bool, stats *collections.ScanStats) (iter.Seq[collections.RawAd], error) {
	if s := strings.TrimSpace(constraint); s == "" || strings.EqualFold(s, "true") {
		return db.c.ScanRawProjected(projection, true, redact), nil
	}
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("classad-db: bad constraint %q: %w", constraint, err)
	}
	return db.c.QueryRawProjectedStats(q, projection, true, redact, stats), nil
}

// QueryRawWire yields each matching ad as a self-contained WIRE-FORM ROW (an
// inline-names subset ad assembled by slice copies -- see
// collections.ScanRawWire): the relay form for shipping ads to a remote
// consumer with the old-ClassAd render deferred to that consumer's client edge.
// projection restricts the entries (empty = whole ad); redact strips private
// attributes at the source. At-rest-encrypted values are opened during assembly
// (the consumer holds no data key).
//
// Only a persistent (inline) store can serve wire rows. An in-memory table returns
// ErrRawWireUnsupported rather than an empty sequence: a relay scan that yields
// nothing is indistinguishable from a query that matched nothing, so returning one
// would turn every RAM-table query into a silent empty result at the consumer.
func (db *DB) QueryRawWire(constraint string, projection []string, redact bool) (iter.Seq[[]byte], error) {
	if !db.c.SupportsRawWire() {
		return nil, ErrRawWireUnsupported
	}
	if s := strings.TrimSpace(constraint); s == "" || strings.EqualFold(s, "true") {
		return db.c.ScanRawWire(projection, redact), nil
	}
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("classad-db: bad constraint %q: %w", constraint, err)
	}
	return db.c.QueryRawWire(q, projection, redact), nil
}
