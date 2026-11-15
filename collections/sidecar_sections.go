package collections

import (
	"encoding/binary"
	"sort"
)

// The v7 sidecar container: a self-describing section table.
//
// Versions v1..v6 each added one section by minting a new magic and extending a fixed
// trailer, so every section cost a container version, and a build that did not recognise
// the newest magic fell back to walking the whole segment. Six versions in, that is the
// wrong shape: the next section should cost nothing.
//
// v7 records (id, length) per section, so a reader slices by the directory rather than by
// a layout it has to know in advance. A section id this build does not understand is kept
// as bytes and written back out unchanged, which is what makes a read-modify-write safe:
// the rewriting paths (reflag the key index, replace the columnar blob, record damage) no
// longer have to enumerate every section to avoid dropping one.
//
//	[section bodies, in directory order]
//	[directory: nsec * (id u16 | pad u16 | len u32)]
//	[segUsed u64 | dictRec u32 | colRec u32 | nsec u32 | magic("SCN7") u32]
//
// The attribute section keeps offset 0 when it is present, as in v1..v6: its roaring
// bitmaps are offset-sensitive and the shared ARCX parser expects them there.
//
// Reading stays backward compatible -- parseSidecar falls back through v6..v1 -- but
// WRITING is one-way. A sidecar written by this build is not parseable by one that
// predates v7, which falls back to walking the segment: correct, and slower until the
// sidecar is rebuilt. That is the same cost every previous version bump imposed, and the
// last time it will be paid.
const (
	sidecarContainerMagicV7 = 0x53434e37 // "SCN7"
	// 2 (id) + 2 (pad, for 4-byte alignment of the length) + 4 (len).
	sidecarDirEntryLen = 8
	// segUsed u64 + dictRec u32 + colRec u32 + nsec u32 + magic u32.
	sidecarTrailerLenV7 = 24
	// sidecarMaxSections bounds what a trailer may claim before the directory is even
	// read, so a corrupt nsec cannot make the reader address far outside the mapping.
	// The container has six section kinds; the headroom is for ids added later.
	sidecarMaxSections = 64
)

// Section ids. Never renumber one: the id is on disk.
const (
	secAttr    uint16 = 1
	secKey     uint16 = 2
	secCol     uint16 = 3
	secZone    uint16 = 4
	secDamage  uint16 = 5
	secProfile uint16 = 6
)

// sidecarUnknownSection is a section whose id this build does not know. It is carried
// through a read-modify-write unchanged rather than dropped -- a newer build's section
// must survive an older build rewriting the container around it.
type sidecarUnknownSection struct {
	id   uint16
	body []byte
}

// sidecarSections is a parsed container. Every section is present as a field (or in
// unknown), so a caller that changes one and rebuilds carries the rest forward by
// construction. The slices alias data; copy them before unmapping.
type sidecarSections struct {
	attr    []byte
	key     []byte
	col     []byte
	zone    []byte
	damage  []byte
	profile []byte
	unknown []sidecarUnknownSection

	used    int    // the sealed segment's written extent; 0 = not recorded
	dictRec uint32 // the dictionary record's offset; 0 = not recorded
	colRec  uint32 // the columnar payload record's offset; 0 = not recorded

	ok bool // false: data is not a well-formed container
}

// buildSidecar packs sc into the v7 layout. Zero-length sections are omitted from the
// directory, so a collection with no columnar accelerator and no profile writes only what
// it has; absence and emptiness are the same thing to every reader.
func buildSidecar(sc sidecarSections) []byte {
	type section struct {
		id   uint16
		body []byte
	}
	// attr first so it keeps offset 0 (its bitmaps are offset-sensitive); the rest in id
	// order, which keeps the bytes deterministic for a given set of sections.
	ordered := []section{
		{secAttr, sc.attr},
		{secKey, sc.key},
		{secCol, sc.col},
		{secZone, sc.zone},
		{secDamage, sc.damage},
		{secProfile, sc.profile},
	}
	unknown := append([]sidecarUnknownSection(nil), sc.unknown...)
	sort.Slice(unknown, func(i, j int) bool { return unknown[i].id < unknown[j].id })
	for _, u := range unknown {
		ordered = append(ordered, section{u.id, u.body})
	}

	body, n := 0, 0
	for _, s := range ordered {
		if len(s.body) > 0 {
			body += len(s.body)
			n++
		}
	}
	out := make([]byte, 0, body+n*sidecarDirEntryLen+sidecarTrailerLenV7)
	for _, s := range ordered {
		if len(s.body) > 0 {
			out = append(out, s.body...)
		}
	}
	for _, s := range ordered {
		if len(s.body) == 0 {
			continue
		}
		out = binary.LittleEndian.AppendUint16(out, s.id)
		out = binary.LittleEndian.AppendUint16(out, 0)
		out = binary.LittleEndian.AppendUint32(out, uint32(len(s.body)))
	}
	out = binary.LittleEndian.AppendUint64(out, uint64(sc.used))
	out = binary.LittleEndian.AppendUint32(out, sc.dictRec)
	out = binary.LittleEndian.AppendUint32(out, sc.colRec)
	out = binary.LittleEndian.AppendUint32(out, uint32(n))
	out = binary.LittleEndian.AppendUint32(out, sidecarContainerMagicV7)
	return out
}

