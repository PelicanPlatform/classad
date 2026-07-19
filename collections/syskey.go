package collections

// System keys are a reserved internal keyspace for durable bookkeeping records
// (e.g. idempotency markers, TTL reapers) that live in the same table as data but
// are hidden from every client-facing scan/query/iteration. They are reserved by a
// leading NUL byte (0x00): a normal ClassAd key -- a HashKey or a user key -- is
// printable text and never begins with NUL, so the two keyspaces cannot collide.
//
// A system-keyed record is stored, updated, committed, compacted, and looked up by
// explicit key exactly like any other record (no change to writes, no change to
// Lookup); only the client-facing enumeration paths filter it out. A dedicated
// system-only scan (Collection.ForEachSystemAd) enumerates just these records for a
// reaper.

// IsSystemKey reports whether key is a reserved internal system key (begins with a
// NUL byte). Exported so callers (and the db/dbrpc layers) can classify a key
// without duplicating the sentinel.
func IsSystemKey(key string) bool { return len(key) > 0 && key[0] == 0 }

// SystemKey builds a system key from a name by prefixing the reserved NUL byte. The
// name is otherwise opaque; callers namespace it however they like.
func SystemKey(name string) string { return "\x00" + name }

// isSystemKeyBytes is IsSystemKey over the raw record key bytes the scan primitives
// expose (a view into a frozen segment window), avoiding a per-record string copy.
func isSystemKeyBytes(key []byte) bool { return len(key) > 0 && key[0] == 0 }
