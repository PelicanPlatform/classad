package collections

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"sort"
)

// A key-index sidecar is a sealed segment's pageable primary-key index: for every
// record in the segment, an entry mapping the record's key-hash to its in-segment
// offset, sorted by hash for binary search. It lives beside the segment file as
// <seg>.kidx, is mmapped read-only on reopen, and is torn down with the segment.
//
// This is phase 1 of moving the primary key directory out of RAM (see
// docs/pageable-key-index.md): it establishes the on-disk index and the lookup path.
// It does NOT yet replace shard.dir -- the directory is still authoritative and fully
// resident; the sidecar is built and validated beside it. Because a record's currency
// is a local property (supersededBySeq == seqMax), a lookup that finds a key's
// records via the sidecar decides which is live by reading that field, with no
// cross-segment version ordering.
//
// Format (little-endian):
//
//	magic u32 | version u16 | flags u16 | upto u32 | count u32
//	count * { hash u64, off u32 }   (sorted by hash, then off)
//	crc32 u32   (IEEE, over all preceding bytes)
//
// flags was a zero pad until it carried the segment's CurrentTime proof (kidxCTChecked,
// kidxCTDefines; see ctproof.go). The version is unchanged because the field is optional both ways:
// a build that predates it reads the flags as padding and ignores them, and writes zero, which this
// build reads as "not checked".
const (
	keyIdxMagic     = 0x4b494458 // "KIDX"
	keyIdxVersion   = 1
	keyIdxHeaderLen = 16 // magic+version+pad+upto+count
	keyIdxEntryLen  = 12 // hash u64 + off u32
)

// A segment's two TRAILER records -- its attribute dictionary (appendDict) and its columnar
// payload (appendCol) -- are found at open by walking the segment's records until one of them
// turns up. The walk reads only headers, but it touches every record, so a segment that carries
// NEITHER (the common case: not interned, not columnarized) is walked end to end, twice, on
// every open, to establish that there is nothing to find. Across an archive that is a full pass
// over every record at every start, and the answer -- "there is none" -- is held in RAM and
// thrown away at close, so it is rediscovered forever.
//
// These two bits record the answer instead. They are written into the key index of a SEALED
// segment's sidecar, whose data is immutable, so the absence they state cannot stop being true.
// A reopen that sees one skips the corresponding walk outright.
//
// Zero means "not recorded", never "present": a sidecar written before these bits existed, or by
// a segment whose framing was in doubt, reads as zero and is walked exactly as before. Setting a
// bit is therefore the only claim being made, and it is made only where the whole segment was
// walked (or written) with no damage anywhere in it.
const (
	kidxNoDictRec uint16 = 1 << 2 // segment carries no attribute-dictionary record
	kidxNoColRec  uint16 = 1 << 3 // segment carries no columnar-payload record
)

// reflagKeyIndex returns a copy of an existing key-index section with its flags field replaced
// and its checksum recomputed. It is how a fact learned at open is written into a sidecar that
// was built without it, WITHOUT rebuilding the index -- which would mean walking the segment's
// records again, the very cost being removed. Returns nil if key is not a well-formed index.
func reflagKeyIndex(key []byte, flags uint16) []byte {
	if _, err := parseKeyIndex(key); err != nil {
		return nil
	}
	out := append([]byte(nil), key...)
	binary.LittleEndian.PutUint16(out[6:], flags)
	binary.LittleEndian.PutUint32(out[len(out)-4:], crc32.ChecksumIEEE(out[:len(out)-4]))
	return out
}

// buildKeyIndex scans a segment's records in [0, upto) and returns the serialized key
// sidecar: one (key-hash, offset) entry per record, sorted by hash. h must be the
// same Hasher the directory uses, so sidecar hashes match directory hashes.
//
// dm is the segment's damage set: a damaged record is left out, so no lookup reaches it.
//
// flags is the key index's flags field: the segment's CurrentTime proof over [0, upto)
// (segment.ctKIDXFlags), or zero.
func buildKeyIndex(data []byte, upto int, h Hasher, dm *damageSet, flags uint16) []byte {
	type ent struct {
		hash uint64
		off  uint32
	}
	var ents []ent
	for off := 0; off < upto; {
		o := uint32(off)
		if end, ok := dm.skip(o); ok {
			off = int(end)
			continue
		}
		total := recTotalLen(data, o)
		if total == 0 {
			break
		}
		ents = append(ents, ent{h.Hash(recKey(data, o)), o})
		off += int(total)
	}
	sort.Slice(ents, func(i, j int) bool {
		if ents[i].hash != ents[j].hash {
			return ents[i].hash < ents[j].hash
		}
		return ents[i].off < ents[j].off
	})

	buf := make([]byte, 0, keyIdxHeaderLen+len(ents)*keyIdxEntryLen+4)
	buf = binary.LittleEndian.AppendUint32(buf, keyIdxMagic)
	buf = binary.LittleEndian.AppendUint16(buf, keyIdxVersion)
	buf = binary.LittleEndian.AppendUint16(buf, flags)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(upto))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(ents)))
	for _, e := range ents {
		buf = binary.LittleEndian.AppendUint64(buf, e.hash)
		buf = binary.LittleEndian.AppendUint32(buf, e.off)
	}
	buf = binary.LittleEndian.AppendUint32(buf, crc32.ChecksumIEEE(buf))
	return buf
}

