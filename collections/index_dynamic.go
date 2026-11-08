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
//
// An attribute the collection seals at rest (EncryptedAttrs, or private, with encryption
// on) is skipped, as Open refuses it and SetEncryptedAttrs refuses to seal an indexed one:
// the index builds from the stored value, which for a sealed attribute is the ciphertext,
// so every record would be a candidate and the index would cost memory and answer nothing.
// Building it from the plaintext instead would write the secret into the sidecar on disk.
func (c *Collection) AddIndex(categorical, value []string) bool {
	return c.addIndex(categorical, value, false)
}

// addIndexAuto is AddIndex for the auto-tuner: the added indexes are marked auto, so
// the memory-budget trimmer may later remove them (it never removes human indexes).
func (c *Collection) addIndexAuto(categorical, value []string) bool {
	return c.addIndex(categorical, value, true)
}

func (c *Collection) addIndex(categorical, value []string, auto bool) bool {
	categorical, value = c.withoutSealed(categorical), c.withoutSealed(value)
	for {
		cur := c.spec.Load()
		next := cur.clone()
		// ok is false only when an id collides with a different attribute -- an inline id
		// hashing onto another name, or a path whose dotted spelling is also an attribute
		// name. That attribute is then left unindexed rather than aliased onto another's
		// postings.
		intern := func(name string) (uint32, bool) {
			if next.inline {
				return next.indexID(nil, name)
			}
			return next.indexID(c.intern, name)
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
			id, ok := intern(name)
			if !ok {
				continue
			}
			removeID(&next.valIDs, next.val, id) // categorical wins if it was a value index
			addID(&next.catIDs, next.cat, id)
			// The attribute now has an index of its own, which settles its kind; a
			// component-only declaration alongside it would be a second answer to the
			// question componentKind asks. pruneComposites below then drops any composite
			// whose component this just re-kinded.
			next.dropCompOnly(id)
			mark(id)
		}
		for _, name := range value {
			id, ok := intern(name)
			if !ok {
				continue
			}
			if _, isCat := next.cat[id]; isCat {
				continue // already categorical; do not double-index
			}
			addID(&next.valIDs, next.val, id)
			next.dropCompOnly(id) // an index of its own settles the kind (see above)
			mark(id)
		}
		next.pruneComposites() // a composite whose component changed kind no longer describes the data
		if next.equalIDs(cur) && next.equalAuto(cur) {
			return false
		}
		next.gen = next.signature()
		if c.spec.CompareAndSwap(cur, next) {
			// An append-only collection zones every value-indexed attribute (New wires
			// ZoneAttrs and ValueAttrs together), so a value index added at runtime must
			// extend the zone set too -- otherwise a range query on it would prune
			// postings but never whole segments, unlike an identical index configured at
			// creation. No-op for a mutable collection.
			c.addZoneAttrs(topLevelNames(value))
			return true
		}
	}
}

// withoutSealed returns names without the attributes this collection seals at rest (see AddIndex).
func (c *Collection) withoutSealed(names []string) []string {
	if c.sealer == nil {
		return names
	}
	out := names[:0:0]
	for _, n := range names {
		// A path is judged by its root attribute, which is the one stored and sealed (see New).
		root := n
		if comps := splitAttrPath(n); comps != nil {
			root = comps[0]
		}
		if !c.shouldEncrypt(root) {
			out = append(out, n)
		}
	}
	return out
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
			// A component-only declaration is dropped by name too: the attribute stops
			// being anything to this spec, and pruneComposites then takes the composites
			// that named it, exactly as it does for a dropped real index.
			next.dropCompOnly(id)
			// The id stops meaning anything, so it stops meaning a path too: a later AddIndex
			// on the plain attribute of that name must not be refused by a shape this spec no
			// longer indexes.
			delete(next.paths, id)
			delete(next.auto, id)
		}
		next.pruneComposites() // a composite outlives neither of its components
		if next.equalIDs(cur) {
			return false
		}
		next.gen = next.signature()
		if c.spec.CompareAndSwap(cur, next) {
			return true
		}
	}
}

