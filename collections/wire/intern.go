// Package wire defines a compact TLV binary form of a ClassAd with attribute-key
// interning and a hot-attribute header, plus the encoder/decoder that convert to
// and from the fully-public ast.ClassAd representation.
//
// The package depends only on the ast package (and the standard library): Encode
// takes an *ast.ClassAd and Decode returns one, so wire stays decoupled from the
// higher-level classad.ClassAd wrapper. Bridging classad.ClassAd <-> ast.ClassAd
// lives in the store layer.
package wire

import (
	"strings"
	"sync"
	"sync/atomic"
)

// InternTable maps attribute (and function) names to small integer ids so the
// wire form can reference names by uvarint id instead of repeating bytes.
//
// Names are case-insensitive but case-preserving, matching ClassAd attribute
// semantics: "Owner", "owner" and "OWNER" all intern to the same id, and the id
// resolves back to the first-seen casing. The table is append-only: an id is
// stable for the life of the table (a collection compaction may build a fresh
// table with a remap, but never mutates ids in place).
type InternTable struct {
	mu sync.RWMutex
	// byExact maps an exact (case-preserving) name to its id. It is a pure cache
	// over byFold that lets Intern/LookupID skip the strings.ToLower fold on the
	// hot path: during ingest the same attribute names recur in the same casing
	// across virtually every ad, so after the first ad this hits ~always. Several
	// exact names can map to one id (e.g. "Owner", "owner", "OWNER").
	byExact map[string]uint32
	byFold  map[string]uint32 // ToLower(name) -> id
	// canon (id -> entry: first-seen casing + private flag) is published as an
	// immutable, append-only slice behind an atomic pointer so Name/IsPrivate
	// resolve an id with no lock. Name is called for every attribute of every ad
	// during serialization; an RWMutex.RLock there -- though logically a read --
	// serializes on its atomic reader counter across cores, which alone was ~23%
	// of CPU on concurrent large-ad scans. Writers (Intern, holding mu) copy-append
	// and atomically publish; existing entries are never mutated, so a lock-free
	// reader always sees a valid snapshot. The private flag lives INSIDE the entry
	// (not a parallel slice under its own atomic) so a single load observes a
	// consistent name+flag pair.
	canon atomic.Pointer[[]internEntry]
	// privacy classifies a name as private (secret) ONCE, when its id is first
	// allocated; per-attribute consumers then read the precomputed flag by id.
	// nil means nothing is private. Set at construction only -- flags computed for
	// already-interned ids are never revisited.
	privacy func(string) bool
}

// internEntry is one id's published state: its canonical (first-seen) casing and
// whether the name is private (per the table's privacy predicate).
type internEntry struct {
	name    string
	private bool
}

// NewInternTable returns an empty table. Id 0 is a valid id (the first name
// interned); callers that need a sentinel should track presence separately.
func NewInternTable() *InternTable {
	return NewInternTableWithPrivacy(nil)
}

// NewInternTableWithPrivacy returns an empty table whose entries are flagged
// private when privacy(name) is true, evaluated once per unique name at intern
// time. Redacting iterators (Ad.ForEachNamedRedact) skip flagged ids, so a
// serialization path can strip secrets in O(1) per attribute instead of
// re-classifying every attribute name of every ad. A nil privacy flags nothing.
func NewInternTableWithPrivacy(privacy func(string) bool) *InternTable {
	t := &InternTable{
		byExact: make(map[string]uint32),
		byFold:  make(map[string]uint32),
		privacy: privacy,
	}
	empty := []internEntry{}
	t.canon.Store(&empty)
	return t
}

// Intern returns the id for name, allocating a new id (and recording name's
// casing) the first time a fold-equal name is seen.
func (t *InternTable) Intern(name string) uint32 {
	// Fast path: this exact name (same casing) has been seen before. Avoids the
	// strings.ToLower allocation, which dominates bulk ingest.
	t.mu.RLock()
	id, ok := t.byExact[name]
	t.mu.RUnlock()
	if ok {
		return id
	}

	// Slow path: fold and look up or allocate.
	fold := strings.ToLower(name)
	t.mu.Lock()
	defer t.mu.Unlock()
	// Re-check byExact in case a concurrent Intern added it since the RUnlock.
	if id, ok := t.byExact[name]; ok {
		return id
	}
	id, ok = t.byFold[fold]
	if !ok {
		// First time any casing of this name is seen: allocate an id and record
		// this casing as canonical (classifying it as private exactly once).
		// Copy-append (cap==len forces a fresh backing array) so lock-free readers
		// holding the old snapshot are unaffected, then publish atomically. New
		// names are rare after warmup, so the copy is cheap.
		old := *t.canon.Load()
		id = uint32(len(old))
		next := append(old[:len(old):len(old)], internEntry{
			name:    name,
			private: t.privacy != nil && t.privacy(name),
		})
		t.canon.Store(&next)
		t.byFold[fold] = id
	}
	t.byExact[name] = id
	return id
}

// LookupID returns the id for name if it has already been interned, without
// allocating a new one (a read-only counterpart to Intern). Used by the query
// fast path to resolve an attribute name to its id without polluting the table
// with names that are not attributes.
func (t *InternTable) LookupID(name string) (uint32, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if id, ok := t.byExact[name]; ok { // exact-casing hit: no fold needed
		return id, true
	}
	id, ok := t.byFold[strings.ToLower(name)]
	return id, ok
}

// Name returns the canonical (first-seen) casing for id, or ("", false) if id
// was never allocated.
func (t *InternTable) Name(id uint32) (string, bool) {
	canon := *t.canon.Load() // lock-free: canon entries are immutable once published
	if int(id) >= len(canon) {
		return "", false
	}
	return canon[id].name, true
}

// IsPrivate reports whether id's name was classified private by the table's
// privacy predicate when it was interned. Lock-free; an unallocated id is not
// private. O(1) -- this is the point of precomputing the flag: a redacting
// serializer checks one bool per attribute instead of re-classifying its name.
func (t *InternTable) IsPrivate(id uint32) bool {
	canon := *t.canon.Load()
	return int(id) < len(canon) && canon[id].private
}

// isPrivateName classifies a (non-interned) name with the table's privacy
// predicate -- the redacting iterator uses it for inline-encoded ads, whose
// attribute names are stored in the ad body rather than as interned ids.
func (t *InternTable) isPrivateName(name string) bool {
	return t.privacy != nil && t.privacy(name)
}

// Len returns the number of interned names (== the next id to be allocated).
func (t *InternTable) Len() int {
	return len(*t.canon.Load())
}

// snapshotNames returns a copy of the id->name slice, for embedding a standalone
// intern table into a self-contained ad.
func (t *InternTable) snapshotNames() []string {
	canon := *t.canon.Load()
	out := make([]string, len(canon))
	for i, e := range canon {
		out[i] = e.name
	}
	return out
}