// mmapKeyIndex is a read-only view over a key sidecar's bytes (an mmap on reopen, or
// a plain slice in tests). It holds no heap copy of the entries -- lookups binary-
// search directly over the mapping, so a large sidecar stays pageable.
type mmapKeyIndex struct {
	upto  uint32
	count uint32
	flags uint16 // the header's flags field (see the format above)
	ents  []byte // count*keyIdxEntryLen bytes, sorted by hash
}

// parseKeyIndex validates a key sidecar's bytes (magic, version, CRC, length) and
// returns a view over them. The bytes must outlive the view (they alias the mmap).
func parseKeyIndex(data []byte) (*mmapKeyIndex, error) {
	if len(data) < keyIdxHeaderLen+4 {
		return nil, fmt.Errorf("collections: key sidecar too short")
	}
	if binary.LittleEndian.Uint32(data) != keyIdxMagic {
		return nil, fmt.Errorf("collections: key sidecar bad magic")
	}
	if v := binary.LittleEndian.Uint16(data[4:]); v != keyIdxVersion {
		return nil, fmt.Errorf("collections: key sidecar version %d", v)
	}
	body := data[:len(data)-4]
	if crc32.ChecksumIEEE(body) != binary.LittleEndian.Uint32(data[len(data)-4:]) {
		return nil, fmt.Errorf("collections: key sidecar bad CRC")
	}
	upto := binary.LittleEndian.Uint32(data[8:])
	count := binary.LittleEndian.Uint32(data[12:])
	entStart := keyIdxHeaderLen
	entEnd := entStart + int(count)*keyIdxEntryLen
	if entEnd != len(body) {
		return nil, fmt.Errorf("collections: key sidecar count %d does not match length", count)
	}
	return &mmapKeyIndex{upto: upto, count: count, flags: binary.LittleEndian.Uint16(data[6:]), ents: data[entStart:entEnd]}, nil
}

func (m *mmapKeyIndex) hashAt(i uint32) uint64 {
	return binary.LittleEndian.Uint64(m.ents[i*keyIdxEntryLen:])
}
func (m *mmapKeyIndex) offAt(i uint32) uint32 {
	return binary.LittleEndian.Uint32(m.ents[i*keyIdxEntryLen+8:])
}

