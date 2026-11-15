package collections

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func sampleSections() sidecarSections {
	return sidecarSections{
		attr:    []byte("ATTRBLOB"),
		key:     []byte("KEYBLOB1"),
		col:     []byte("COL"),
		zone:    []byte("ZONEZONE"),
		damage:  []byte("DMG"),
		profile: []byte("PROFILE!"),
		used:    1 << 20,
		dictRec: 4242,
		colRec:  777,
	}
}

func TestSidecarV7RoundTrip(t *testing.T) {
	in := sampleSections()
	got := parseSidecar(buildSidecar(in))
	if !got.ok {
		t.Fatalf("round-tripped container does not parse")
	}
	for _, c := range []struct {
		name     string
		want, is []byte
	}{
		{"attr", in.attr, got.attr}, {"key", in.key, got.key}, {"col", in.col, got.col},
		{"zone", in.zone, got.zone}, {"damage", in.damage, got.damage}, {"profile", in.profile, got.profile},
	} {
		if !bytes.Equal(c.want, c.is) {
			t.Errorf("%s = %q, want %q", c.name, c.is, c.want)
		}
	}
	if got.used != in.used || got.dictRec != in.dictRec || got.colRec != in.colRec {
		t.Errorf("trailer = (%d,%d,%d), want (%d,%d,%d)",
			got.used, got.dictRec, got.colRec, in.used, in.dictRec, in.colRec)
	}
}

// The attribute section's roaring bitmaps are offset-sensitive and the shared ARCX parser
// expects them at 0, as in every container version before this one.
func TestSidecarV7AttrKeepsOffsetZero(t *testing.T) {
	raw := buildSidecar(sampleSections())
	if !bytes.HasPrefix(raw, []byte("ATTRBLOB")) {
		t.Errorf("attribute section is not at offset 0: container starts %q", raw[:8])
	}
}

// An empty section is omitted from the directory and reads back as absent. This is what
// keeps a collection with no columnar accelerator from paying for one.
func TestSidecarV7OmitsEmptySections(t *testing.T) {
	in := sidecarSections{key: []byte("KEYONLY"), used: 99}
	raw := buildSidecar(in)
	got := parseSidecar(raw)
	if !got.ok {
		t.Fatal("does not parse")
	}
	if got.attr != nil || got.col != nil || got.zone != nil || got.damage != nil || got.profile != nil {
		t.Errorf("absent sections came back non-nil: attr=%v col=%v zone=%v dmg=%v prof=%v",
			got.attr, got.col, got.zone, got.damage, got.profile)
	}
	// One section in the directory, nothing more.
	if want := len("KEYONLY") + sidecarDirEntryLen + sidecarTrailerLenV7; len(raw) != want {
		t.Errorf("container is %d bytes, want %d (one section)", len(raw), want)
	}
}

// THE point of the section table: a section written by a NEWER build survives this build
// parsing the container, changing one section, and writing it back. Without this a
// read-modify-write silently drops whatever it did not enumerate.
func TestSidecarV7PreservesUnknownSections(t *testing.T) {
	const futureID = 4095
	in := sampleSections()
	in.unknown = []sidecarUnknownSection{{id: futureID, body: []byte("FROM-THE-FUTURE")}}

	// Parse, change one section, rebuild -- what every rewriting path does.
	mid := parseSidecar(buildSidecar(in))
	if !mid.ok {
		t.Fatal("does not parse")
	}
	mid.key = []byte("REFLAGGED")
	out := parseSidecar(buildSidecar(mid))
	if !out.ok {
		t.Fatal("rebuilt container does not parse")
	}
	if len(out.unknown) != 1 || out.unknown[0].id != futureID {
		t.Fatalf("unknown section lost: %+v", out.unknown)
	}
	if !bytes.Equal(out.unknown[0].body, []byte("FROM-THE-FUTURE")) {
		t.Errorf("unknown section body = %q", out.unknown[0].body)
	}
	if !bytes.Equal(out.key, []byte("REFLAGGED")) {
		t.Errorf("key = %q, want the rewritten value", out.key)
	}
	if !bytes.Equal(out.zone, in.zone) {
		t.Errorf("zone = %q, want it carried through unchanged", out.zone)
	}
}

