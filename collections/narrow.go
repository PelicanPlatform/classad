package collections

import "github.com/PelicanPlatform/classad/collections/wire"

// PROJECTING BEFORE CONVERTING, NOT AFTER.
//
// A projected read wants a handful of attributes, but the record it starts from is keyed by
// SEGMENT-LOCAL dictionary ids, and the subset builder needs inline names. The conversion between
// those was doing the obvious thing in the wrong order: decode the whole ad to an AST, re-encode all
// of it with inline names, then walk the result and throw almost all of it away.
//
// Measured on 1500 real OSPool machine ads projecting two attributes, that conversion was 2.28s of
// the 3.43s the scan spent in this package -- two thirds of the work, to produce 550 attributes
// nobody asked for. Narrowing first, in id space, leaves the conversion with only the attributes the
// caller wants.
//
// The narrowing has to be a SUPERSET of what the projection keeps, never a subset: the authoritative
// keep test still runs afterwards, so admitting an extra attribute costs a few bytes of scratch,
// while dropping one silently returns an ad missing a field the caller asked for.

// narrowScratch holds the buffers a narrowing pass reuses across records. Per-record allocation is
// what this optimization is trying to avoid, so it would be self-defeating to allocate two slices
// per record to achieve it.
type narrowScratch struct {
	entries []byte
	out     []byte
}

// narrowAd rebuilds w keeping only the attributes keep accepts, preserving the header (which carries
// MyType/TargetType) and the record's own key encoding.
//
// Reports false if w is not a well-formed ad, in which case the caller must fall back rather than
// treat the result as an empty ad.
func narrowAd(sc *narrowScratch, w []byte, keep func(id uint32, name string) bool) ([]byte, bool) {
	hdr, _, _, inline, ok := wire.Ad(w).SplitBody()
	if !ok {
		return nil, false
	}
	sc.entries = sc.entries[:0]
	n := 0
	wire.Ad(w).ForEachRaw(func(id uint32, name string, node []byte) bool {
		if !keep(id, name) {
			return true
		}
		sc.entries = wire.AppendKey(sc.entries, inline, id, name)
		sc.entries = append(sc.entries, node...)
		n++
		return true
	})
	sc.out = wire.BuildAd(sc.out[:0], hdr, n, sc.entries)
	return sc.out, true
}

// localWanted returns the projection's names resolved to seg-local dictionary ids, cached for the
// dict it was resolved against.
//
// Cached per dict rather than per record because resolving is a dictionary probe per projected name,
// and a scan crosses few segments and many records. The dict pointer identifies the segment: a
// different segment means a different id space, and reusing the old set there would keep the wrong
// attributes -- so the cache is keyed by identity, not merely presence.
//
// Resolution is case-insensitive (segDictLookup folds), so a projection naming an attribute in a
// different case than the record stores it still resolves.
func (sel *wireSubsetSelector) localWanted(dict *segDictHandle) map[uint32]struct{} {
	if sel.projDict == dict && sel.localIDs != nil {
		return sel.localIDs
	}
	m := make(map[uint32]struct{}, len(sel.names))
	for _, n := range sel.names {
		if id, ok := dict.lookup(n); ok {
			m[id] = struct{}{}
		}
	}
	sel.projDict, sel.localIDs = dict, m
	return m
}
