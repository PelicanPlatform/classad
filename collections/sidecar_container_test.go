package collections

import (
	"bytes"
	"encoding/binary"
	"math"
	"slices"
	"testing"
)

// TestSegmentSidecarContainer covers the sidecar container framing, including the v2 (3-section)
// layout that carries the columnar-accelerator blob and the backward-compatible parse of a v1
// (2-section, pre-columnar) container.
func TestSegmentSidecarContainer(t *testing.T) {
	attr := []byte("ARCX-attribute-index-blob")
	key := []byte("KIDX-key-index-blob-payload")
	col := []byte("COLX-columnar-block-payload-bytes")

	// v2: all three sections round-trip.
	c2 := buildSegmentSidecar(attr, key, col, nil, 0, 0, nil)
	if binary.LittleEndian.Uint32(c2[len(c2)-4:]) != sidecarContainerMagicV2 {
		t.Fatal("v2 container: wrong trailing magic")
	}
	a, k, cc, _, ok := splitSegmentSidecar(c2)
	if !ok || !bytes.Equal(a, attr) || !bytes.Equal(k, key) || !bytes.Equal(cc, col) {
		t.Fatalf("v2 split: ok=%v attr=%q key=%q col=%q", ok, a, k, cc)
	}

	// Empty col writes the v1 layout byte-for-byte (identical to a pre-columnar sidecar): trailing
	// magic is SCNT, length is attr+key+12, and split returns col == nil.
	c1 := buildSegmentSidecar(attr, key, nil, nil, 0, 0, nil)
	if binary.LittleEndian.Uint32(c1[len(c1)-4:]) != sidecarContainerMagic {
		t.Fatal("empty col should write the v1 magic")
	}
	if len(c1) != len(attr)+len(key)+sidecarTrailerLen {
		t.Fatalf("v1 length = %d, want %d", len(c1), len(attr)+len(key)+sidecarTrailerLen)
	}
	a, k, cc, _, ok = splitSegmentSidecar(c1)
	if !ok || !bytes.Equal(a, attr) || !bytes.Equal(k, key) || cc != nil {
		t.Fatalf("v1 split: ok=%v attr=%q key=%q col=%v (want col nil)", ok, a, k, cc)
	}

	// Empty attr (no attribute index) still frames correctly alongside a columnar block.
	c3 := buildSegmentSidecar(nil, key, col, nil, 0, 0, nil)
	a, k, cc, _, ok = splitSegmentSidecar(c3)
	if !ok || len(a) != 0 || !bytes.Equal(k, key) || !bytes.Equal(cc, col) {
		t.Fatalf("empty-attr split: ok=%v attr=%q key=%q col=%q", ok, a, k, cc)
	}

	// Malformed: too short, wrong magic, and inconsistent lengths all fail cleanly (no panic).
	for _, bad := range [][]byte{
		nil,
		[]byte("short"),
		append(append([]byte{}, c2...), 0x00), // trailing byte breaks the length invariant
		func() []byte { b := append([]byte{}, c2...); b[len(b)-1] ^= 0xFF; return b }(), // magic corrupted
		func() []byte { // v2 with an over-large colLen
			b := append([]byte{}, c2...)
			binary.LittleEndian.PutUint32(b[len(b)-8:], 1<<30)
			return b
		}(),
	} {
		if _, _, _, _, ok := splitSegmentSidecar(bad); ok {
			t.Errorf("malformed container parsed as ok: %q", bad)
		}
	}
}

