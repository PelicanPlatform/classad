package db

import (
	"fmt"
	"iter"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// UpdateOld ingests an ad from old-ClassAd wire text under key, skipping the AST
// build -- the wire-native ingest path (as collections.UpdateOld). It writes
// through the same shard storage, change log, and watch feed as a committed Put,
// but bypasses the optimistic-concurrency layer (last-writer-wins), which suits
// high-rate single-key upserts where per-key write-write races do not occur (a
// collector re-advertising its own ad).
//
// When the database is encrypted it falls back to parse + Put: the wire-native
// encoder does not seal, so an encrypted store must take the AST path to keep
// data encrypted at rest.
func (db *DB) UpdateOld(key, text string) error {
	if db.c.EncryptionEnabled() {
		ad, err := classad.ParseOld(text)
		if err != nil {
			return fmt.Errorf("classad-db: parse ad for %q: %w", key, err)
		}
		return db.Put(key, ad)
	}
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
// (last-writer-wins) like UpdateOld. Falls back to per-ad Put on an encrypted
// store (the wire-native encoder does not seal).
func (db *DB) UpdateOldBatch(items []OldAdText) error {
	if len(items) == 0 {
		return nil
	}
	if db.c.EncryptionEnabled() {
		for _, it := range items {
			if err := db.UpdateOld(it.Key, it.Text); err != nil {
				return err
			}
		}
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

// QueryRawWire yields each matching ad as a self-contained WIRE-FORM ROW (an
// inline-names subset ad assembled by slice copies -- see
// collections.ScanRawWire): the relay form for shipping ads to a remote
// consumer with the old-ClassAd render deferred to that consumer's client edge.
// projection restricts the entries (empty = whole ad); redact strips private
// attributes at the source. At-rest-encrypted values are opened during assembly
// (the consumer holds no data key). Only meaningful for persistent (inline)
// stores; an in-memory table yields nothing.
func (db *DB) QueryRawWire(constraint string, projection []string, redact bool) (iter.Seq[[]byte], error) {
	if s := strings.TrimSpace(constraint); s == "" || strings.EqualFold(s, "true") {
		return db.c.ScanRawWire(projection, redact), nil
	}
	q, err := vm.Parse(constraint)
	if err != nil {
		return nil, fmt.Errorf("classad-db: bad constraint %q: %w", constraint, err)
	}
	return db.c.QueryRawWire(q, projection, redact), nil
}