// A malformed container must be REFUSED, not sliced on a best guess: a zone map or a
// profile read from the wrong offsets is not detectably wrong to its reader, and both
// silently drop records -- from queries, and from retention.
func TestSidecarV7RefusesMalformed(t *testing.T) {
	good := buildSidecar(sampleSections())

	corrupt := map[string]func([]byte) []byte{
		"section length past the body": func(b []byte) []byte {
			c := append([]byte(nil), b...)
			// First directory entry's length field.
			dirAt := len(c) - sidecarTrailerLenV7 - 6*sidecarDirEntryLen
			binary.LittleEndian.PutUint32(c[dirAt+4:], 1<<30)
			return c
		},
		"directory does not cover the body": func(b []byte) []byte {
			c := append([]byte(nil), b...)
			dirAt := len(c) - sidecarTrailerLenV7 - 6*sidecarDirEntryLen
			binary.LittleEndian.PutUint32(c[dirAt+4:], 1) // attr shrinks; body no longer accounted for
			return c
		},
		"duplicate section id": func(b []byte) []byte {
			c := append([]byte(nil), b...)
			dirAt := len(c) - sidecarTrailerLenV7 - 6*sidecarDirEntryLen
			binary.LittleEndian.PutUint16(c[dirAt+sidecarDirEntryLen:], secAttr) // key claims to be attr
			return c
		},
		"absurd section count": func(b []byte) []byte {
			c := append([]byte(nil), b...)
			binary.LittleEndian.PutUint32(c[len(c)-8:], sidecarMaxSections+1)
			return c
		},
		"truncated below the directory": func(b []byte) []byte {
			return append([]byte(nil), b[:len(b)-sidecarTrailerLenV7-4]...)
		},
	}

	for name, break_ := range corrupt {
		t.Run(name, func(t *testing.T) {
			got := parseSidecar(break_(good))
			if got.ok {
				t.Errorf("accepted a corrupt container: %+v", got)
			}
			if got.zone != nil || got.profile != nil {
				t.Errorf("yielded sections from a corrupt container: zone=%q profile=%q", got.zone, got.profile)
			}
		})
	}
}

// Containers written by older builds are on disk and are never rewritten unless merged, so
// every earlier version must still read. Built here by hand from the documented layout
// rather than by calling our own writer: the guarantee is against the FORMAT.
func TestSidecarReadsLegacyContainers(t *testing.T) {
	attr, key, col, zone, dmg := []byte("A"), []byte("KK"), []byte("CCC"), []byte("ZZZZ"), []byte("D")

	v6 := func() []byte {
		var b []byte
		b = append(b, attr...)
		b = append(b, key...)
		b = append(b, col...)
		b = append(b, zone...)
		b = append(b, dmg...)
		for _, n := range []int{len(attr), len(key), len(col), len(zone), len(dmg)} {
			b = binary.LittleEndian.AppendUint32(b, uint32(n))
		}
		b = binary.LittleEndian.AppendUint64(b, 4096)
		b = binary.LittleEndian.AppendUint32(b, 11) // dictRec
		b = binary.LittleEndian.AppendUint32(b, 22) // colRec
		return binary.LittleEndian.AppendUint32(b, sidecarContainerMagicV6)
	}()

	v3 := func() []byte {
		var b []byte
		b = append(b, attr...)
		b = append(b, key...)
		b = append(b, col...)
		b = append(b, zone...)
		for _, n := range []int{len(attr), len(key), len(col), len(zone)} {
			b = binary.LittleEndian.AppendUint32(b, uint32(n))
		}
		b = binary.LittleEndian.AppendUint64(b, 2048)
		return binary.LittleEndian.AppendUint32(b, sidecarContainerMagicV3)
	}()

	for _, tc := range []struct {
		name       string
		raw        []byte
		used       int
		dict, colR uint32
		wantDamage bool
	}{
		{"v6", v6, 4096, 11, 22, true},
		{"v3", v3, 2048, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSidecar(tc.raw)
			if !got.ok {
				t.Fatalf("%s container does not parse", tc.name)
			}
			if !bytes.Equal(got.attr, attr) || !bytes.Equal(got.key, key) ||
				!bytes.Equal(got.col, col) || !bytes.Equal(got.zone, zone) {
				t.Errorf("sections wrong: attr=%q key=%q col=%q zone=%q", got.attr, got.key, got.col, got.zone)
			}
			if got.used != tc.used || got.dictRec != tc.dict || got.colRec != tc.colR {
				t.Errorf("trailer = (%d,%d,%d), want (%d,%d,%d)",
					got.used, got.dictRec, got.colRec, tc.used, tc.dict, tc.colR)
			}
			if gotDmg := len(got.damage) > 0; gotDmg != tc.wantDamage {
				t.Errorf("damage present = %v, want %v", gotDmg, tc.wantDamage)
			}
			// A legacy container has no profile: retention must read "nothing known",
			// never bytes borrowed from another section.
			if got.profile != nil {
				t.Errorf("legacy container yielded a profile: %q", got.profile)
			}
		})
	}
}

