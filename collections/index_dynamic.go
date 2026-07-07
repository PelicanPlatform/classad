package collections

import "strings"

// Dynamic index add/drop (auto-detect steps 2 & 3). The configured index set lives
// behind an atomic pointer (Collection.spec); AddIndex and DropIndex publish a new
// spec with a bumped generation. Neither touches segment indexes directly: they are
// derived, per-segment state, reconciled by Reindex, which rebuilds any segment
// whose index was built under an older generation. So a fresh add is correct
// immediately (queries on the new attribute full-scan until reindexed) and becomes
// fast once Reindex backfills it; a drop stops the attribute being consulted at
// once and reclaims its postings at the next Reindex. The caller keeps full control
// of reindex cadence — the mechanism never blocks writes or compaction.

// AddIndex adds categorical (string equality / membership) and value (numeric
// equality + range) indexes at runtime and returns whether the configuration
// changed. Names already indexed are ignored; a name given as both categorical and
// value, or already indexed as the other kind, is placed as categorical (equality
// on strings is the common case and categorical also serves it). The new indexes
// take effect for existing segments on the next Reindex; new segments pick them up
// when they are first indexed.
func (c *Collection) AddIndex(categorical, value []string) bool {
	for {
		cur := c.spec.Load()
		next := cur.clone()
		intern := func(name string) uint32 {
			if next.inline {
				return next.inlineID(name)
			}
			return c.intern.Intern(name)
		}
		for _, name := range categorical {
			id := intern(name)
			removeID(&next.valIDs, next.val, id) // categorical wins if it was a value index
			addID(&next.catIDs, next.cat, id)
		}
		for _, name := range value {
			id := intern(name)
			if _, isCat := next.cat[id]; isCat {
				continue // already categorical; do not double-index
			}
			addID(&next.valIDs, next.val, id)
		}
		if next.equalIDs(cur) {
			return false
		}
		next.gen = cur.gen + 1
		if c.spec.CompareAndSwap(cur, next) {
			return true
		}
	}
}

// DropIndex removes the given attributes from the configured indexes (categorical
// or value, whichever they were) and returns whether the configuration changed. The
// attributes stop being used by queries immediately; their postings are reclaimed
// from existing segments at the next Reindex. Names that were never indexed are
// ignored.
func (c *Collection) DropIndex(names ...string) bool {
	for {
		cur := c.spec.Load()
		next := cur.clone()
		for _, name := range names {
			var id uint32
			var ok bool
			if next.inline {
				id, ok = next.nameToID[strings.ToLower(name)]
			} else {
				id, ok = c.intern.LookupID(name)
			}
			if !ok {
				continue // never indexed
			}
			removeID(&next.catIDs, next.cat, id)
			removeID(&next.valIDs, next.val, id)
		}
		if next.equalIDs(cur) {
			return false
		}
		next.gen = cur.gen + 1
		if c.spec.CompareAndSwap(cur, next) {
			return true
		}
	}
}

// IndexedAttrs returns the currently-indexed attribute names, split by kind, in the
// collection's canonical (interned) casing.
func (c *Collection) IndexedAttrs() (categorical, value []string) {
	spec := c.spec.Load()
	name := func(id uint32) (string, bool) {
		if spec.inline {
			n, ok := spec.names[id]
			return n, ok
		}
		return c.intern.Name(id)
	}
	for _, id := range spec.catIDs {
		if n, ok := name(id); ok {
			categorical = append(categorical, n)
		}
	}
	for _, id := range spec.valIDs {
		if n, ok := name(id); ok {
			value = append(value, n)
		}
	}
	return categorical, value
}

// addID appends id to ids/set if not already present.
func addID(ids *[]uint32, set map[uint32]struct{}, id uint32) {
	if _, dup := set[id]; dup {
		return
	}
	set[id] = struct{}{}
	*ids = append(*ids, id)
}

// removeID drops id from ids/set if present.
func removeID(ids *[]uint32, set map[uint32]struct{}, id uint32) {
	if _, ok := set[id]; !ok {
		return
	}
	delete(set, id)
	out := (*ids)[:0]
	for _, x := range *ids {
		if x != id {
			out = append(out, x)
		}
	}
	*ids = out
}
