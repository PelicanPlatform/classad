package collections

import (
	"bytes"
	"encoding/binary"
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
	c2 := buildSegmentSidecar(attr, key, col)
	if binary.LittleEndian.Uint32(c2[len(c2)-4:]) != sidecarContainerMagicV2 {
		t.Fatal("v2 container: wrong trailing magic")
	}
	a, k, cc, ok := splitSegmentSidecar(c2)
	if !ok || !bytes.Equal(a, attr) || !bytes.Equal(k, key) || !bytes.Equal(cc, col) {
		t.Fatalf("v2 split: ok=%v attr=%q key=%q col=%q", ok, a, k, cc)
	}

	// Empty col writes the v1 layout byte-for-byte (identical to a pre-columnar sidecar): trailing
	// magic is SCNT, length is attr+key+12, and split returns col == nil.
	c1 := buildSegmentSidecar(attr, key, nil)
	if binary.LittleEndian.Uint32(c1[len(c1)-4:]) != sidecarContainerMagic {
		t.Fatal("empty col should write the v1 magic")
	}
	if len(c1) != len(attr)+len(key)+sidecarTrailerLen {
		t.Fatalf("v1 length = %d, want %d", len(c1), len(attr)+len(key)+sidecarTrailerLen)
	}
	a, k, cc, ok = splitSegmentSidecar(c1)
	if !ok || !bytes.Equal(a, attr) || !bytes.Equal(k, key) || cc != nil {
		t.Fatalf("v1 split: ok=%v attr=%q key=%q col=%v (want col nil)", ok, a, k, cc)
	}

	// Empty attr (no attribute index) still frames correctly alongside a columnar block.
	c3 := buildSegmentSidecar(nil, key, col)
	a, k, cc, ok = splitSegmentSidecar(c3)
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
		if _, _, _, ok := splitSegmentSidecar(bad); ok {
			t.Errorf("malformed container parsed as ok: %q", bad)
		}
	}
}