// lookup returns the offsets of every record in the segment whose key-hash == hash
// (a key's several versions, plus any hash collisions). The caller reads each record
// to match the full key and pick the live one (supersededBySeq == seqMax).
func (m *mmapKeyIndex) lookup(hash uint64) []uint32 {
	lo, hi := uint32(0), m.count
	for lo < hi {
		mid := (lo + hi) / 2
		if m.hashAt(mid) < hash {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	var offs []uint32
	for i := lo; i < m.count && m.hashAt(i) == hash; i++ {
		offs = append(offs, m.offAt(i))
	}
	return offs
}

// A segment sidecar container packs the (optional) attribute-index blob, the key-index blob, and
// the (optional) columnar-accelerator blob into ONE file (<seg>.idx), so a persistent segment
// carries a single sidecar rather than three. The attribute blob is written first so it keeps
// offset 0 -- its roaring bitmaps stay aligned and the existing ARCX writer/parser (shared with
// the archive table type) handle it unchanged; the offset-insensitive key blob follows; the
// columnar blob (if any) follows that. A fixed trailer records the section lengths. Two versions:
//
//	v1 (no columnar block): [attr][key][attrLen u32 | keyLen u32 | magic("SCNT") u32]   (12-byte)
//	v2 (with columnar):     [attr][key][col][attrLen u32 | keyLen u32 | colLen u32 | magic("SCN2") u32]  (16-byte)
//
// An empty columnar blob writes v1 byte-for-byte as before, so a collection without the columnar
// accelerator produces identical sidecars. The container magic is always the last 4 bytes, and the
// two magics differ, so the reader distinguishes the versions unambiguously; a v1 sidecar (or a
// pre-columnar file) reads with col == nil.
const (
	sidecarContainerMagic   = 0x53434e54 // "SCNT" (v1, 2-section)
	sidecarContainerMagicV2 = 0x53434e32 // "SCN2" (v2, 3-section, adds the columnar blob)
	sidecarContainerMagicV3 = 0x53434e33 // "SCN3" (v3, adds the zone map + the sealed extent)
	sidecarContainerMagicV4 = 0x53434e34 // "SCN4" (v4, adds the dictionary record's offset)
	sidecarContainerMagicV5 = 0x53434e35 // "SCN5" (v5, adds the segment's damaged spans)
	sidecarContainerMagicV6 = 0x53434e36 // "SCN6" (v6, adds the columnar payload record's offset)
	sidecarTrailerLen       = 12
	sidecarTrailerLenV2     = 16
	sidecarTrailerLenV3     = 28 // 4 lengths (u32) + segUsed (u64) + magic (u32)
	sidecarTrailerLenV4     = 32 // v3 + dictRec (u32)
	sidecarTrailerLenV5     = 36 // 5 lengths (u32) + segUsed (u64) + dictRec (u32) + magic (u32)
	sidecarTrailerLenV6     = 40 // v5 + colRec (u32)
)

// buildSegmentSidecar packs the attribute-index blob (may be empty), the key-index blob, and the
// columnar blob (may be empty) into the container byte layout. An empty colBlob yields the v1
// (2-section) layout, identical to a pre-columnar sidecar.
// segUsed is the sealed segment's written extent. Recording it lets recovery skip walking and
// CRC-verifying every record just to find where durable data ends -- a sealed segment's extent
// is final, so re-deriving it on every open is a full integrity scan of the whole archive
// masquerading as recovery. Zero means "not recorded" and the reader falls back to scanning.
//
// damageBlob is the segment's damaged spans (encodeDamage), which every walk must skip. A segment
// with any is written as v5, which adds that section after the zone map and its length to the
// trailer: [attr][key][col][zone][damage][attrLen|keyLen|colLen|zoneLen|damageLen u32 | segUsed u64 |
// dictRec u32 | magic("SCN5") u32]. A build that predates v5 does not recognise the container, so it
// neither trusts the extent nor adopts the key index: it walks the segment, as it would with no
// sidecar, and stops at the damage rather than read through it. A segment without damage is written
// as before.
//
// colRec is where the segment's columnar payload record is, which a segment that has one is written
// as v6 to carry: [attr][key][col][zone][damage][the five lengths | segUsed u64 | dictRec u32 |
// colRec u32 | magic("SCN6") u32], the damage section empty when there is none. A build that
// predates v6 does not recognise the container and falls back to walking the segment, so the field
// costs nothing but the search it saves.
func buildSegmentSidecar(attrBlob, keyBlob, colBlob, zoneBlob []byte, segUsed int, dictRec, colRec uint32, damageBlob []byte) []byte {
	// v6, written only for a segment that HAS a columnar payload record: its offset, like the
	// dictionary's, is what spares a reopen the walk that would otherwise look for it -- a walk
	// over every record of the segment, and so over every record of the store. A segment without
	// one writes exactly the container it did before, so nothing else on disk changes shape.
	if colRec > 0 {
		b := make([]byte, 0, len(attrBlob)+len(keyBlob)+len(colBlob)+len(zoneBlob)+len(damageBlob)+sidecarTrailerLenV6)
		b = append(b, attrBlob...)
		b = append(b, keyBlob...)
		b = append(b, colBlob...)
		b = append(b, zoneBlob...)
		b = append(b, damageBlob...)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(attrBlob)))
		b = binary.LittleEndian.AppendUint32(b, uint32(len(keyBlob)))
		b = binary.LittleEndian.AppendUint32(b, uint32(len(colBlob)))
		b = binary.LittleEndian.AppendUint32(b, uint32(len(zoneBlob)))
		b = binary.LittleEndian.AppendUint32(b, uint32(len(damageBlob)))
		b = binary.LittleEndian.AppendUint64(b, uint64(segUsed))
		b = binary.LittleEndian.AppendUint32(b, dictRec)
		b = binary.LittleEndian.AppendUint32(b, colRec)
		b = binary.LittleEndian.AppendUint32(b, sidecarContainerMagicV6)
		return b
	}
	if len(damageBlob) > 0 {
		b := make([]byte, 0, len(attrBlob)+len(keyBlob)+len(colBlob)+len(zoneBlob)+len(damageBlob)+sidecarTrailerLenV5)
		b = append(b, attrBlob...)
		b = append(b, keyBlob...)
		b = append(b, colBlob...)
		b = append(b, zoneBlob...)
		b = append(b, damageBlob...)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(attrBlob)))
		b = binary.LittleEndian.AppendUint32(b, uint32(len(keyBlob)))
		b = binary.LittleEndian.AppendUint32(b, uint32(len(colBlob)))
		b = binary.LittleEndian.AppendUint32(b, uint32(len(zoneBlob)))
		b = binary.LittleEndian.AppendUint32(b, uint32(len(damageBlob)))
		b = binary.LittleEndian.AppendUint64(b, uint64(segUsed))
		b = binary.LittleEndian.AppendUint32(b, dictRec)
		b = binary.LittleEndian.AppendUint32(b, sidecarContainerMagicV5)
		return b
	}
	if len(zoneBlob) > 0 || segUsed > 0 || dictRec > 0 {
		b := make([]byte, 0, len(attrBlob)+len(keyBlob)+len(colBlob)+len(zoneBlob)+sidecarTrailerLenV4)
		b = append(b, attrBlob...)
		b = append(b, keyBlob...)
		b = append(b, colBlob...)
		b = append(b, zoneBlob...)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(attrBlob)))
		b = binary.LittleEndian.AppendUint32(b, uint32(len(keyBlob)))
		b = binary.LittleEndian.AppendUint32(b, uint32(len(colBlob)))
		b = binary.LittleEndian.AppendUint32(b, uint32(len(zoneBlob)))
		b = binary.LittleEndian.AppendUint64(b, uint64(segUsed))
		b = binary.LittleEndian.AppendUint32(b, dictRec)
		b = binary.LittleEndian.AppendUint32(b, sidecarContainerMagicV4)
		return b
	}
	if len(colBlob) == 0 {
		b := make([]byte, 0, len(attrBlob)+len(keyBlob)+sidecarTrailerLen)
		b = append(b, attrBlob...)
		b = append(b, keyBlob...)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(attrBlob)))
		b = binary.LittleEndian.AppendUint32(b, uint32(len(keyBlob)))
		b = binary.LittleEndian.AppendUint32(b, sidecarContainerMagic)
		return b
	}
	b := make([]byte, 0, len(attrBlob)+len(keyBlob)+len(colBlob)+sidecarTrailerLenV2)
	b = append(b, attrBlob...)
	b = append(b, keyBlob...)
	b = append(b, colBlob...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(attrBlob)))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(keyBlob)))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(colBlob)))
	b = binary.LittleEndian.AppendUint32(b, sidecarContainerMagicV2)
	return b
}

