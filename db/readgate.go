package db

import "iter"

// AcquireRead registers the caller as an in-flight reader of the table and reports whether
// it may read. False means the table has been closed or dropped. Every true answer MUST be
// balanced by ReleaseRead.
//
// Holding it is what makes DropTable safe for a reader that is between segment pins -- see
// collections.Collection.AcquireRead. The scan methods below take it for the lifetime of
// the sequence they return, so an ordinary caller never needs it; it is exported for a
// caller that reads a table through several calls and needs the table to stay mapped across
// them.
func (db *DB) AcquireRead() bool { return db.c.AcquireRead() }

// ReleaseRead balances an AcquireRead that returned true. The last reader out of a dropped
// table is the one that unmaps it, so call it with no lock held.
func (db *DB) ReleaseRead() { db.c.ReleaseRead() }

// dropClose releases the table's mappings for a DROP: the on-disk files are about to be
// removed, so nothing is flushed or persisted. A reader still inside keeps the mappings
// alive and performs the unmap itself on the way out (collections.Collection.Discard).
func (db *DB) dropClose() error { return db.c.Discard() }

// gated wraps a scan sequence so the table cannot be unmapped while it is being consumed.
// The gate is taken when iteration STARTS, not when the method returns the sequence, and
// released when the body returns -- which covers the consumer stopping part way (range-over-
// func turns a break into a false from yield; iter.Pull's stop unwinds the body), an error
// return inside, and a panic.
//
// A table dropped before iteration begins yields nothing: the rows are gone, and the
// sequence has no way to report an error at this point. A drop DURING iteration does not
// truncate the scan -- the gate holds the mappings until it finishes.
//
// The one thing it cannot survive is a consumer that abandons an iter.Pull without calling
// stop, which leaks the pull goroutine and with it this gate. That leaks the table's address
// space and descriptors until the process exits; it does not block anything, because a drop
// never waits on the gate.
func gated[T any](db *DB, s iter.Seq[T]) iter.Seq[T] {
	return func(yield func(T) bool) {
		if !db.c.AcquireRead() {
			return
		}
		defer db.c.ReleaseRead()
		s(yield)
	}
}

// gatedErr wraps the (sequence, error) shape most of the query methods return.
func gatedErr[T any](db *DB, s iter.Seq[T], err error) (iter.Seq[T], error) {
	if err != nil {
		return nil, err
	}
	return gated(db, s), nil
}

// AcquireRead registers the caller as an in-flight reader of the archive table; see
// DB.AcquireRead. Balance a true answer with ReleaseRead.
func (t *ArchiveTable) AcquireRead() bool { return t.a.AcquireRead() }

// ReleaseRead balances an AcquireRead that returned true.
func (t *ArchiveTable) ReleaseRead() { t.a.ReleaseRead() }

// dropClose is DB.dropClose for an archive table.
func (t *ArchiveTable) dropClose() error { return t.a.Discard() }

// gatedArch is gated for an archive table's scans.
func gatedArch[T any](t *ArchiveTable, s iter.Seq[T]) iter.Seq[T] {
	return func(yield func(T) bool) {
		if !t.a.AcquireRead() {
			return
		}
		defer t.a.ReleaseRead()
		s(yield)
	}
}
