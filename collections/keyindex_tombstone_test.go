package collections

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestKeyIndexFilesDeleteMarkersUnderTheirKey proves a sealed segment's key index can answer
// "was this key deleted here?" from the index alone.
//
// A delete marker is a keyless record, so v1 hashed its empty inline key and filed every marker
// in the store under one hash. The delete was in the index but unreachable from the key it
// deleted, which is the one question recovery has to answer without reading the record.
func TestKeyIndexFilesDeleteMarkersUnderTheirKey(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	key := []byte("gone")
	kept := []byte("stays")
	for _, k := range [][]byte{key, kept} {
		ad, err := classad.Parse(`[ A=1 ]`)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put(k, ad); err != nil {
			t.Fatal(err)
		}
	}
	if !c.Delete(key) {
		t.Fatal("delete reported nothing removed")
	}

	sh := c.shards[0]
	sh.mu.RLock()
	seg, used := sh.act, sh.act.used
	sh.mu.RUnlock()

	idx, err := parseKeyIndex(buildKeyIndex(seg.data, used, c.h, nil, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !idx.tombstonesUsable() {
		t.Fatal("a freshly built index should file delete markers under their key")
	}

	// The deleted key's marker is findable from the key, and flagged.
	marks := idx.lookupTombstones(c.h.Hash(key))
	if len(marks) != 1 {
		t.Fatalf("lookupTombstones(%q) = %d markers, want 1", key, len(marks))
	}
	if k, ok := recDeleteMarkerKey(seg.data, marks[0]); !ok || string(k) != string(key) {
		t.Fatalf("entry at off %d is not %q's delete marker (got %q, ok=%v)", marks[0], key, k, ok)
	}

	// A key that was never deleted has none, and the live key is unaffected.
	if n := len(idx.lookupTombstones(c.h.Hash(kept))); n != 0 {
		t.Fatalf("lookupTombstones(%q) = %d, want 0 for a key never deleted", kept, n)
	}

	// The read path is unchanged: lookup still returns only data records, so a key whose
	// marker now shares its hash does not hand callers an extra record to read and discard.
	for _, k := range [][]byte{key, kept} {
		for _, off := range idx.lookup(c.h.Hash(k)) {
			if recIsMarker(seg.data, off) {
				t.Fatalf("lookup(%q) returned a marker at off %d", k, off)
			}
		}
	}
	if n := len(idx.lookup(c.h.Hash(key))); n != 1 {
		t.Fatalf("lookup(%q) = %d data records, want the one put", key, n)
	}
}

// TestKeyIndexReadsV1Sidecar proves a sidecar written before delete markers were filed by key is
// still read, and reports that it cannot answer the tombstone question -- so an empty tombstone
// result from it is never mistaken for "this key was not deleted".
func TestKeyIndexReadsV1Sidecar(t *testing.T) {
	ents := []struct {
		hash uint64
		off  uint32
	}{{7, 0}, {9, 64}, {9, 128}}

	buf := make([]byte, 0, keyIdxHeaderLen+len(ents)*keyIdxEntryLen+4)
	buf = binary.LittleEndian.AppendUint32(buf, keyIdxMagic)
	buf = binary.LittleEndian.AppendUint16(buf, keyIdxVersionV1)
	buf = binary.LittleEndian.AppendUint16(buf, 0)
	buf = binary.LittleEndian.AppendUint32(buf, 4096)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(ents)))
	for _, e := range ents {
		buf = binary.LittleEndian.AppendUint64(buf, e.hash)
		buf = binary.LittleEndian.AppendUint32(buf, e.off)
	}
	buf = binary.LittleEndian.AppendUint32(buf, crc32.ChecksumIEEE(buf))

	idx, err := parseKeyIndex(buf)
	if err != nil {
		t.Fatalf("a v1 sidecar must still parse: %v", err)
	}
	if idx.tombstonesUsable() {
		t.Fatal("a v1 sidecar cannot answer the tombstone question and must not claim to")
	}
	if got := idx.lookup(9); len(got) != 2 || got[0] != 64 || got[1] != 128 {
		t.Fatalf("lookup(9) = %v, want both of the hash's records", got)
	}
	if got := idx.lookup(7); len(got) != 1 || got[0] != 0 {
		t.Fatalf("lookup(7) = %v, want [0]", got)
	}
	if got := idx.lookup(11); len(got) != 0 {
		t.Fatalf("lookup(11) = %v, want none", got)
	}
}

// TestKeyIndexDamageProofAcceptsDeleteMarkers guards the coupling that filing markers by key
// broke: keyIndexDamage derives a segment's damaged spans by checking that each index entry's
// hash matches the record at its offset. A marker is keyless, so checking its INLINE key would
// never match an entry filed under the key it deletes -- and the proof would be refused for
// every segment holding a delete, which is most of them. Nothing else here covers a healthy
// segment that contains one.
func TestKeyIndexDamageProofAcceptsDeleteMarkers(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	for i := range 8 {
		ad, err := classad.Parse(`[ A=1 ]`)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put(fmt.Appendf(nil, "k%d", i), ad); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 8; i += 3 { // several deletes, so several markers land in the segment
		if !c.Delete(fmt.Appendf(nil, "k%d", i)) {
			t.Fatalf("k%d: delete reported nothing removed", i)
		}
	}

	sh := c.shards[0]
	sh.mu.RLock()
	seg, used := sh.act, sh.act.used
	sh.mu.RUnlock()

	ki, err := parseKeyIndex(buildKeyIndex(seg.data, used, c.h, nil, 0))
	if err != nil {
		t.Fatal(err)
	}
	spans, ok := keyIndexDamage(seg.data, used, ki, c.h, newDamageSet(nil))
	if !ok {
		t.Fatal("keyIndexDamage refused the proof for an undamaged segment holding delete markers")
	}
	if len(spans) != 0 {
		t.Fatalf("keyIndexDamage reported %d damaged spans in an undamaged segment: %v", len(spans), spans)
	}
}