// StaleIndexSegments reports sealed segments whose index was built under a superseded
// configuration and is still due a rebuild, out of the sealed segments total.
//
// "Due a rebuild" is the operative part. With Options.IndexBackfillBytes set, Reindex
// deliberately carries a configuration change only to recent segments; older ones keep the
// index they have until BackfillIndexes reaches them, which on a large archive takes many
// passes. Counting those would leave the number non-zero for that long and say nothing about
// whether anything is wrong -- so they are excluded here and reported separately by
// StaleIndexSegmentsByPolicy (and, the ones missing an attribute, by IndexBackfillPending).
//
// A persistent non-zero value from this therefore still means what it always meant: a
// reindex is failing, or never running.
func (c *Collection) StaleIndexSegments() (stale, sealed int) {
	stale, _, sealed = c.staleIndexCounts()
	return stale, sealed
}

// StaleIndexSegmentsByPolicy reports sealed segments left on a superseded index
// configuration because they fall outside the backfill horizon. Expected, not actionable:
// it is the size of the deliberate mixture. It includes segments that already cover every
// indexed attribute and differ only by postings for a dropped one, which nothing rebuilds;
// IndexBackfillPending counts the ones that are missing an attribute.
func (c *Collection) StaleIndexSegmentsByPolicy() int {
	_, byPolicy, _ := c.staleIndexCounts()
	return byPolicy
}

func (c *Collection) staleIndexCounts() (pending, byPolicy, sealed int) {
	gen := c.spec.Load().gen
	for _, sh := range c.shards {
		sh.mu.RLock()
		window := c.backfillWindow(sh)
		segs := append([]*segment(nil), sh.segs...)
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil {
				continue
			}
			mm := seg.msidx.Load()
			if mm == nil {
				continue // not sealed to a sidecar: Reindex still rebuilds it in place
			}
			sealed++
			if mm.specGen == gen {
				continue
			}
			if window != nil && !window[seg] {
				byPolicy++ // outside the horizon: left to BackfillIndexes, not to Reindex
				continue
			}
			pending++
		}
	}
	return pending, byPolicy, sealed
}

// IndexedAttrs returns the currently-indexed attribute names, split by kind, in the
// collection's canonical (interned) casing.
//
// Component-only attributes (Options.CategoricalComponentAttrs / ValueComponentAttrs) are
// NOT reported, and that is the contract callers want: they ask this to decide whether a
// constraint on an attribute can be answered from an index, and a component-only attribute
// has no index of its own to answer one with. Nothing else reports them -- they are a
// property of the composites that use them, which IndexSizes already names.
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