// splitSegmentSidecar returns the attribute, key, and columnar sub-slices of a container. col is
// nil for a v1 (2-section) container -- a pre-columnar sidecar, read unchanged. ok=false if data
// is not a well-formed container (bare/old sidecar, or corruption).
func splitSegmentSidecar(data []byte) (attr, key, col, zone []byte, ok bool) {
	a, k, c, z, _, o := splitSegmentSidecarFull(data)
	return a, k, c, z, o
}

// splitSegmentSidecarFull also returns the recorded sealed extent (0 when absent).
func splitSegmentSidecarFull(data []byte) (attr, key, col, zone []byte, segUsed int, ok bool) {
	a, k, c, z, u, _, o := splitSegmentSidecarV4(data)
	return a, k, c, z, u, o
}

// splitSegmentSidecarV4 also returns the recorded dictionary record offset (0 when absent).
func splitSegmentSidecarV4(data []byte) (attr, key, col, zone []byte, segUsed int, dictRec uint32, ok bool) {
	if v5, isV5 := splitSidecarV5(data); isV5 {
		return v5.attr, v5.key, v5.col, v5.zone, v5.used, v5.dictRec, v5.ok
	}
	if len(data) >= sidecarTrailerLenV4 {
		t := data[len(data)-sidecarTrailerLenV4:]
		if binary.LittleEndian.Uint32(t[28:]) == sidecarContainerMagicV4 {
			attrLen := int(binary.LittleEndian.Uint32(t[0:]))
			keyLen := int(binary.LittleEndian.Uint32(t[4:]))
			colLen := int(binary.LittleEndian.Uint32(t[8:]))
			zoneLen := int(binary.LittleEndian.Uint32(t[12:]))
			used := int(binary.LittleEndian.Uint64(t[16:]))
			rec := binary.LittleEndian.Uint32(t[24:])
			body := len(data) - sidecarTrailerLenV4
			if attrLen >= 0 && keyLen >= 0 && colLen >= 0 && zoneLen >= 0 &&
				attrLen+keyLen+colLen+zoneLen == body {
				return data[:attrLen], data[attrLen : attrLen+keyLen],
					data[attrLen+keyLen : attrLen+keyLen+colLen],
					data[attrLen+keyLen+colLen : body], used, rec, true
			}
			return nil, nil, nil, nil, 0, 0, false
		}
	}
	a, k, c, z, u, o := splitSegmentSidecarOld(data)
	return a, k, c, z, u, 0, o
}

// sidecarV5 is a v5 (or v6) container's sections and trailer fields.
type sidecarV5 struct {
	attr, key, col, zone, damage []byte
	used                         int
	dictRec                      uint32
	colRec                       uint32 // v6 only; 0 in a v5 container, meaning "not recorded"
	ok                           bool   // false for a container that is malformed
}

