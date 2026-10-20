package collections

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// legacyRepairPack is what Repair did to a segment before it left columnarized segments alone: copy
// every record that verifies, in order, to the front of a file of the same size, and drop the rest.
// It does not rewrite a columnar payload's offset map, so every record after a dropped one sits at
// its predecessor's old offset.
func legacyRepairPack(data []byte) []byte {
	kept := make([]byte, 0, len(data))
	for off := 0; off+recHeaderSize <= len(data); {
		total := int(recTotalLen(data, uint32(off)))
		framed := total > 0 && total%8 == 0 && off+total <= len(data)
		if framed && recVerifyCRC(data, uint32(off)) {
			kept = append(kept, data[off:off+total]...)
			off += total
			continue
		}
		if framed {
			off += total
			continue
		}
		next := repairResync(data, off)
		if next < 0 {
			break
		}
		off = next
	}
	out := make([]byte, len(data))
	copy(out, kept)
	return out
}

// fsckSegmentOf returns r's entry for path.
func fsckSegmentOf(t *testing.T, r *FsckReport, path string) FsckSegment {
	t.Helper()
	for i := range r.Segments {
		if r.Segments[i].Path == path {
			return r.Segments[i]
		}
	}
	t.Fatalf("fsck did not report %s", path)
	return FsckSegment{}
}

