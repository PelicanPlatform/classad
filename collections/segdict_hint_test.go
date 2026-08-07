package collections

import (
	"encoding/binary"
	"fmt"
	"os"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// internedArchive builds a persistent append-only collection whose sealed segments are
// interned (so they carry dictionaries), and returns it after a reopen.
func internedArchive(t *testing.T, dir string) *Collection {
	t.Helper()
	c, err := Open(Options{AppendOnly: true, Dir: dir, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3000 {
		ad, err := classad.Parse(fmt.Sprintf(
			`[ ClusterId=%d; Owner="user%d"; JobStatus=%d; Cmd="/bin/sleep" ]`, i, i%7, i%6))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.RetrainDict(1024); err != nil { // interns the sealed segments
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c2, err := Open(Options{AppendOnly: true, Dir: dir, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c2.Close() })
	return c2
}

func internedSegments(c *Collection) []*segment {
	var out []*segment
	for _, s := range c.shards[0].segs {
		if s != nil && s.dict.Load() != nil {
			out = append(out, s)
		}
	}
	return out
}

// TestSegDictHintMatchesWalk: the offset recorded in the sidecar must be the one the walk
// would have found. If it were not, every attribute name in the segment would resolve
// through the wrong dictionary.
func TestSegDictHintMatchesWalk(t *testing.T) {
	c := internedArchive(t, t.TempDir())
	segs := internedSegments(c)
	if len(segs) == 0 {
		t.Fatal("fixture produced no interned segments")
	}
	for _, seg := range segs {
		hinted := seg.dict.Load().rec
		seg.dict.Store(nil)
		publishSegDictAt(seg, 0) // force the walk
		walked := seg.dict.Load()
		if walked == nil {
			t.Fatalf("walk found no dict in an interned segment")
		}
		if walked.rec != hinted {
			t.Errorf("hint offset %d != walked offset %d", hinted, walked.rec)
		}
	}
}

// TestSegDictHintRejectsBadOffset is the safety property. A hint is data read off disk, so it
// can be wrong -- stale, truncated, or from another segment. An unverified hint would publish
// arbitrary bytes as the dictionary and silently corrupt every name lookup in the segment,
// which is far worse than the walk it replaces. Every bad hint must fall back and still land
// on the real dictionary.
func TestSegDictHintRejectsBadOffset(t *testing.T) {
	c := internedArchive(t, t.TempDir())
	segs := internedSegments(c)
	if len(segs) == 0 {
		t.Fatal("fixture produced no interned segments")
	}
	seg := segs[0]
	real := seg.dict.Load().rec
	if real == 0 {
		t.Fatal("dict record at offset 0; the test cannot distinguish hint from fallback")
	}

	for _, tc := range []struct {
		name string
		hint uint32
	}{
		{"past the written extent", uint32(seg.used) + 1},
		{"way out of bounds", 1 << 30},
		{"a real record that is not the dict", 0}, // offset 0 is the first DATA record
		{"mid-record, not a record boundary", real + 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hint := tc.hint
			if tc.name == "a real record that is not the dict" {
				// Offset 0 is a data record; publishSegDictAt treats hint 0 as "no hint",
				// so use the second record instead to exercise a non-zero wrong offset.
				hint = recTotalLen(seg.data, 0)
				if hint == 0 || recIsDict(seg.data, hint) {
					t.Skip("fixture layout unsuitable")
				}
			}
			seg.dict.Store(nil)
			publishSegDictAt(seg, hint)
			got := seg.dict.Load()
			if got == nil {
				t.Fatalf("bad hint %d left the segment with no dict at all", hint)
			}
			if got.rec != real {
				t.Errorf("bad hint %d published dict at %d, want the real one at %d",
					hint, got.rec, real)
			}
			// And the dictionary actually resolves: a wrong base would give nonsense.
			if n := got.count(); n == 0 || n > 1000 {
				t.Errorf("dict published from bad hint %d has implausible count %d", hint, n)
			}
		})
	}
}

// TestSegDictNamesResolveAfterReopen is the end-to-end guard: whichever path published the
// dictionary, a reopened interned segment must decode its records to the right names.
func TestSegDictNamesResolveAfterReopen(t *testing.T) {
	c := internedArchive(t, t.TempDir())
	n := 0
	seen := map[string]bool{}
	for ad := range c.Scan() {
		n++
		if v, ok := ad.Lookup("Owner"); ok {
			seen[fmt.Sprint(v)] = true
		}
	}
	if n != 3000 {
		t.Errorf("scanned %d records after reopen, want 3000", n)
	}
	if len(seen) != 7 {
		t.Errorf("distinct Owner values = %d, want 7 (names resolved through the dict)", len(seen))
	}
}

// TestSegDictFallsBackToWalkOnOldSidecar: a sidecar written by an earlier version carries no
// dictionary offset. Recovery must still find the dictionary, just by walking.
func TestSegDictFallsBackToWalkOnOldSidecar(t *testing.T) {
	dir := t.TempDir()
	c := internedArchive(t, dir)
	segs := internedSegments(c)
	if len(segs) == 0 {
		t.Fatal("fixture produced no interned segments")
	}
	paths := make([]string, 0, len(segs))
	for _, s := range segs {
		paths = append(paths, s.path+".idx")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Rewrite each sidecar in the v3 layout (no dict offset), as an older writer produced.
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		attr, key, col, zone, used, _, ok := splitSegmentSidecarV4(data)
		if !ok {
			continue
		}
		if err := os.WriteFile(p, buildV3Sidecar(attr, key, col, zone, used), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	c2, err := Open(Options{AppendOnly: true, Dir: dir, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if got := len(internedSegments(c2)); got != len(segs) {
		t.Errorf("interned segments after v3 reopen = %d, want %d", got, len(segs))
	}
	n := 0
	for range c2.Scan() {
		n++
	}
	if n != 3000 {
		t.Errorf("scanned %d records from v3 sidecars, want 3000", n)
	}
}

// buildV3Sidecar writes the pre-v4 container layout, for the compatibility test above.
func buildV3Sidecar(attrBlob, keyBlob, colBlob, zoneBlob []byte, segUsed int) []byte {
	b := make([]byte, 0, len(attrBlob)+len(keyBlob)+len(colBlob)+len(zoneBlob)+sidecarTrailerLenV3)
	b = append(b, attrBlob...)
	b = append(b, keyBlob...)
	b = append(b, colBlob...)
	b = append(b, zoneBlob...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(attrBlob)))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(keyBlob)))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(colBlob)))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(zoneBlob)))
	b = binary.LittleEndian.AppendUint64(b, uint64(segUsed))
	b = binary.LittleEndian.AppendUint32(b, sidecarContainerMagicV3)
	return b
}

// TestSegDictOffsetIsRecorded: the sidecar of an interned segment must actually carry the
// dictionary offset, or the fast path is dead code and reopen silently keeps walking.
func TestSegDictOffsetIsRecorded(t *testing.T) {
	c := internedArchive(t, t.TempDir())
	segs := internedSegments(c)
	if len(segs) == 0 {
		t.Fatal("fixture produced no interned segments")
	}
	for _, seg := range segs {
		_, dictRec := readSidecarTrailer(seg.path + ".idx")
		if dictRec == 0 {
			t.Errorf("segment %s: sidecar records no dict offset", seg.path)
			continue
		}
		if want := seg.dict.Load().rec; dictRec != want {
			t.Errorf("segment %s: sidecar dict offset %d, live dict at %d", seg.path, dictRec, want)
		}
	}
}
