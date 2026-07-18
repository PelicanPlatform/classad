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