// parseSidecar reads any container version. ok is false for bytes that are not a
// well-formed container -- a bare pre-container sidecar, or corruption. A container whose
// directory does not account for exactly its body is rejected rather than sliced on a
// best guess: a section sliced at the wrong offset is not detectably wrong to its reader,
// and a zone map or profile read from the wrong bytes silently drops records from queries
// and from retention.
func parseSidecar(data []byte) sidecarSections {
	if sc, isV7 := splitSidecarV7(data); isV7 {
		return sc
	}
	// v6/v5 (damage section, column record), then v4..v1 via the legacy readers.
	if v5, isV5 := splitSidecarV5(data); isV5 {
		return sidecarSections{attr: v5.attr, key: v5.key, col: v5.col, zone: v5.zone,
			damage: v5.damage, used: v5.used, dictRec: v5.dictRec, colRec: v5.colRec, ok: v5.ok}
	}
	attr, key, col, zone, used, dictRec, ok := splitSidecarLegacy(data)
	return sidecarSections{attr: attr, key: key, col: col, zone: zone,
		used: used, dictRec: dictRec, ok: ok}
}

// splitSidecarV7 splits a v7 container. isV7 is false when data is not one at all; a v7
// container that is malformed returns isV7 true with ok false, so the caller stops rather
// than falling through to a legacy reader that might mis-slice the same bytes.
func splitSidecarV7(data []byte) (out sidecarSections, isV7 bool) {
	if len(data) < sidecarTrailerLenV7 {
		return out, false
	}
	t := data[len(data)-sidecarTrailerLenV7:]
	if binary.LittleEndian.Uint32(t[20:]) != sidecarContainerMagicV7 {
		return out, false
	}
	used := int(binary.LittleEndian.Uint64(t[0:]))
	n := int(binary.LittleEndian.Uint32(t[16:]))
	if used < 0 || n < 0 || n > sidecarMaxSections {
		return out, true
	}
	dirLen := n * sidecarDirEntryLen
	if len(data) < sidecarTrailerLenV7+dirLen {
		return out, true
	}
	dirAt := len(data) - sidecarTrailerLenV7 - dirLen
	dir := data[dirAt : dirAt+dirLen]

	out.used = used
	out.dictRec = binary.LittleEndian.Uint32(t[8:])
	out.colRec = binary.LittleEndian.Uint32(t[12:])

	off, seen := 0, make(map[uint16]struct{}, n)
	for i := 0; i < n; i++ {
		e := dir[i*sidecarDirEntryLen:]
		id := binary.LittleEndian.Uint16(e[0:])
		ln := int(binary.LittleEndian.Uint32(e[4:]))
		if ln < 0 || off+ln > dirAt {
			return sidecarSections{}, true // a length that runs past the body
		}
		if _, dup := seen[id]; dup {
			return sidecarSections{}, true // two sections claiming one id: which is the zone map?
		}
		seen[id] = struct{}{}
		body := data[off : off+ln]
		switch id {
		case secAttr:
			out.attr = body
		case secKey:
			out.key = body
		case secCol:
			out.col = body
		case secZone:
			out.zone = body
		case secDamage:
			out.damage = body
		case secProfile:
			out.profile = body
		default:
			out.unknown = append(out.unknown, sidecarUnknownSection{id: id, body: body})
		}
		off += ln
	}
	if off != dirAt {
		return sidecarSections{}, true // the directory does not account for the whole body
	}
	out.ok = true
	return out, true
}

// clone deep-copies every section, so a container rebuilt from a parse of an mmap'd
// sidecar does not alias the mapping. The rewriting paths unmap before they publish.
func (sc sidecarSections) clone() sidecarSections {
	cp := func(b []byte) []byte {
		if b == nil {
			return nil
		}
		return append([]byte(nil), b...)
	}
	out := sc
	out.attr, out.key, out.col = cp(sc.attr), cp(sc.key), cp(sc.col)
	out.zone, out.damage, out.profile = cp(sc.zone), cp(sc.damage), cp(sc.profile)
	out.unknown = nil
	for _, u := range sc.unknown {
		out.unknown = append(out.unknown, sidecarUnknownSection{id: u.id, body: cp(u.body)})
	}
	return out
}