// TestSidecarContainerZones covers the v3 section: a zone map round-trips, and the three
// older container shapes still parse (a sidecar written before v3 must keep working, since
// the whole point is that a missing zone section falls back to recomputation rather than
// failing).
func TestSidecarContainerZones(t *testing.T) {
	attr := []byte("ATTRBLOB")
	key := []byte("KEYBLOB!")
	col := []byte("COLBLOB!")
	zones := map[uint32]zoneRange{7: {Min: -1.5, Max: 100}, 3: {Min: 0, Max: 0}}
	names := map[uint32]string{7: "Memory", 3: "Cpus"}
	nameOf := func(id uint32) (string, bool) { n, ok := names[id]; return n, ok }
	idOf := func(name string) (uint32, bool) {
		for id, n := range names {
			if n == name {
				return id, true
			}
		}
		return 0, false
	}

	c4 := buildSegmentSidecar(attr, key, col, buildZoneBlob(zones, nameOf), 4096, 0, nil)
	a, k, cc, z, ok := splitSegmentSidecar(c4)
	if !ok {
		t.Fatal("v3 container did not parse")
	}
	if string(a) != string(attr) || string(k) != string(key) || string(cc) != string(col) {
		t.Fatalf("sections mangled: attr=%q key=%q col=%q", a, k, cc)
	}
	got, st := parseZoneBlob(z, idOf)
	if st != zoneBlobOK {
		t.Fatal("zone section did not parse")
	}
	if len(got) != len(zones) {
		t.Fatalf("zones = %v, want %v", got, zones)
	}
	for id, want := range zones {
		if got[id] != want {
			t.Errorf("zone %d = %v, want %v", id, got[id], want)
		}
	}

	// No zones: stays at the older shape, and yields a nil zone section.
	if _, _, _, z2, ok := splitSegmentSidecar(buildSegmentSidecar(attr, key, col, nil, 0, 0, nil)); !ok || z2 != nil {
		t.Errorf("container without zones: ok=%v zone=%v, want ok and nil", ok, z2)
	}

	// A malformed zone section must be rejected, not half-decoded: a wrong [min,max] would
	// silently prune segments that hold matches.
	good := buildZoneBlob(zones, nameOf)
	for _, bad := range [][]byte{{}, {1, 0, 0, 0}, append([]byte{1, 0, 0, 0}, make([]byte, 19)...),
		good[:len(good)-1], append(append([]byte(nil), good...), 0), good[:9]} {
		if _, st := parseZoneBlob(bad, idOf); st == zoneBlobOK {
			t.Errorf("parseZoneBlob(%v) accepted a malformed section", bad)
		}
	}
	// min > max is impossible for a real zone map and must be refused.
	bad := buildZoneBlob(map[uint32]zoneRange{7: {Min: 5, Max: 1}}, nameOf)
	if _, st := parseZoneBlob(bad, idOf); st == zoneBlobOK {
		t.Error("parseZoneBlob accepted min > max")
	}
}

// legacyZoneBlob is the first zone-section format, keyed by intern id.
func legacyZoneBlob(zones map[uint32]zoneRange) []byte {
	ids := make([]uint32, 0, len(zones))
	for id := range zones {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	b := binary.LittleEndian.AppendUint32(nil, uint32(len(ids)))
	for _, id := range ids {
		b = binary.LittleEndian.AppendUint32(b, id)
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(zones[id].Min))
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(zones[id].Max))
	}
	return b
}

// A zone section is bound to attributes by name: an id that means something else in this
// process cannot pick up another attribute's range, a name this process does not know is
// dropped, and an id-keyed (legacy) section is recognized and never trusted.
func TestZoneBlobByName(t *testing.T) {
	zones := map[uint32]zoneRange{4: {Min: 1, Max: 7}, 9: {Min: 1.7e9, Max: 1.8e9}}
	writer := map[uint32]string{4: "RequestMemory", 9: "CompletionDate"}
	blob := buildZoneBlob(zones, func(id uint32) (string, bool) { n, ok := writer[id]; return n, ok })
	// The reader's ids are not the writer's: CompletionDate is 4 here, RequestMemory 12, and
	// it has never heard of a third name.
	reader := map[string]uint32{"CompletionDate": 4, "RequestMemory": 12}
	got, st := parseZoneBlob(blob, func(n string) (uint32, bool) { id, ok := reader[n]; return id, ok })
	if st != zoneBlobOK || len(got) != 2 || got[4] != (zoneRange{Min: 1.7e9, Max: 1.8e9}) || got[12] != (zoneRange{Min: 1, Max: 7}) {
		t.Errorf("rebound zones = %v (%v)", got, st)
	}
	got, st = parseZoneBlob(blob, func(n string) (uint32, bool) { return 3, n == "CompletionDate" })
	if st != zoneBlobOK || len(got) != 1 || got[3].Min != 1.7e9 {
		t.Errorf("unknown name kept: %v", got)
	}
	if got, st := parseZoneBlob(legacyZoneBlob(zones), func(string) (uint32, bool) { return 0, true }); st != zoneBlobLegacy || got != nil {
		t.Errorf("legacy section: %v, %v", got, st)
	}
}
