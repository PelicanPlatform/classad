package collections

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepairRecoversWhatOpenDrops is the whole point. Open's walk stops at the first
// bad record, so a mid-file bit flip in a segment without a sidecar costs every
// record after it. Repair must give those back -- all but the one that is genuinely
// corrupt.
func TestRepairRecoversWhatOpenDrops(t *testing.T) {
	s := buildCorruptStore(t, 400)
	seg := s.segments()[0]
	if err := os.Remove(seg + ".idx"); err != nil { // force Open onto the walk path
		t.Fatal(err)
	}
	offs := recordOffsets(t, seg)
	flipBit(t, seg, offs[len(offs)/2]+recKeyOff+2)

	beforeFound, _, beforeWrong, err := s.survivors()
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeWrong) != 0 {
		t.Fatalf("damaged store served %d wrong values before repair", len(beforeWrong))
	}
	t.Logf("a plain open recovers %d/%d keys", len(beforeFound), len(s.keys))

	rep, err := Repair(s.dir, RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + rep.String())

	afterFound, afterMissing, afterWrong, err := s.survivors()
	if err != nil {
		t.Fatalf("reopen after repair: %v", err)
	}
	t.Logf("after repair: %d/%d keys", len(afterFound), len(s.keys))

	if len(afterWrong) != 0 {
		t.Fatalf("repair produced %d keys with WRONG contents: %v", len(afterWrong), afterWrong[:min(len(afterWrong), 5)])
	}
	if len(afterFound) <= len(beforeFound) {
		t.Fatalf("repair recovered nothing: %d keys before, %d after", len(beforeFound), len(afterFound))
	}
	// Exactly one record was corrupted, so exactly one key should still be missing.
	if len(afterMissing) != 1 {
		t.Errorf("want exactly 1 key still missing (the corrupted record), got %d: %v",
			len(afterMissing), afterMissing[:min(len(afterMissing), 5)])
	}
	t.Logf("repair recovered %d keys that a plain open had dropped", len(afterFound)-len(beforeFound))
}

// TestRepairKeepsTheOriginal: an operator who disagrees with what repair kept needs
// the bytes to argue with. Destroying the evidence during recovery is not acceptable.
func TestRepairKeepsTheOriginal(t *testing.T) {
	s := buildCorruptStore(t, 400)
	seg := s.segments()[0]
	_ = os.Remove(seg + ".idx")
	before, err := os.ReadFile(seg)
	if err != nil {
		t.Fatal(err)
	}
	offs := recordOffsets(t, seg)
	flipBit(t, seg, offs[len(offs)/2]+recKeyOff+2)
	damaged, err := os.ReadFile(seg)
	if err != nil {
		t.Fatal(err)
	}

	rep, err := Repair(s.dir, RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Segments) != 1 {
		t.Fatalf("want 1 segment repaired, got %d", len(rep.Segments))
	}
	q := rep.Segments[0].Quarantined
	if q == "" {
		t.Fatal("repair did not record a quarantine path")
	}
	got, err := os.ReadFile(q)
	if err != nil {
		t.Fatalf("quarantined original is not readable: %v", err)
	}
	if string(got) != string(damaged) {
		t.Error("the quarantined file is not the original bytes")
	}
	if string(got) == string(before) {
		t.Error("test flaw: the damage never reached the file")
	}
}