// TestFsckFindsRecordsPackedUnderColumnarPayload: a Repair from before columnarized segments were left
// alone packed a damaged columnarized segment's records without rewriting its payload's offset map.
// Every record after the dropped one then read its predecessor's row -- 260 keys came back as another
// key's ad -- and Fsck called the store undamaged, because every record verifies. Fsck must report the
// payload as not matching its records, and Open must refuse the payload so that no key reads wrong.
func TestFsckFindsRecordsPackedUnderColumnarPayload(t *testing.T) {
	base := t.TempDir()
	payloadFixture(t, base)
	orig := getEveryKey(t, payloadFixtureOpts(base), payloadFixtureAds)

	// No false positive on the healthy columnarized store.
	clean, err := Fsck(base)
	if err != nil {
		t.Fatal(err)
	}
	columnar := 0
	for _, s := range clean.Segments {
		if s.Columnar {
			columnar++
		}
		if s.ColumnarMismatch != "" {
			t.Errorf("healthy %s reported as mismatched: %s", filepath.Base(s.Path), s.ColumnarMismatch)
		}
	}
	if columnar == 0 {
		t.Fatal("no columnarized segment was checked")
	}

	dir := copyStore(t, base)
	target := firstColumnarSegment(t, dir)
	offs := recordOffsets(t, target)
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	dropped := string(recKey(data, uint32(offs[len(offs)/2])))
	flipBit(t, target, offs[len(offs)/2]+recKeyOff+2)
	if data, err = os.ReadFile(target); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(target, legacyRepairPack(data), 0o644); err != nil {
		t.Fatal(err)
	}
	// The old Repair removed the derived files that encode offsets.
	for _, p := range []string{target + ".idx", filepath.Join(filepath.Dir(target), dirSnapName)} {
		if err = os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	inSeg := keysOfSegment(t, target)

	r, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	seg := fsckSegmentOf(t, r, target)
	if seg.ColumnarMismatch == "" || seg.ColumnarMismatchRows == 0 {
		t.Fatalf("fsck does not report the payload as mismatched:\n%s", r.String())
	}
	t.Logf("%s", r.String())
	if out := r.String(); !strings.Contains(out, "COLUMNAR PAYLOAD DOES NOT MATCH ITS RECORDS") ||
		strings.Contains(out, "no damage found") {
		t.Errorf("the fsck report does not call the mismatch damage:\n%s", out)
	}
	original := snapshotFiles(t, dir)
	rep, err := Repair(dir, RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Segments) != 0 || len(rep.Columnarized) != 1 || rep.Columnarized[0] != target {
		t.Errorf("repair: rewrote %d, left alone %v; want nothing rewritten and %s left alone",
			len(rep.Segments), rep.Columnarized, target)
	}
	assertFilesUnchanged(t, original, "Repair")

	refused := ColNativeOffsetMismatches()
	c, err := Open(payloadFixtureOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if ColNativeOffsetMismatches() == refused {
		t.Error("open did not refuse the payload")
	}
	kc := checkEveryKey(t, c, orig)
	// Every key of the packed segment is unreadable and counted, except the one the old repair
	// dropped, which is no longer in the store at all.
	if len(kc.missing) != len(inSeg)+1 {
		t.Errorf("%d keys did not read, want the segment's %d and the dropped %s", len(kc.missing), len(inSeg), dropped)
	}
	if len(kc.uncounted) != 1 || kc.uncounted[0] != dropped {
		t.Errorf("uncounted misses %v, want only the dropped key %s", kc.uncounted, dropped)
	}
	n, skips := checkScanAds(t, c, orig)
	if n != len(orig)-len(inSeg)-1 || skips != int64(len(inSeg)) {
		t.Errorf("a scan returned %d ads and counted %d skips, want %d and %d",
			n, skips, len(orig)-len(inSeg)-1, len(inSeg))
	}
}

// flipBits XORs mask into the byte at off.
func flipBits(t *testing.T, path string, off int, mask byte) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[off] ^= mask
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// flipRecordBody flips a bit in the stored ad of the record at off, leaving its key intact so a
// read by key still reaches it.
func flipRecordBody(t *testing.T, path string, off int) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	flipBit(t, path, off+recKeyOff+len(recKey(b, uint32(off)))+6)
}

// TestRepairLeavesDamagedDictionaryAlone: every record of an interned segment is keyed by the
// segment's attribute dictionary record. Repair dropped a damaged one with the other bad records,
// leaving 563 records nothing could decode; and Open published a damaged one unverified, so a
// flipped bit in a name renamed that attribute in all 564 ads. Repair must leave the segment as it
// is, and Open must refuse the dictionary so that every key reads exactly or is counted missing.
func TestRepairLeavesDamagedDictionaryAlone(t *testing.T) {
	base := t.TempDir()
	buildInternedStore(t, base)
	orig := getEveryKey(t, internedStoreOpts(base), internedAds)

	cases := []struct {
		name string
		// damage damages the segment, given its record offsets and its dictionary record's offset.
		damage func(t *testing.T, path string, offs []int, dict int)
		// noSidecar removes the segment's sidecar, so neither Repair nor Open learns where the
		// dictionary is from it, and Open walks the records.
		noSidecar bool
	}{
		// A name in the dictionary: read through, it renames the attribute.
		{"name", func(t *testing.T, path string, _ []int, dict int) {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			name := segDictName(b, uint32(dict+recKeyOff+4), 1)
			at := bytes.Index(b[dict:], name)
			if len(name) == 0 || at < 0 {
				t.Fatal("no name to damage")
			}
			flipBit(t, path, dict+at)
		}, false},
		// The dictionary and a record: the record makes the segment a repair candidate, and the
		// rewrite dropped the dictionary with it.
		{"name+record", func(t *testing.T, path string, offs []int, dict int) {
			flipBit(t, path, dict+recKeyOff+4+40)
			flipRecordBody(t, path, offs[len(offs)/2])
		}, false},
		// The dictionary's own flag: the record no longer says what it is, and the sidecar does.
		{"flag+record", func(t *testing.T, path string, offs []int, dict int) {
			flipBits(t, path, dict+recKeyLenOff+3, byte(dictFlag>>24))
			flipRecordBody(t, path, offs[len(offs)/2])
		}, false},
		// As name+record with no sidecar: the record's own flags are all Repair has to go on, and
		// Open's walk stops at the damaged record, short of the dictionary.
		{"name+record/no-sidecar", func(t *testing.T, path string, offs []int, dict int) {
			flipBit(t, path, dict+recKeyOff+4+40)
			flipRecordBody(t, path, offs[len(offs)/2])
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := copyStore(t, base)
			target, dict := firstInternedSegment(t, dir)
			inSeg := keysOfSegment(t, target)
			offs := recordOffsets(t, target)
			tc.damage(t, target, offs, dict)
			if tc.noSidecar {
				if err := os.Remove(target + ".idx"); err != nil {
					t.Fatal(err)
				}
			}

			r, err := Fsck(dir)
			if err != nil {
				t.Fatal(err)
			}
			seg := fsckSegmentOf(t, r, target)
			if !seg.DictDamaged || !segmentNeedsRepair(seg) {
				t.Fatalf("fsck does not report the dictionary as damaged:\n%s", r.String())
			}
			if !strings.Contains(r.String(), "DICTIONARY DAMAGED") {
				t.Errorf("the fsck report does not name the dictionary:\n%s", r.String())
			}
			original := snapshotFiles(t, dir)
			for _, dry := range []bool{true, false} {
				rep, rerr := Repair(dir, RepairOptions{DryRun: dry})
				if rerr != nil {
					t.Fatal(rerr)
				}
				if len(rep.Segments) != 0 || len(rep.DictDamaged) != 1 || rep.DictDamaged[0] != target {
					t.Errorf("dry=%v: repair rewrote %d, DictDamaged %v; want nothing rewritten and %s left alone",
						dry, len(rep.Segments), rep.DictDamaged, target)
				}
				if !strings.Contains(rep.String(), "LEFT ALONE "+filepath.Base(target)) {
					t.Errorf("dry=%v: the repair report does not name the segment:\n%s", dry, rep.String())
				}
			}
			assertFilesUnchanged(t, original, "Repair")
			if _, serr := os.Stat(filepath.Join(dir, quarantineDir)); serr == nil {
				t.Error("repair quarantined something")
			}

			unusable, pastBefore := SegDictsUnusable(), RecordsPastDamage()
			c, err := Open(internedStoreOpts(dir))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if SegDictsUnusable() == unusable {
				t.Error("open did not refuse the dictionary")
			}
			// Records past a stopped walk are not loaded and are counted at Open, not per read.
			past := int(RecordsPastDamage() - pastBefore)
			if tc.noSidecar != (past > 0) {
				t.Errorf("open counted %d records past damage", past)
			}
			loaded := len(inSeg) - past
			before := readSkipsOf("dict-unusable")
			kc := checkEveryKey(t, c, orig)
			if len(kc.uncounted) != past {
				t.Errorf("%d missing keys were not counted by their read, want the %d Open counted", len(kc.uncounted), past)
			}
			if len(kc.missing) != len(inSeg) {
				t.Errorf("%d keys did not read, want the segment's %d", len(kc.missing), len(inSeg))
			}
			if d := readSkipsOf("dict-unusable") - before; d < int64(loaded-1) {
				t.Errorf("%d dict-unusable reads counted, want at least %d", d, loaded-1)
			}
			n, skips := checkScanAds(t, c, orig)
			if n != len(orig)-len(inSeg) || skips != int64(loaded) {
				t.Errorf("a scan returned %d ads and counted %d skips, want %d and %d",
					n, skips, len(orig)-len(inSeg), loaded)
			}
			// A query matches each record before decoding it, and a record that cannot be decoded
			// must be counted there too, not dropped as a non-match.
			before = readSkipTotal()
			if got := len(readAll(t, c, "ProcId >= 0")); got != len(orig)-len(inSeg) {
				t.Errorf("a query returned %d ads, want %d", got, len(orig)-len(inSeg))
			}
			if d := readSkipTotal() - before; d != int64(loaded) {
				t.Errorf("a query counted %d skips, want %d", d, loaded)
			}
		})
	}
}

// TestRepairRefusesWhenDictionaryIsInLostFraming: when the record before a damaged dictionary loses
// its framing, the walk resyncs past the end without ever reaching the dictionary as a record, so its
// flags say nothing. The sidecar still records where it was: Fsck must report it damaged and Repair
// must not write the segment without it. Only Fsck and Repair run; neither changes anything here.
func TestRepairRefusesWhenDictionaryIsInLostFraming(t *testing.T) {
	dir := t.TempDir()
	buildInternedStore(t, dir)
	target, dict := firstInternedSegment(t, dir)
	offs := recordOffsets(t, target)
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	name := segDictName(b, uint32(dict+recKeyOff+4), 1)
	at := bytes.Index(b[dict:], name)
	if len(name) == 0 || at < 0 {
		t.Fatal("no name to damage")
	}
	flipBit(t, target, dict+at)
	// The keyed record just before the dictionary: an odd length no longer frames.
	prev := -1
	for _, o := range offs {
		if o < dict && !recIsMarker(b, uint32(o)) {
			prev = o
		}
	}
	flipBit(t, target, prev+recTotalLenOff)
	// And one ordinary bad record, so the damage is not all at the tail.
	flipRecordBody(t, target, offs[len(offs)/2])

	r, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	seg := fsckSegmentOf(t, r, target)
	if !seg.DictDamaged {
		t.Errorf("fsck does not report the dictionary as damaged:\n%s", r.String())
	}
	original := snapshotFiles(t, dir)
	for _, dry := range []bool{true, false} {
		rep, rerr := Repair(dir, RepairOptions{DryRun: dry})
		if rerr != nil {
			t.Fatal(rerr)
		}
		if len(rep.Segments) != 0 || len(rep.DictDamaged) != 1 || rep.DictDamaged[0] != target {
			t.Errorf("dry=%v: repair rewrote %d, DictDamaged %v; want nothing rewritten and %s left alone",
				dry, len(rep.Segments), rep.DictDamaged, target)
		}
	}
	assertFilesUnchanged(t, original, "Repair")
}