// splitSidecarV6 splits a v6 container -- v5 plus the columnar payload record's offset. isV6 is
// false when data is not a v6 container at all.
func splitSidecarV6(data []byte) (out sidecarV5, isV6 bool) {
	if len(data) < sidecarTrailerLenV6 {
		return out, false
	}
	t := data[len(data)-sidecarTrailerLenV6:]
	if binary.LittleEndian.Uint32(t[36:]) != sidecarContainerMagicV6 {
		return out, false
	}
	var l [5]int
	sum := 0
	for i := range l {
		l[i] = int(binary.LittleEndian.Uint32(t[4*i:]))
		sum += l[i]
	}
	used := int(binary.LittleEndian.Uint64(t[20:]))
	if sum != len(data)-sidecarTrailerLenV6 || used < 0 {
		return out, true
	}
	a := l[0]
	k := a + l[1]
	c := k + l[2]
	z := c + l[3]
	return sidecarV5{attr: data[:a], key: data[a:k], col: data[k:c], zone: data[c:z], damage: data[z:sum],
		used: used, dictRec: binary.LittleEndian.Uint32(t[28:]),
		colRec: binary.LittleEndian.Uint32(t[32:]), ok: true}, true
}

// splitSidecarV5 splits a v5 container, damage section included -- or a v6 one, which is a v5
// container with one more trailer field. isV5 is false when data is neither.
func splitSidecarV5(data []byte) (out sidecarV5, isV5 bool) {
	if v6, isV6 := splitSidecarV6(data); isV6 {
		return v6, true
	}
	if len(data) < sidecarTrailerLenV5 {
		return out, false
	}
	t := data[len(data)-sidecarTrailerLenV5:]
	if binary.LittleEndian.Uint32(t[32:]) != sidecarContainerMagicV5 {
		return out, false
	}
	var l [5]int
	sum := 0
	for i := range l {
		l[i] = int(binary.LittleEndian.Uint32(t[4*i:]))
		sum += l[i]
	}
	used := int(binary.LittleEndian.Uint64(t[20:]))
	if sum != len(data)-sidecarTrailerLenV5 || used < 0 {
		return out, true
	}
	a := l[0]
	k := a + l[1]
	c := k + l[2]
	z := c + l[3]
	return sidecarV5{attr: data[:a], key: data[a:k], col: data[k:c], zone: data[c:z], damage: data[z:sum],
		used: used, dictRec: binary.LittleEndian.Uint32(t[28:]), ok: true}, true
}

func splitSegmentSidecarOld(data []byte) (attr, key, col, zone []byte, segUsed int, ok bool) {
	if len(data) >= sidecarTrailerLenV3 {
		t := data[len(data)-sidecarTrailerLenV3:]
		if binary.LittleEndian.Uint32(t[24:]) == sidecarContainerMagicV3 {
			attrLen := int(binary.LittleEndian.Uint32(t[0:]))
			keyLen := int(binary.LittleEndian.Uint32(t[4:]))
			colLen := int(binary.LittleEndian.Uint32(t[8:]))
			zoneLen := int(binary.LittleEndian.Uint32(t[12:]))
			used := int(binary.LittleEndian.Uint64(t[16:]))
			body := len(data) - sidecarTrailerLenV3
			if attrLen >= 0 && keyLen >= 0 && colLen >= 0 && zoneLen >= 0 &&
				attrLen+keyLen+colLen+zoneLen == body {
				return data[:attrLen], data[attrLen : attrLen+keyLen],
					data[attrLen+keyLen : attrLen+keyLen+colLen],
					data[attrLen+keyLen+colLen : body], used, true
			}
			return nil, nil, nil, nil, 0, false
		}
	}
	if len(data) >= sidecarTrailerLenV2 {
		t := data[len(data)-sidecarTrailerLenV2:]
		if binary.LittleEndian.Uint32(t[12:]) == sidecarContainerMagicV2 {
			attrLen := int(binary.LittleEndian.Uint32(t[0:]))
			keyLen := int(binary.LittleEndian.Uint32(t[4:]))
			colLen := int(binary.LittleEndian.Uint32(t[8:]))
			if attrLen < 0 || keyLen < 0 || colLen < 0 || attrLen+keyLen+colLen+sidecarTrailerLenV2 != len(data) {
				return nil, nil, nil, nil, 0, false
			}
			return data[:attrLen], data[attrLen : attrLen+keyLen], data[attrLen+keyLen : attrLen+keyLen+colLen], nil, 0, true
		}
	}
	if len(data) >= sidecarTrailerLen {
		t := data[len(data)-sidecarTrailerLen:]
		if binary.LittleEndian.Uint32(t[8:]) == sidecarContainerMagic {
			attrLen := int(binary.LittleEndian.Uint32(t[0:]))
			keyLen := int(binary.LittleEndian.Uint32(t[4:]))
			if attrLen < 0 || keyLen < 0 || attrLen+keyLen+sidecarTrailerLen != len(data) {
				return nil, nil, nil, nil, 0, false
			}
			return data[:attrLen], data[attrLen : attrLen+keyLen], nil, nil, 0, true
		}
	}
	return nil, nil, nil, nil, 0, false
}