// TestRepairRemovesOffsetDerivedState: rewriting a segment moves every record, so the
// index sidecar and the shard's directory snapshot now describe positions that hold
// something else. Both must be removed, not left to be trusted.
func TestRepairRemovesOffsetDerivedState(t *testing.T) {
	s := buildCorruptStore(t, 400)
	seg := s.segments()[0]
	shardDir := filepath.Dir(seg)
	offs := recordOffsets(t, seg)
	flipBit(t, seg, offs[len(offs)/2]+recKeyOff+2)

	// Both exist going in (the store was closed cleanly).
	if _, err := os.Stat(seg + ".idx"); err != nil {
		t.Skipf("no sidecar to test with: %v", err)
	}
	snap := filepath.Join(shardDir, dirSnapName)
	if _, err := os.Stat(snap); err != nil {
		t.Skipf("no dir snapshot to test with: %v", err)
	}

	if _, err := Repair(s.dir, RepairOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(seg + ".idx"); !os.IsNotExist(err) {
		t.Error("the stale index sidecar survived the repair")
	}
	if _, err := os.Stat(snap); !os.IsNotExist(err) {
		t.Error("the stale directory snapshot survived the repair")
	}
	// And the store still reads correctly with both rebuilt from scratch.
	found, _, wrong, err := s.survivors()
	if err != nil {
		t.Fatal(err)
	}
	if len(wrong) != 0 {
		t.Fatalf("after repair, %d keys read back WRONG: %v", len(wrong), wrong[:min(len(wrong), 5)])
	}
	// The real consequence, not just tidiness: a stale sidecar records an extent that
	// predates the rewrite, so Open truncates the segment right back and the repaired
	// records are lost again.
	if len(found) != len(s.keys)-1 {
		t.Errorf("after repair %d/%d keys readable, want %d (all but the corrupt record); "+
			"a stale sidecar silently undoes the repair", len(found), len(s.keys), len(s.keys)-1)
	}
	t.Logf("%d/%d keys read correctly with sidecar and snapshot rebuilt", len(found), len(s.keys))
}

// TestRepairLeavesATornTailAlone: a half-written record at the end is an interrupted
// write, not corruption. Open already handles it correctly, so repair must not churn
// the file.
func TestRepairLeavesATornTailAlone(t *testing.T) {
	s := buildCorruptStore(t, 400)
	segs := s.segments()
	last := segs[len(segs)-1]
	offs := recordOffsets(t, last)
	flipBit(t, last, offs[len(offs)-1]+recKeyOff+2)

	rep, err := Repair(s.dir, RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + rep.String())
	for _, sg := range rep.Segments {
		if filepath.Base(sg.Path) == filepath.Base(last) {
			t.Errorf("repair rewrote a segment whose only damage is at the tail: %s", sg.Path)
		}
	}
}

// TestRepairIsCleanOnAHealthyStore is the control: repair must do nothing at all to a
// store with no damage.
func TestRepairIsCleanOnAHealthyStore(t *testing.T) {
	s := buildCorruptStore(t, 400)
	before := snapshotTree(t, s.dir)

	rep, err := Repair(s.dir, RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Segments) != 0 {
		t.Errorf("repair rewrote %d segments of a healthy store", len(rep.Segments))
	}
	after := snapshotTree(t, s.dir)
	if len(before) != len(after) {
		t.Fatalf("repair changed the file set: %d -> %d", len(before), len(after))
	}
	for p, sum := range before {
		if after[p] != sum {
			t.Errorf("repair modified %s on a healthy store", p)
		}
	}
}

// TestRepairDryRunWritesNothing: a dry run must be a faithful rehearsal, reporting
// the same counts while leaving every byte alone.
func TestRepairDryRunWritesNothing(t *testing.T) {
	s := buildCorruptStore(t, 400)
	seg := s.segments()[0]
	_ = os.Remove(seg + ".idx")
	offs := recordOffsets(t, seg)
	flipBit(t, seg, offs[len(offs)/2]+recKeyOff+2)

	before := snapshotTree(t, s.dir)
	dry, err := Repair(s.dir, RepairOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	after := snapshotTree(t, s.dir)
	for p, sum := range before {
		if after[p] != sum {
			t.Errorf("dry run modified %s", p)
		}
	}
	if len(after) != len(before) {
		t.Errorf("dry run changed the file set: %d -> %d", len(before), len(after))
	}
	if !strings.Contains(dry.String(), "would repair") {
		t.Error("a dry-run report should say what it WOULD do")
	}

	// The rehearsal must match the performance.
	wet, err := Repair(s.dir, RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dk, dd := dry.Totals()
	wk, wd := wet.Totals()
	if dk != wk || dd != wd {
		t.Errorf("dry run predicted kept=%d dropped=%d; the real run did kept=%d dropped=%d", dk, dd, wk, wd)
	}
}

// TestRepairIsIdempotent: running repair twice must not rewrite anything the second
// time. A repair that keeps finding work churns the store and fills quarantine/ on
// every run, which an operator would reasonably automate and then regret.
func TestRepairIsIdempotent(t *testing.T) {
	s := buildCorruptStore(t, 400)
	seg := s.segments()[0]
	_ = os.Remove(seg + ".idx")
	offs := recordOffsets(t, seg)
	flipBit(t, seg, offs[len(offs)/2]+recKeyOff+2)

	first, err := Repair(s.dir, RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Segments) == 0 {
		t.Fatal("the first repair found nothing to do; the test proves nothing")
	}

	second, err := Repair(s.dir, RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Segments) != 0 {
		t.Errorf("a second repair rewrote %d segment(s); repair should converge\n%s",
			len(second.Segments), second.String())
	}
}

// TestRepairSurvivesATotallyDestroyedSegment: when nothing in a segment verifies, the
// result is an empty segment rather than an error or a crash, and the OTHER segments
// must still come back.
func TestRepairSurvivesATotallyDestroyedSegment(t *testing.T) {
	s := buildCorruptStore(t, 400)
	seg := s.segments()[0]
	_ = os.Remove(seg + ".idx")

	// Overwrite the whole written extent with noise.
	offs := recordOffsets(t, seg)
	end := offs[len(offs)-1]
	f, err := os.OpenFile(seg, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	noise := make([]byte, end)
	for i := range noise {
		noise[i] = byte(i*31 + 7)
	}
	if _, err := f.WriteAt(noise, 0); err != nil {
		t.Fatal(err)
	}
	f.Close()

	rep, err := Repair(s.dir, RepairOptions{})
	if err != nil {
		t.Fatalf("repair failed on a destroyed segment: %v", err)
	}
	t.Log("\n" + rep.String())

	found, _, wrong, err := s.survivors()
	if err != nil {
		t.Fatalf("reopen after repair: %v", err)
	}
	if len(wrong) != 0 {
		t.Fatalf("a destroyed segment produced %d keys with WRONG contents: %v",
			len(wrong), wrong[:min(len(wrong), 5)])
	}
	// The other two segments' keys must survive.
	if len(found) == 0 {
		t.Fatal("one destroyed segment cost the whole store")
	}
	t.Logf("one segment destroyed; %d/%d keys still readable, none wrong", len(found), len(s.keys))
}