// clone must not alias the mapping: the rewriting paths unmap before they publish.
func TestSidecarSectionsCloneDoesNotAlias(t *testing.T) {
	raw := buildSidecar(sampleSections())
	cl := parseSidecar(raw).clone()
	for i := range raw {
		raw[i] = 0
	}
	if !bytes.Equal(cl.zone, []byte("ZONEZONE")) || !bytes.Equal(cl.profile, []byte("PROFILE!")) {
		t.Errorf("clone aliased the source: zone=%q profile=%q", cl.zone, cl.profile)
	}
}

// readSidecarTrailer is the third reader of the container -- recovery's, which reads the
// trailer and directory by ReadAt without mapping the file -- and it must refuse exactly
// what parseSidecar refuses. Were it more lenient, recovery would trust an extent (and skip
// the walk that verifies it) from a sidecar the open then declines to adopt.
func TestReadSidecarTrailerAgreesWithParseOnMalformed(t *testing.T) {
	in := sampleSections()
	in.damage = nil // the trailer reader decodes a damage section; the sample's is not one
	good := buildSidecar(in)
	path := filepath.Join(t.TempDir(), "seg.idx")

	if err := os.WriteFile(path, good, 0o644); err != nil {
		t.Fatal(err)
	}
	if f := readSidecarTrailer(path); f.used != in.used || f.dictRec != in.dictRec || f.colRec != in.colRec {
		t.Fatalf("well-formed container: facts = %+v, want used=%d dictRec=%d colRec=%d", f, in.used, in.dictRec, in.colRec)
	}

	// Two sections claiming one id. parseSidecar refuses it (TestSidecarV7RefusesMalformed);
	// the trailer reader has to as well.
	dup := append([]byte(nil), good...)
	dirAt := len(dup) - sidecarTrailerLenV7 - 5*sidecarDirEntryLen // five sections without damage
	binary.LittleEndian.PutUint16(dup[dirAt+sidecarDirEntryLen:], secAttr)
	if parseSidecar(dup).ok {
		t.Fatal("fixture: parseSidecar accepted the duplicate-id container")
	}
	if err := os.WriteFile(path, dup, 0o644); err != nil {
		t.Fatal(err)
	}
	if f := readSidecarTrailer(path); f.used != 0 || f.dictRec != 0 || f.colRec != 0 || f.damage != nil {
		t.Errorf("duplicate-id container: trailer reader returned %+v, parseSidecar refuses it", f)
	}
}
