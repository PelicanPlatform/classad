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

// wireNarrower is ONE GOROUTINE's narrowing state for a wire-subset scan: the reusable buffers plus
// the projection resolved into the id space of the segment it is currently reading.
//
// Everything here is mutable per record, so it cannot live on the shared wireSubsetSelector: the
// parallel wire scan runs W workers over one selector, and a cache hanging off it would be a data
// race (and would thrash besides, since workers steal different segments). Each worker holds its
// own, exactly as it holds its own decompression buffer.
type wireNarrower struct {
	scratch narrowScratch
	// dict/localIDs cache the projection resolved into one segment's local id space. Cached per
	// dict rather than per record because resolving is a dictionary probe per projected name, and
	// a worker reads many records per segment. The dict pointer identifies the segment: a
	// different segment means a different id space, and reusing the old set there would keep the
	// wrong attributes -- so the cache is keyed by identity, not merely presence.
	dict     *segDictHandle
	localIDs map[uint32]struct{}
}

// localWanted returns sel's names -- plus the type fields -- resolved to dict's segment-local ids.
//
// The type fields are in the set because sel.keep admits them whatever the projection says, so that
// a shipped row stays typed. Leaving them out of the narrowing made the row untyped instead: they
// were dropped before the keep test ever saw them, and an interned store's projected rows came back
// with no MyType at all. The narrowing must be a superset of what keep accepts, never a subset.
//
// Resolution is case-insensitive (segDictLookup folds), so a projection naming an attribute in a
// different case than the record stores it still resolves.
func (nw *wireNarrower) localWanted(sel *wireSubsetSelector, dict *segDictHandle) map[uint32]struct{} {
	if nw.dict == dict && nw.localIDs != nil {
		return nw.localIDs
	}
	m := make(map[uint32]struct{}, len(sel.names)+len(rawTypeFieldNames))
	for _, n := range sel.names {
		if id, ok := dict.lookup(n); ok {
			m[id] = struct{}{}
		}
	}
	for _, n := range rawTypeFieldNames {
		if id, ok := dict.lookup(n); ok {
			m[id] = struct{}{}
		}
	}
	nw.dict, nw.localIDs = dict, m
	return m
}

// narrowProjected narrows an INTERNED segment's record to (a superset of) the projected attributes,
// before wireToInline converts it. wireToInline decodes the whole ad and re-encodes every attribute
// with inline names, so handing it the full record means paying for every attribute the ad has to
// emit the handful the caller asked for.
//
// Returns w unchanged when there is nothing to narrow -- a whole-ad read, an already-inline record
// (dict == nil, nothing to convert), or an encrypted collection, whose values wireToInline opens
// during the conversion and this shortcut does not.
//
// Both the serial and the parallel wire scan go through here, deliberately: the narrowing lived in
// the serial emitter only, so every scan big enough to fan out -- which is every scan that matters
// -- converted whole records anyway and the optimization never reached a real read. Measured over
// 100k ~45-attribute job ads with a five-attribute projection on an interned store, giving the
// parallel scan the same narrowing took it from 3.0 to 1.4 us/ad, and the conversion from 29% of
// the scan's CPU to 5%.
func (c *Collection) narrowProjected(nw *wireNarrower, sel *wireSubsetSelector, dict *segDictHandle, w []byte) []byte {
	if sel.wantAll || dict == nil || c.sealer != nil {
		return w
	}
	want := nw.localWanted(sel, dict)
	out, ok := narrowAd(&nw.scratch, w, func(id uint32, name string) bool {
		if name != "" {
			return true // inline entry in an interned record: keep, the real test is sel.keep below
		}
		_, in := want[id]
		return in
	})
	if !ok {
		return w // not a well-formed ad: hand the original on and let the subset builder refuse it
	}
	return out
}
