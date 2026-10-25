package db

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// A table's tuned configuration -- which attributes are indexed, which are hot, which the
// columnar schema must carry, how much history to keep -- outlives the process that tuned
// it, so each table type writes it to one JSON file beside its own data: a mutable table to
// indexcfg.json, an archive to archiveconfig.json.
//
// configFile is that job, once. Each table type had its own copy of it before, and both
// copies had the same hole in different disguises. The archive's showed as a data race:
// its config struct was shared, so a background index backfill marshalling it while the
// foreground updated it was reported the first time anyone ran that pair under -race, and
// it was fixed there. The mutable table shares no struct, so -race had nothing to say
// about it and it looked fine -- but its savers raced on the same temp path just the same,
// and concurrent runtime changes left indexcfg.json unparseable or reverted. One hole, two
// copies, one of them fixed. Routing both through this type is what makes the next fix
// land everywhere it applies.
//
// THE INVARIANT, stated because its absence is how the race got in: one lock is held
// across BOTH the config update and the file write, for every writer of a given file.
// Not the field assignment alone. Two savers that unlock between the two steps collide on
// the shared .tmp path -- one truncates and fills it while the other is mid-write, and
// whichever renames first publishes a splice of both. The result is not a near miss: the
// file is left either unparseable or holding an older configuration, and nothing reports
// it. The next open quietly reverts to the creation-time index set.
//
// Deriving the config from live state at save time, which both tables do for the fields
// the storage layer owns, does not substitute for the lock and is not outside it. Read the
// live state before taking the lock and two savers can interleave into a lost update: each
// snapshots, the second writes, then the FIRST writes its older snapshot over the top, and
// the index one of them just built is absent from the file with nothing to say so. So the
// read goes inside -- fn is where a caller reads the storage layer, not before the call.
//
// That nests the storage layer's own lock inside this one. It is safe in the one direction
// used here, because the storage layer never calls back into a config file; a caller that
// changes that has to revisit it.
//
// What it owns, and what it deliberately does not:
//
//   - Owns: the path, the atomic rewrite (tmp + rename, so a reader never sees a partial
//     file), and the lock above.
//   - Owns: the in-memory case. A table with no directory (dir == "") persists nothing and
//     reads nothing back; update and save are no-ops that report success.
//   - Does NOT own: what goes in the struct, or what reopening does with it. The two tables
//     differ there and are meant to. The mutable table re-derives every field from the
//     collection on each save, because that is where it tracks index provenance and the
//     runtime required tier; the archive accumulates them in the held struct, because its
//     file is the only record of which entries a human asked for. An archive's file is
//     authoritative on reopen; a mutable table's is reconciled against the live index set.
//     Each load path restores provenance its own way.
//
// Error handling is the caller's to choose, which is why update and save return one rather
// than logging or ignoring it. Several callers are deliberately best-effort: a write failure
// leaves the change live for this run and unpersisted, and they discard the error at the
// call site where that decision is visible.
type configFile[T any] struct {
	// dir is the table's directory, or "" for an in-memory table (nothing is persisted).
	dir string
	// name is the file's name within dir.
	name string

	// mu guards cfg and serializes the rewrite of the file. See the type doc for why it
	// covers the write and not just the field update.
	mu  sync.Mutex
	cfg T
}

// newConfigFile returns the store for dir/name, holding cfg as the current configuration.
// A caller that has just read the file back passes what it read; one creating a table
// passes what it is creating it with.
func newConfigFile[T any](dir, name string, cfg T) *configFile[T] {
	return &configFile[T]{dir: dir, name: name, cfg: cfg}
}

// path is the file's location, or "" for an in-memory table.
func (f *configFile[T]) path() string {
	if f.dir == "" {
		return ""
	}
	return filepath.Join(f.dir, f.name)
}

// with runs fn against the held configuration under the lock, and persists nothing. It is
// for reading a consistent copy out (a slice read without it can be appended to underneath
// the reader), and for the callers that update one part of the config and persist it in a
// later step.
func (f *configFile[T]) with(fn func(cfg *T)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.cfg)
}

// update applies fn to the held configuration and writes the result, as one step under the
// lock. fn is where a caller folds live state in, so that the read and the write cannot be
// split by another saver; it should do no file I/O and must not call back into this store.
func (f *configFile[T]) update(fn func(cfg *T)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.cfg)
	return f.writeLocked()
}

// save writes the held configuration unchanged.
func (f *configFile[T]) save() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writeLocked()
}

// writeLocked marshals and atomically rewrites the file, with f.mu held. A no-op for an
// in-memory table.
func (f *configFile[T]) writeLocked() error {
	path := f.path()
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(f.cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, data)
}

// readConfigFile reads dir/name into a T, before any table exists to hold it -- the archive
// reads its config to decide whether it is opening or creating, and a mutable table reads
// its time-travel setting before the collection is opened.
//
// exists reports whether the file was there at all, which is the archive's create/open
// signal and is true even for a file that will not parse. ok reports that it also
// unmarshalled, and cfg is meaningful only then. An in-memory table (dir == "") has
// neither.
func readConfigFile[T any](dir, name string) (cfg T, exists, ok bool) {
	if dir == "" {
		return cfg, false, false
	}
	data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // G304 - path is table-internal
	if err != nil {
		return cfg, false, false
	}
	if json.Unmarshal(data, &cfg) != nil {
		var zero T
		return zero, true, false
	}
	return cfg, true, true
}

// writeAtomic writes data to path via a temp file and rename, so a reader never observes
// a partial write.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // G306 - catalog metadata, not secret
		return err
	}
	return os.Rename(tmp, path)
}
