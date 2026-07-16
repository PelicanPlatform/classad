package collections

import (
	"sort"
	"strings"
)

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
	return c.addIndex(categorical, value, false)
}

// addIndexAuto is AddIndex for the auto-tuner: the added indexes are marked auto, so
// the memory-budget trimmer may later remove them (it never removes human indexes).
func (c *Collection) addIndexAuto(categorical, value []string) bool {
	return c.addIndex(categorical, value, true)
}

func (c *Collection) addIndex(categorical, value []string, auto bool) bool {
	for {
		cur := c.spec.Load()
		next := cur.clone()
		intern := func(name string) uint32 {
			if next.inline {
				return next.inlineID(name)
			}
			return c.intern.Intern(name)
		}
		// mark records id's provenance, judged against the PRE-EXISTING spec (cur): an
		// auto add marks it auto unless it was already a human index (no downgrade); a
		// human add always clears the auto mark (human ownership wins).
		mark := func(id uint32) {
			wasHuman := (cur.catHas(id) || cur.valHas(id)) && !cur.isAuto(id)
			if auto {
				if wasHuman {
					return
				}
				if next.auto == nil {
					next.auto = map[uint32]struct{}{}
				}
				next.auto[id] = struct{}{}
			} else if next.auto != nil {
				delete(next.auto, id)
			}
		}
		for _, name := range categorical {
			id := intern(name)
			removeID(&next.valIDs, next.val, id) // categorical wins if it was a value index
			addID(&next.catIDs, next.cat, id)
			mark(id)
		}
		for _, name := range value {
			id := intern(name)
			if _, isCat := next.cat[id]; isCat {
				continue // already categorical; do not double-index
			}
			addID(&next.valIDs, next.val, id)
			mark(id)
		}
		if next.equalIDs(cur) && next.equalAuto(cur) {
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
			delete(next.auto, id)
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

// AddAutoIndex adds indexes marked as auto-created (provenance auto), like AddIndex but
// eligible for automatic trimming by AutoTune's memory budget. Used by the persistence
// layer to restore auto provenance on restart.
func (c *Collection) AddAutoIndex(categorical, value []string) bool {
	return c.addIndexAuto(categorical, value)
}

// AutoIndexNames returns the names of auto-created indexes (provenance auto), so the
// human/auto distinction can be persisted and survive a restart.
func (c *Collection) AutoIndexNames() []string {
	spec := c.spec.Load()
	if spec == nil || len(spec.auto) == 0 {
		return nil
	}
	name := func(id uint32) (string, bool) {
		if spec.inline {
			n, ok := spec.names[id]
			return n, ok
		}
		return c.intern.Name(id)
	}
	var out []string
	for id := range spec.auto {
		if n, ok := name(id); ok {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
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
