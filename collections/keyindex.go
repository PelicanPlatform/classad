package collections

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
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
//	magic u32 | version u16 | pad u16 | upto u32 | count u32
//	count * { hash u64, off u32 }   (sorted by hash, then off)
//	crc32 u32   (IEEE, over all preceding bytes)
const (
	keyIdxMagic     = 0x4b494458 // "KIDX"
	keyIdxVersion   = 1
	keyIdxHeaderLen = 16 // magic+version+pad+upto+count
	keyIdxEntryLen  = 12 // hash u64 + off u32
)

// buildKeyIndex scans a segment's records in [0, upto) and returns the serialized key
// sidecar: one (key-hash, offset) entry per record, sorted by hash. h must be the
// same Hasher the directory uses, so sidecar hashes match directory hashes.
func buildKeyIndex(data []byte, upto int, h Hasher) []byte {
	type ent struct {
		hash uint64
		off  uint32
	}
	var ents []ent
	for off := 0; off < upto; {
		o := uint32(off)
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
	buf = binary.LittleEndian.AppendUint16(buf, 0)
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
	return &mmapKeyIndex{upto: upto, count: count, ents: data[entStart:entEnd]}, nil
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

// A segment sidecar container packs the (optional) attribute-index blob and the
// key-index blob into ONE file (<seg>.idx), so a persistent segment carries a single
// sidecar rather than two. The attribute blob is written first so it keeps offset 0 --
// its roaring bitmaps stay aligned and the existing ARCX writer/parser (shared with
// the archive table type) handle it unchanged; the offset-insensitive key blob
// follows; a fixed trailer records the two lengths:
//
//	[attr blob (attrLen bytes; ARCX; empty when no attr index)]
//	[key  blob (keyLen bytes; KIDX)]
//	[attrLen u32 | keyLen u32 | containerMagic u32]   (12-byte trailer)
const (
	sidecarContainerMagic = 0x53434e54 // "SCNT"
	sidecarTrailerLen     = 12
)

// buildSegmentSidecar packs an attribute-index blob (may be empty) and a key-index
// blob into the container byte layout.
func buildSegmentSidecar(attrBlob, keyBlob []byte) []byte {
	b := make([]byte, 0, len(attrBlob)+len(keyBlob)+sidecarTrailerLen)
	b = append(b, attrBlob...)
	b = append(b, keyBlob...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(attrBlob)))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(keyBlob)))
	b = binary.LittleEndian.AppendUint32(b, sidecarContainerMagic)
	return b
}

// splitSegmentSidecar returns the attribute and key sub-slices of a container, or
// ok=false if data is not a well-formed container (bare/old sidecar, or corruption).
func splitSegmentSidecar(data []byte) (attr, key []byte, ok bool) {
	if len(data) < sidecarTrailerLen {
		return nil, nil, false
	}
	t := data[len(data)-sidecarTrailerLen:]
	if binary.LittleEndian.Uint32(t[8:]) != sidecarContainerMagic {
		return nil, nil, false
	}
	attrLen := int(binary.LittleEndian.Uint32(t[0:]))
	keyLen := int(binary.LittleEndian.Uint32(t[4:]))
	if attrLen < 0 || keyLen < 0 || attrLen+keyLen+sidecarTrailerLen != len(data) {
		return nil, nil, false
	}
	return data[:attrLen], data[attrLen : attrLen+keyLen], true
}

// writeFileAtomic writes b to path via a temp file + fsync + rename, so a crash never
// leaves a torn sidecar.
func writeFileAtomic(path string, b []byte) error {
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
