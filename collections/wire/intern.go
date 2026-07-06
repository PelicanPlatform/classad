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
	canon   []string          // id -> first-seen casing
}

// NewInternTable returns an empty table. Id 0 is a valid id (the first name
// interned); callers that need a sentinel should track presence separately.
func NewInternTable() *InternTable {
	return &InternTable{
		byExact: make(map[string]uint32),
		byFold:  make(map[string]uint32),
	}
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
		// this casing as canonical.
		id = uint32(len(t.canon))
		t.canon = append(t.canon, name)
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
	t.mu.RLock()
	defer t.mu.RUnlock()
	if int(id) >= len(t.canon) {
		return "", false
	}
	return t.canon[id], true
}

// Len returns the number of interned names (== the next id to be allocated).
func (t *InternTable) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.canon)
}

// snapshotNames returns a copy of the id->name slice, for embedding a standalone
// intern table into a self-contained ad.
func (t *InternTable) snapshotNames() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, len(t.canon))
	copy(out, t.canon)
	return out
}
