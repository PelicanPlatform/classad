package collections

// Measurement pass: what does each kind of damage actually cost today?
// These print a census rather than asserting a number, so they describe behavior
// without freezing it -- a fix should change the log, not break the test.

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCorruptionCensus damages one thing per sub-test and reports the survivors.
func TestCorruptionCensus(t *testing.T) {
	t.Run("torn tail (last record of last segment)", func(t *testing.T) {
		s := buildCorruptStore(t, 400)
		segs := s.segments()
		last := segs[len(segs)-1]
		offs := recordOffsets(t, last)
		flipBit(t, last, offs[len(offs)-1]+recKeyOff+2) // inside the key: CRC-covered
		s.report("torn tail")
	})

	t.Run("mid-file record (first sealed segment)", func(t *testing.T) {
		s := buildCorruptStore(t, 400)
		seg := s.segments()[0]
		offs := recordOffsets(t, seg)
		mid := offs[len(offs)/2]
		t.Logf("damaging record %d/%d at offset %d of %s", len(offs)/2, len(offs), mid, filepath.Base(seg))
		flipBit(t, seg, mid+recKeyOff+2)
		s.report("mid-file record")
	})

	t.Run("supersededBySeq (not CRC-covered)", func(t *testing.T) {
		s := buildCorruptStore(t, 400)
		seg := s.segments()[0]
		offs := recordOffsets(t, seg)
		mid := offs[len(offs)/2]
		flipBit(t, seg, mid+recSupOff+7) // high byte: makes a live record look superseded
		s.report("supersededBySeq bit")
	})

	t.Run("next pointer (not CRC-covered)", func(t *testing.T) {
		s := buildCorruptStore(t, 400)
		seg := s.segments()[0]
		offs := recordOffsets(t, seg)
		mid := offs[len(offs)/2]
		flipBit(t, seg, mid+recNextOffOff+1)
		s.report("next pointer bit")
	})

	t.Run("attribute sidecar .idx", func(t *testing.T) {
		s := buildCorruptStore(t, 400)
		idx := s.segments()[0] + ".idx"
		if _, err := os.Stat(idx); err != nil {
			t.Skipf("no .idx sidecar written: %v", err)
		}
		flipBit(t, idx, 1) // inside the magic
		s.report("sidecar .idx magic")
	})

	t.Run("dir snapshot", func(t *testing.T) {
		s := buildCorruptStore(t, 400)
		snap := filepath.Join(filepath.Dir(s.segments()[0]), dirSnapName)
		if _, err := os.Stat(snap); err != nil {
			t.Skipf("no dir snapshot: %v", err)
		}
		flipBit(t, snap, 1)
		s.report("dir.snap magic")
	})

	t.Run("segment file renamed", func(t *testing.T) {
		s := buildCorruptStore(t, 400)
		seg := s.segments()[0]
		renamed := filepath.Join(filepath.Dir(seg), "segment-1.d0.dat")
		if err := os.Rename(seg, renamed); err != nil {
			t.Fatal(err)
		}
		t.Logf("renamed %s -> %s", filepath.Base(seg), filepath.Base(renamed))
		s.report("segment renamed")
	})

	t.Run("mid-file record, NO sidecar", func(t *testing.T) {
		s := buildCorruptStore(t, 400)
		seg := s.segments()[0]
		// Drop the sidecar so recovery must walk and CRC-verify, instead of taking
		// the recorded extent. This is the path that stops at the first bad record.
		if err := os.Remove(seg + ".idx"); err != nil {
			t.Fatal(err)
		}
		offs := recordOffsets(t, seg)
		mid := offs[len(offs)/2]
		t.Logf("damaging record %d/%d of %s (no sidecar)", len(offs)/2, len(offs), filepath.Base(seg))
		flipBit(t, seg, mid+recKeyOff+2)
		s.report("mid-file, no sidecar")
	})

	t.Run("segment truncated to 60%", func(t *testing.T) {
		s := buildCorruptStore(t, 400)
		seg := s.segments()[0]
		st, err := os.Stat(seg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(seg, st.Size()*6/10); err != nil {
			t.Fatal(err)
		}
		s.report("segment truncated")
	})
}