// writeFaultHook is a test seam: when non-nil and it returns a non-nil error for a given target
// path, the rebuildable-metadata write to that path fails with that error, simulating a disk-full
// (ENOSPC) at a specific site (sidecar / dir snapshot / dictionary). Keyed by path so a test can
// fail exactly one site deterministically. Nil in production. Checked in writeFileAtomic (sidecars),
// writeDirSnapshot, and writeDictFile.
var writeFaultHook func(path string) error

// writeFileAtomic writes b to path via a temp file + fsync + rename, so a crash never
// leaves a torn sidecar.
func writeFileAtomic(path string, b []byte) error {
	if writeFaultHook != nil {
		if err := writeFaultHook(path); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// --- persisted zone maps (sidecar v3) ---
//
// A sealed segment's zone map is derived state: [min,max] per zoned attribute, computed by
// decoding every record. It was recomputed on every open, which made recovery O(records) --
// measured at ~95% of reopen time, and hours for an archive of any size. It is computed once
// at seal anyway, so persist it beside the index it lives with.
//
// The section is keyed by attribute NAME. The first format keyed it by the attribute's intern
// id, which is process-local: ids are handed out in the order a process meets names, so once the
// zone or value-index configuration changes (an attribute added, removed or reordered -- or a value
// index added at runtime, whose id comes after the data's) the same id names a different
// attribute in the next process. A zone map adopted that way applies one attribute's [min,max]
// to another and prunes segments that hold matches: silently wrong answers, and age retention
// dropping segments by the wrong attribute.
//
// Format: [zoneBlobMagic u32][n u32] then n x { nameLen u16, name, flags u8, min f64, max f64 },
// flags bit 0 being zoneRange.Full. A segment with zone attributes but no entries writes the
// header alone, so it is adopted as such rather than recomputed at every open; a segment with no
// zone attributes writes nothing.
//
// Two older formats are well-formed but never adopted; the segment's zones are recomputed once
// and the sidecar rewritten in this format (see persistSegZonesUpgrade):
//   - id-keyed ([n u32] then n x { id u32, min f64, max f64 }): cannot be told which attributes
//     its ids meant. Its marker-free layout cannot be mistaken for a marked one, which would need
//     a 16 GiB blob.
//   - "ZNM2" (as above without flags): its ranges skipped every value that was not a numeric
//     literal, expressions included, so one can exclude a record whose expression evaluates
//     into a queried range, and pruning on it drops that record.
//
// An older build rejects this section as malformed and recomputes, since neither its marker nor
// the id-keyed layout matches.

// zoneBlobMagic opens a name-keyed zone section with flags ("ZNM3").
const zoneBlobMagic uint32 = 0x334d4e5a

// zoneBlobMagicV2 opened the previous name-keyed section ("ZNM2"), which is never adopted.
const zoneBlobMagicV2 uint32 = 0x324d4e5a

// zoneFlagFull marks a zoneRange.Full entry; any other flag bit makes the section malformed.
const zoneFlagFull = 1 << 0

// zoneBlobStatus is what parseZoneBlob made of a section.
type zoneBlobStatus uint8

const (
	zoneBlobBad    zoneBlobStatus = iota // empty or malformed: not a zone map
	zoneBlobLegacy                       // an older format (id-keyed or ZNM2): not to be trusted
	zoneBlobOK
)

// buildZoneBlob encodes a zone map keyed by the names nameOf gives its ids. An id with no name
// is left out: a missing entry only stops that attribute pruning, where a wrong one would prune
// wrongly. A nil map encodes as nothing, an empty one as the header alone.
func buildZoneBlob(zones map[uint32]zoneRange, nameOf func(uint32) (string, bool)) []byte {
	if zones == nil {
		return nil
	}
	type entry struct {
		name string
		z    zoneRange
	}
	entries := make([]entry, 0, len(zones))
	for id, z := range zones {
		if name, ok := nameOf(id); ok && name != "" && len(name) <= math.MaxUint16 {
			entries = append(entries, entry{name, z})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	b := make([]byte, 0, 8+len(entries)*33)
	b = binary.LittleEndian.AppendUint32(b, zoneBlobMagic)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(entries)))
	for _, e := range entries {
		b = binary.LittleEndian.AppendUint16(b, uint16(len(e.name)))
		b = append(b, e.name...)
		var flags byte
		if e.z.Full {
			flags |= zoneFlagFull
		}
		b = append(b, flags)
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(e.z.Min))
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(e.z.Max))
	}
	return b
}

