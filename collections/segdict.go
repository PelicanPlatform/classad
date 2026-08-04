package collections

import (
	"sort"
	"strings"
)

// segDict is a sealed segment's per-segment attribute-name dictionary in its serialized,
// mmap-probed form. A segment written interned stores its attribute names once, in a dict
// carried inside the segment container; records then reference names by a small segment-LOCAL
// id (its position in the dict). This file is the dict's build + zero-copy probe layer.
//
// The whole dict is probed directly over the mapped bytes -- no Go map or slice is
// materialized per segment -- so loading many sealed dicts does not reintroduce the heap that
// interning removes. name->id is a minimal perfect hash (mph.go) with an authoritative
// sorted-name fallback; id->name is an offset table into the name blob. All lookups are
// case-insensitive (ClassAd attribute names fold), matching InternTable.
//
// Layout (every offset is relative to the dict's start `base`):
//
//	u32 count
//	u32 idOffTableOff  -- id -> offset (into the name blob) table
//	u32 nameBlobOff    -- start of the name blob
//	u32 slotIdOff      -- MPH slot -> id table (nAssigned entries)
//	u32 sortedIdOff    -- ids sorted by folded name (authoritative binary-search fallback)
//	u32 mphOff         -- name->slot MPH (appendMPH); a non-authoritative accelerator
//	[count]u32   idToNameOff    (each relative to nameBlobOff)
//	[nAssigned]u32 slotToId
//	[count]u32   sortedIds
//	name blob: [count] { u32 len; name bytes }   -- canonical (first-seen) casing
//	MPH bytes (appendMPH)
const segDictHeaderBytes = 6 * 4

// appendSegDict serializes the dictionary for names (names[id] is the canonical casing of
// local id `id`; ids are 0..len-1 in seal/first-seen order). Folded names must be distinct,
// which they are for an InternTable's Names() (one entry per fold-equal group).
func appendSegDict(dst []byte, names []string) []byte {
	count := uint32(len(names))
	folded := make([]string, count)
	for i, n := range names {
		folded[i] = strings.ToLower(n)
	}
	m, slots := buildMPH(folded) // slots[id] = MPH slot, or -1 if unassigned (fallback resolves)

	slotToId := make([]uint32, m.nAssigned)
	for id, s := range slots {
		if s >= 0 {
			slotToId[s] = uint32(id)
		}
	}
	sortedIds := make([]uint32, count)
	for i := range sortedIds {
		sortedIds[i] = uint32(i)
	}
	sort.Slice(sortedIds, func(a, b int) bool { return folded[sortedIds[a]] < folded[sortedIds[b]] })

	var nameBlob []byte
	idToNameOff := make([]uint32, count)
	for id, n := range names {
		idToNameOff[id] = uint32(len(nameBlob))
		nameBlob = appendU32(nameBlob, uint32(len(n)))
		nameBlob = append(nameBlob, n...)
	}

	idOffTableOff := uint32(segDictHeaderBytes)
	slotIdOff := idOffTableOff + count*4
	sortedIdOff := slotIdOff + uint32(len(slotToId))*4
	nameBlobOff := sortedIdOff + count*4
	mphOff := nameBlobOff + uint32(len(nameBlob))

	dst = appendU32(dst, count)
	dst = appendU32(dst, idOffTableOff)
	dst = appendU32(dst, nameBlobOff)
	dst = appendU32(dst, slotIdOff)
	dst = appendU32(dst, sortedIdOff)
	dst = appendU32(dst, mphOff)
	for _, o := range idToNameOff {
		dst = appendU32(dst, o)
	}
	for _, v := range slotToId {
		dst = appendU32(dst, v)
	}
	for _, v := range sortedIds {
		dst = appendU32(dst, v)
	}
	dst = append(dst, nameBlob...)
	dst = appendMPH(dst, m)
	return dst
}

// segDictHandle binds a serialized dict to the bytes it lives in (a segment's mmap arena) at
// offset base, so reads probe it zero-copy. It is immutable once published; a segment holds
// one via an atomic pointer (nil => the segment is inline-encoded). All methods are safe for
// concurrent readers.
type segDictHandle struct {
	data []byte // the segment arena (or any buffer) the dict is embedded in
	base uint32 // offset of the dict's first byte within data
}

func (h *segDictHandle) lookup(name string) (uint32, bool) { return segDictLookup(h.data, h.base, name) }
func (h *segDictHandle) name(id uint32) []byte             { return segDictName(h.data, h.base, id) }
func (h *segDictHandle) count() uint32                     { return segDictCount(h.data, h.base) }

// resolve is the id->name function form for wire.DecodeResolve, so an interned record decodes
// straight over the mmap with no in-memory table.
func (h *segDictHandle) resolve(id uint32) (string, bool) {
	if b := h.name(id); b != nil {
		return string(b), true
	}
	return "", false
}

// segDictCount returns the number of names in the dict at base.
func segDictCount(data []byte, base uint32) uint32 { return le32(data, base) }

// segDictName returns the canonical name bytes for local id, aliasing data (an mmap). Returns
// nil if id is out of range. Two zero-copy reads (offset table, then the length-prefixed name).
func segDictName(data []byte, base, id uint32) []byte {
	if id >= le32(data, base) {
		return nil
	}
	idOffTableOff := le32(data, base+4)
	nameBlobOff := le32(data, base+8)
	nameOff := le32(data, base+idOffTableOff+id*4)
	p := base + nameBlobOff + nameOff
	n := le32(data, p)
	return data[p+4 : p+4+n]
}

// segDictLookup resolves a name (case-insensitive) to its local id. The MPH gives an O(1)
// candidate that is verified against the stored name; on any miss (unassigned member, verify
// mismatch, or non-member) it falls back to an authoritative binary search over the
// folded-name-sorted index, so a build/probe discrepancy can only cost a redundant search,
// never a wrong answer. ok=false means the name is not in this segment's dict.
func segDictLookup(data []byte, base uint32, name string) (id uint32, ok bool) {
	count := le32(data, base)
	if count == 0 {
		return 0, false
	}
	slotIdOff := le32(data, base+12)
	mphOff := le32(data, base+20)
	folded := strings.ToLower(name)
	if slot, hit := mphLookupBytes(data, base+mphOff, folded); hit {
		cid := le32(data, base+slotIdOff+slot*4)
		if strings.EqualFold(string(segDictName(data, base, cid)), name) {
			return cid, true
		}
	}
	return segDictSearch(data, base, folded)
}

// segDictSearch is the authoritative fallback: binary search over the folded-name-sorted id
// index. `folded` must already be lower-cased.
func segDictSearch(data []byte, base uint32, folded string) (uint32, bool) {
	count := le32(data, base)
	sortedIdOff := le32(data, base+16)
	lo, hi := uint32(0), count
	for lo < hi {
		mid := (lo + hi) / 2
		id := le32(data, base+sortedIdOff+mid*4)
		nm := strings.ToLower(string(segDictName(data, base, id)))
		switch {
		case nm == folded:
			return id, true
		case nm < folded:
			lo = mid + 1
		default:
			hi = mid
		}
	}
	return 0, false
}