// MarkAutoIndexes records the named EXISTING indexes as auto-created. It changes only
// provenance: no index is added, dropped, or rebuilt, and a name that is not currently
// indexed is ignored.
//
// This is the restore side of AutoIndexNames for a store that rebuilds its whole index set
// at open (an archive does). Going through AddAutoIndex there would not work: the indexes
// already exist as human-created by then, and an auto add deliberately refuses to downgrade
// a human index. Adding them separately instead would restore provenance correctly but
// rebuild them, which for an archive means reading history back through the decompressor on
// every start.
func (c *Collection) MarkAutoIndexes(names []string) {
	if len(names) == 0 {
		return
	}
	for {
		cur := c.spec.Load()
		next := cur.clone()
		changed := false
		for _, n := range names {
			fold := strings.ToLower(n)
			var id uint32
			var ok bool
			if next.inline {
				id, ok = next.nameToID[fold]
			} else {
				id, ok = c.intern.LookupID(fold)
			}
			if !ok || (!next.catHas(id) && !next.valHas(id)) {
				continue // not indexed: nothing to attribute
			}
			if next.isAuto(id) {
				continue
			}
			if next.auto == nil {
				next.auto = make(map[uint32]struct{})
			}
			next.auto[id] = struct{}{}
			changed = true
		}
		if !changed {
			return
		}
		if c.spec.CompareAndSwap(cur, next) {
			return
		}
	}
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

// --- composite provenance ------------------------------------------------------------
//
// The composite counterparts of AddAutoIndex / MarkAutoIndexes / AutoIndexNames, and for
// the same reason: a composite the tuner created is trimmable by the memory budget and
// droppable when nothing demands its tuple, and a human-configured one is neither. Reopen
// without the mark and the tuner's own composite comes back exempt from both, permanently,
// on the strength of nothing -- which is the shape of mistake the single-attribute Auto
// field already exists to avoid.
//
// There are two restore paths here as well, for the same reason there are two there. A store
// that opens with no composite configured -- which is every store whose caller configures
// none, so the tuner's are all it has -- ADDS its auto set back, and the add carries the
// mark. A store that rebuilds its whole index set from a saved configuration at open (an
// archive does) has its composites back already, as configured ones, by the time anything
// could mark them: adding them again would be refused as a tuple already covered, and
// configuring them as auto in the first place would leave the saved configuration unable to
// say which tuples a human asked for. So that one MARKS.

// CompositeAttrs returns the attribute tuples the collection indexes together, in the
// spelling each was configured with and in tuple-identity order. The composite counterpart
// of IndexedAttrs: what a store persists so a reopen rebuilds the same composites.
func (c *Collection) CompositeAttrs() [][]string {
	spec := c.spec.Load()
	if spec == nil || len(spec.comps) == 0 {
		return nil
	}
	out := make([][]string, 0, len(spec.comps))
	for _, cs := range spec.comps {
		out = append(out, append([]string(nil), cs.names...))
	}
	return out
}

// CompositeRefusals returns the configured composite tuples this collection could not
// install, and why -- the other half of CompositeAttrs, which reports only what survived.
//
// It is fixed at construction and never changes afterwards, because it describes the
// configuration this store was opened with. A later AddIndex or DropIndex that would make a
// refused tuple resolvable does not install it: the tuple is not retried, and the entry
// here does not go away. Only reconfiguring and reopening does that, which is also the only
// action a caller should take on a non-empty result -- see CompositeRefusal.
//
// The composites the AUTO-TUNER proposes are not reported here; see addCompositesAuto.
func (c *Collection) CompositeRefusals() []CompositeRefusal {
	if len(c.compRefused) == 0 {
		return nil
	}
	return append([]CompositeRefusal(nil), c.compRefused...)
}

// AutoCompositeNames returns the tuples of the composites the auto-tuner created, a subset
// of CompositeAttrs. The composite counterpart of AutoIndexNames.
func (c *Collection) AutoCompositeNames() [][]string {
	spec := c.spec.Load()
	if spec == nil {
		return nil
	}
	var out [][]string
	for _, cs := range spec.comps {
		if cs.auto {
			out = append(out, append([]string(nil), cs.names...))
		}
	}
	return out
}

// AddAutoComposites installs composites over these tuples, marked auto, and reports whether
// any was installed. The composite counterpart of AddAutoIndex, and the restore path for a
// store whose composites are not re-derived from a saved configuration at open.
//
// Fewer tuples may be installed than were asked for, silently -- and silently on purpose,
// where a configured tuple's refusal is reported (CompositeRefusals): a tuple whose
// component is not indexed in its own right is refused (newCompositeSpecs), as is one some
// composite already covers in any order. Both are the right answer for a restore -- the first says the
// component index did not come back, which leaves the composite nothing to fall back to for
// the conjunctions it cannot answer; the second says the tuple is already configured, and a
// configured composite is not the tuner's to claim.
func (c *Collection) AddAutoComposites(tuples [][]string) bool {
	return len(c.addCompositesAuto(tuples)) > 0
}

// MarkAutoComposites records the named EXISTING composites as auto-created. It changes only
// provenance: no composite is added, dropped or rebuilt, the index generation does not move,
// and a tuple no current composite covers is ignored.
//
// Tuples are matched as SETS of folded component names, not as ordered keys. Order is part
// of a composite's identity but not of what it answers, and two orders of one set cannot
// both be configured (coversTupleIDs refuses the second), so a set names at most one
// composite and matching this way survives a saved tuple being written back in a different
// order than the live composite carries.
func (c *Collection) MarkAutoComposites(tuples [][]string) {
	want := make(map[string]struct{}, len(tuples))
	for _, t := range tuples {
		if len(t) < 2 {
			continue
		}
		want[strings.Join(foldedSorted(t), tupleKeySep)] = struct{}{}
	}
	if len(want) == 0 {
		return
	}
	for {
		cur := c.spec.Load()
		if cur == nil {
			return
		}
		next := cur.clone()
		changed := false
		for i := range next.comps {
			if next.comps[i].auto {
				continue
			}
			if _, ok := want[strings.Join(foldedSorted(next.comps[i].names), tupleKeySep)]; !ok {
				continue
			}
			next.comps[i].auto = true
			changed = true
		}
		if !changed {
			return
		}
		// No signature bump: provenance is not part of what a sidecar was built under, and
		// moving the generation here would make every sealed segment stale at every open --
		// an archive-wide reindex to record who asked for an index.
		if c.spec.CompareAndSwap(cur, next) {
			return
		}
	}
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