// parseZoneBlob decodes a zone section, binding each name to this process's id for it via idOf; a
// name idOf does not know is dropped. Only a zoneBlobOK result is a zone map -- a corrupt or
// legacy section must never yield one, since a too-narrow or misattributed range silently prunes
// segments that hold matches.
func parseZoneBlob(b []byte, idOf func(string) (uint32, bool)) (map[uint32]zoneRange, zoneBlobStatus) {
	if len(b) < 4 {
		return nil, zoneBlobBad
	}
	switch magic := binary.LittleEndian.Uint32(b); {
	case magic == zoneBlobMagicV2:
		return nil, zoneBlobLegacy
	case magic != zoneBlobMagic:
		if legacyZoneBlobValid(b) {
			return nil, zoneBlobLegacy
		}
		return nil, zoneBlobBad
	}
	if len(b) < 8 {
		return nil, zoneBlobBad
	}
	n := int(binary.LittleEndian.Uint32(b[4:]))
	out := make(map[uint32]zoneRange, min(n, len(b)/19))
	off := 8
	for range n {
		if len(b) < off+2 {
			return nil, zoneBlobBad
		}
		nl := int(binary.LittleEndian.Uint16(b[off:]))
		off += 2
		if nl == 0 || len(b) < off+nl+17 {
			return nil, zoneBlobBad
		}
		name := string(b[off : off+nl])
		off += nl
		flags := b[off]
		mn := math.Float64frombits(binary.LittleEndian.Uint64(b[off+1:]))
		mx := math.Float64frombits(binary.LittleEndian.Uint64(b[off+9:]))
		off += 17
		if mn > mx || flags&^zoneFlagFull != 0 {
			return nil, zoneBlobBad
		}
		if id, ok := idOf(name); ok {
			out[id] = zoneRange{Min: mn, Max: mx, Full: flags&zoneFlagFull != 0}
		}
	}
	if off != len(b) {
		return nil, zoneBlobBad
	}
	return out, zoneBlobOK
}

// legacyZoneBlobValid reports whether b is a well-formed id-keyed section.
func legacyZoneBlobValid(b []byte) bool {
	n := int(binary.LittleEndian.Uint32(b))
	if n < 0 || 4+n*20 != len(b) {
		return false
	}
	for i := 0; i < n; i++ {
		off := 4 + i*20
		if math.Float64frombits(binary.LittleEndian.Uint64(b[off+4:])) > math.Float64frombits(binary.LittleEndian.Uint64(b[off+12:])) {
			return false
		}
	}
	return true
}

// readSidecarExtent reads just the trailer of a sealed segment's sidecar to recover the
// segment's written extent, without mapping the sidecar (mapping every sidecar at open is
// itself a cost worth avoiding). Returns 0 when the file is absent, too short, or not a v3
// container -- the caller then falls back to scanning.
func readSidecarExtent(path string) int {
	return readSidecarTrailer(path).used
}

// sidecarFacts is what a sealed segment's sidecar trailer tells recovery before anything else of
// the sidecar is mapped: the segment's written extent, where its two trailer records are, which of
// them it does not have, and which of its records are damaged.
//
// They are read together because they are needed together and in the same place -- at the top of
// recovery, before the segment is walked -- and because every one of them exists to spare that
// walk. A zero field means "not recorded" in every case, never a positive claim, so an older
// container, a short read or anything unexpected leaves recovery doing exactly what it did
// before.
type sidecarFacts struct {
	used    int    // the sealed extent (0: not recorded)
	dictRec uint32 // offset of the attribute-dictionary record (0: not recorded)
	colRec  uint32 // offset of the columnar payload record (0: not recorded; v6 containers only)
	flags   uint16 // the key index's flags, incl. which trailer records the segment has none of
	damage  *damageSet
}

// readSidecarTrailer reads a sealed segment's sidecar trailer and returns its facts. One open
// serves all of them, so none of the hints costs a syscall of its own.
//
// The key-index flags are read HERE, out of the key section the trailer locates, because the two
// trailer-record searches they short-circuit both run before the sidecar itself is mapped.
//
// v3 containers carry the extent but no record offsets. A v5 container also carries the segment's
// damaged spans; one whose set does not parse returns nothing at all, so the segment is walked
// rather than trusted without knowing what to skip.
func readSidecarTrailer(path string) (out sidecarFacts) {
	f, err := os.Open(path)
	if err != nil {
		return sidecarFacts{}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return sidecarFacts{}
	}
	// kidxFlags reads the flags word out of the key-index section that starts at attrLen.
	kidxFlags := func(attrLen, keyLen uint32) uint16 {
		if int64(attrLen)+int64(keyLen) > st.Size() || keyLen < keyIdxHeaderLen {
			return 0
		}
		var h [keyIdxHeaderLen]byte
		if _, err := f.ReadAt(h[:], int64(attrLen)); err != nil {
			return 0
		}
		if binary.LittleEndian.Uint32(h[0:]) != keyIdxMagic || binary.LittleEndian.Uint16(h[4:]) != keyIdxVersion {
			return 0
		}
		return binary.LittleEndian.Uint16(h[6:])
	}
	// readDamage reads and decodes the damage section a v5/v6 trailer describes, which sits
	// immediately before the trailer. ok is false when the section is implausible or will not
	// decode: the extent is then not trusted either, because a walk that does not know what to
	// skip would read through the damage.
	//
	// A zero-length section means the segment has no damage, which is the ordinary case for a v6
	// container -- v6 is written for the columnar payload's offset, not for damage, so most v6
	// containers have no damage section at all. A v5 container is written only when there IS
	// damage, so a zero-length one there is a malformed trailer.
	readDamage := func(dl int64, used int, trailerLen int64, require bool) (*damageSet, int, bool) {
		at := st.Size() - trailerLen - dl
		if used <= 0 || dl < 0 || at < 0 || (require && dl == 0) {
			return nil, 0, false
		}
		if dl == 0 {
			return nil, used, true
		}
		b := make([]byte, dl)
		if _, err := f.ReadAt(b, at); err != nil {
			return nil, 0, false
		}
		d, ok := decodeDamage(b)
		if !ok {
			return nil, 0, false
		}
		return d, used, true
	}
	if st.Size() >= int64(sidecarTrailerLenV6) {
		var t [sidecarTrailerLenV6]byte
		if _, err := f.ReadAt(t[:], st.Size()-int64(sidecarTrailerLenV6)); err == nil &&
			binary.LittleEndian.Uint32(t[36:]) == sidecarContainerMagicV6 {
			d, u, ok := readDamage(int64(binary.LittleEndian.Uint32(t[16:])),
				int(binary.LittleEndian.Uint64(t[20:])), int64(sidecarTrailerLenV6), false)
			if !ok {
				return sidecarFacts{}
			}
			return sidecarFacts{used: u, dictRec: binary.LittleEndian.Uint32(t[28:]),
				colRec: binary.LittleEndian.Uint32(t[32:]),
				flags:  kidxFlags(binary.LittleEndian.Uint32(t[0:]), binary.LittleEndian.Uint32(t[4:])), damage: d}
		}
	}
	if st.Size() >= int64(sidecarTrailerLenV5) {
		var t [sidecarTrailerLenV5]byte
		if _, err := f.ReadAt(t[:], st.Size()-int64(sidecarTrailerLenV5)); err == nil &&
			binary.LittleEndian.Uint32(t[32:]) == sidecarContainerMagicV5 {
			d, u, ok := readDamage(int64(binary.LittleEndian.Uint32(t[16:])),
				int(binary.LittleEndian.Uint64(t[20:])), int64(sidecarTrailerLenV5), true)
			if !ok {
				return sidecarFacts{}
			}
			return sidecarFacts{used: u, dictRec: binary.LittleEndian.Uint32(t[28:]),
				flags: kidxFlags(binary.LittleEndian.Uint32(t[0:]), binary.LittleEndian.Uint32(t[4:])), damage: d}
		}
	}
	if st.Size() >= int64(sidecarTrailerLenV4) {
		var t [sidecarTrailerLenV4]byte
		if _, err := f.ReadAt(t[:], st.Size()-int64(sidecarTrailerLenV4)); err == nil &&
			binary.LittleEndian.Uint32(t[28:]) == sidecarContainerMagicV4 {
			u := int(binary.LittleEndian.Uint64(t[16:]))
			if u < 0 {
				return sidecarFacts{}
			}
			return sidecarFacts{used: u, dictRec: binary.LittleEndian.Uint32(t[24:]),
				flags: kidxFlags(binary.LittleEndian.Uint32(t[0:]), binary.LittleEndian.Uint32(t[4:]))}
		}
	}
	if st.Size() < int64(sidecarTrailerLenV3) {
		return sidecarFacts{}
	}
	var t [sidecarTrailerLenV3]byte
	if _, err := f.ReadAt(t[:], st.Size()-int64(sidecarTrailerLenV3)); err != nil {
		return sidecarFacts{}
	}
	if binary.LittleEndian.Uint32(t[24:]) != sidecarContainerMagicV3 {
		return sidecarFacts{}
	}
	u := int(binary.LittleEndian.Uint64(t[16:]))
	if u < 0 {
		return sidecarFacts{}
	}
	return sidecarFacts{used: u}
}
