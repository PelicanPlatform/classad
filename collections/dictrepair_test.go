package collections

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// swapDictFiles exchanges dicts/a.zst and dicts/b.zst, which is the shape an operator's
// mistake actually takes and the one case a straight rename cannot handle.
func swapDictFiles(t *testing.T, dir string, a, b uint32) {
	t.Helper()
	dicts := filepath.Join(dir, "dicts")
	pa := filepath.Join(dicts, fmt.Sprintf("%d.zst", a))
	pb := filepath.Join(dicts, fmt.Sprintf("%d.zst", b))
	tmp := filepath.Join(dicts, "swap.tmp")
	for _, step := range [][2]string{{pa, tmp}, {pb, pa}, {tmp, pb}} {
		if err := os.Rename(step[0], step[1]); err != nil {
			t.Fatal(err)
		}
	}
}

// TestFsckNamesAMislabelledDictionary: two dictionary files that swapped names are both
// valid dictionaries, both load, and every check that looks at one file in isolation says
// the store is fine. Only asking whether a file is the dictionary its NAME claims finds
// it -- which became possible when dictionaries started carrying their own id.
func TestFsckNamesAMislabelledDictionary(t *testing.T) {
	dir, _ := buildTwoDictStore(t)
	swapDictFiles(t, dir, 1, 2)

	r, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.DictsMislabelled) != 2 {
		t.Fatalf("DictsMislabelled = %+v; want both files", r.DictsMislabelled)
	}
	for _, m := range r.DictsMislabelled {
		if m.FiledAs == m.Names {
			t.Errorf("reported a file as mislabelled against its own id: %+v", m)
		}
	}
	if len(r.DictsDamaged) != 0 {
		t.Errorf("a misplaced dictionary was reported as damaged: %+v", r.DictsDamaged)
	}
	out := r.String()
	if !strings.Contains(out, "MISLABELLED") || !strings.Contains(out, "decode to nothing") {
		t.Errorf("the report does not say what it costs:\n%s", out)
	}
}

// TestRepairRefilesSwappedDictionaries is the repair that identity made possible. The
// swap is the hard case: moving 1 onto 2 while 2 is still there would either clobber it
// or refuse, and both are wrong when the two simply want to exchange places.
func TestRepairRefilesSwappedDictionaries(t *testing.T) {
	dir, _ := buildTwoDictStore(t)
	dicts := filepath.Join(dir, "dicts")
	want1, err := os.ReadFile(filepath.Join(dicts, "1.zst"))
	if err != nil {
		t.Fatal(err)
	}
	want2, err := os.ReadFile(filepath.Join(dicts, "2.zst"))
	if err != nil {
		t.Fatal(err)
	}
	swapDictFiles(t, dir, 1, 2)
	if _, oerr := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14}); !errors.Is(oerr, ErrDictIDMismatch) {
		t.Fatalf("control: Open gave %v; want it to refuse the swap", oerr)
	}

	rep, err := Repair(dir, RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + rep.String())
	if len(rep.Dicts) != 2 {
		t.Fatalf("Repair put %d dictionaries right; want 2", len(rep.Dicts))
	}
	for _, d := range rep.Dicts {
		if d.Action != "re-filed" {
			t.Errorf("dictionary %d: action %q; want re-filed", d.ID, d.Action)
		}
	}
	got1, err := os.ReadFile(filepath.Join(dicts, "1.zst"))
	if err != nil {
		t.Fatal(err)
	}
	got2, err := os.ReadFile(filepath.Join(dicts, "2.zst"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got1, want1) || !bytes.Equal(got2, want2) {
		t.Fatal("the files were not put back where they belong")
	}
	// No temporary left behind, and the store opens.
	entries, err := os.ReadDir(dicts)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".repair-") {
			t.Errorf("a repair temporary was left behind: %s", e.Name())
		}
	}
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatalf("Open after the repair: %v", err)
	}
	defer c.Close()
	if n := c.Len(); n == 0 {
		t.Error("no records came back")
	}
}

// TestRepairDryRunRefilesNothing: an operator facing a store that will not open needs to
// see what repair would do before it does it.
func TestRepairDryRunRefilesNothing(t *testing.T) {
	dir, _ := buildTwoDictStore(t)
	swapDictFiles(t, dir, 1, 2)
	before, err := os.ReadFile(filepath.Join(dir, "dicts", "1.zst"))
	if err != nil {
		t.Fatal(err)
	}

	rep, err := Repair(dir, RepairOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Dicts) == 0 {
		t.Error("the dry run reported nothing to do")
	}
	after, err := os.ReadFile(filepath.Join(dir, "dicts", "1.zst"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the dry run moved a dictionary")
	}
	if out := rep.String(); !strings.Contains(out, "WOULD RE-FILE") {
		t.Errorf("the dry run does not say what it would do:\n%s", out)
	}
}

// TestRepairLeavesACorrectlyFiledDictionaryAlone is the control, and the rule that keeps
// a repair from making things worse: a slot held by a dictionary that belongs there is
// never taken from it, and the stray goes back where it was rather than becoming litter.
func TestRepairLeavesACorrectlyFiledDictionaryAlone(t *testing.T) {
	dir, _ := buildTwoDictStore(t)
	dicts := filepath.Join(dir, "dicts")
	// A copy of dictionary 2 filed as 9, while 2 itself is correctly filed.
	two, err := os.ReadFile(filepath.Join(dicts, "2.zst"))
	if err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(dicts, "9.zst")
	if werr := os.WriteFile(stray, two, 0o644); werr != nil {
		t.Fatal(werr)
	}

	rep, err := Repair(dir, RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range rep.Dicts {
		if d.Action == "re-filed" && d.ID == 2 {
			t.Errorf("re-filed over a dictionary that was already in the right place: %+v", d)
		}
	}
	got, err := os.ReadFile(filepath.Join(dicts, "2.zst"))
	if err != nil || !bytes.Equal(got, two) {
		t.Error("the correctly filed dictionary was disturbed")
	}
	if _, serr := os.Stat(stray); serr != nil {
		t.Errorf("the stray was not put back where it was: %v", serr)
	}
	entries, _ := os.ReadDir(dicts)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".repair-") {
			t.Errorf("a repair temporary was left behind: %s", e.Name())
		}
	}
}

// TestRepairRestoresAMissingDictionaryFromABackup: the backup directory is written when
// the file at a dictionary's own path stops being it, and is the first place repair looks
// because it costs one readdir.
func TestRepairRestoresAMissingDictionaryFromABackup(t *testing.T) {
	dir, _ := buildTwoDictStore(t)
	dicts := filepath.Join(dir, "dicts")
	path := filepath.Join(dicts, "1.zst")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dicts, dictBackupDir, dictBackupName(1, 0xDEADBEEF))
	if mkErr := os.MkdirAll(filepath.Dir(backup), 0o755); mkErr != nil {
		t.Fatal(mkErr)
	}
	if werr := os.WriteFile(backup, want, 0o644); werr != nil {
		t.Fatal(werr)
	}
	if rmErr := os.Remove(path); rmErr != nil {
		t.Fatal(rmErr)
	}

	rep, err := Repair(dir, RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + rep.String())
	if len(rep.Dicts) != 1 || rep.Dicts[0].Action != "restored" || rep.Dicts[0].ID != 1 {
		t.Fatalf("Dicts = %+v; want dictionary 1 restored", rep.Dicts)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("dictionary 1 was not restored: %v", err)
	}
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatalf("Open after the repair: %v", err)
	}
	if n := c.Len(); n == 0 {
		t.Error("no records came back")
	}
	c.Close()
}

// TestRepairRefusesACopyThatNamesAnotherDictionary: every candidate must name the id it
// is being restored as. A copy that disagrees is reporting a confusion, and writing it
// would make the confusion permanent -- the same rule Open's sweep follows.
func TestRepairRefusesACopyThatNamesAnotherDictionary(t *testing.T) {
	dir, _ := buildTwoDictStore(t)
	dicts := filepath.Join(dir, "dicts")
	two, err := os.ReadFile(filepath.Join(dicts, "2.zst"))
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dicts, dictBackupDir, dictBackupName(1, 0x12345678))
	if mkErr := os.MkdirAll(filepath.Dir(backup), 0o755); mkErr != nil {
		t.Fatal(mkErr)
	}
	if werr := os.WriteFile(backup, two, 0o644); werr != nil { // dictionary 2, filed as a backup of 1
		t.Fatal(werr)
	}
	if rmErr := os.Remove(filepath.Join(dicts, "1.zst")); rmErr != nil {
		t.Fatal(rmErr)
	}

	rep, err := Repair(dir, RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Dicts) != 0 {
		t.Errorf("restored dictionary 1 from a copy that names 2: %+v", rep.Dicts)
	}
	if _, serr := os.Stat(filepath.Join(dicts, "1.zst")); serr == nil {
		t.Error("the wrong dictionary was written into dicts/1.zst")
	}
}

// TestRepairOnAHealthyStoreTouchesNoDictionary is the control for all of the above.
func TestRepairOnAHealthyStoreTouchesNoDictionary(t *testing.T) {
	dir, _ := buildTwoDictStore(t)
	rep, err := Repair(dir, RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Dicts) != 0 {
		t.Errorf("a healthy store had %d dictionaries put right: %+v", len(rep.Dicts), rep.Dicts)
	}
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatalf("Open after a no-op repair: %v", err)
	}
	c.Close()
}

// damageTrailer corrupts a carrier segment's trailer at an offset counted back from the
// end of the file, and returns the file's size so a caller can check it did not grow.
func damageTrailer(t *testing.T, path string, fromEnd int64) int64 {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	at := st.Size() - fromEnd
	if _, rerr := f.ReadAt(b[:], at); rerr != nil {
		t.Fatal(rerr)
	}
	b[0] ^= 0x01
	if _, werr := f.WriteAt(b[:], at); werr != nil {
		t.Fatal(werr)
	}
	return st.Size()
}

// carrierWithTrailer builds the carrier fixture, fills its replicas, and returns one
// segment that ended up carrying a copy.
func carrierWithTrailer(t *testing.T) (dir, segPath string) {
	t.Helper()
	dir = buildCarrierStore(t)
	c := openCarrier(t, dir, 1)
	if st := stateFor(t, c.EnsureDictReplicas(), 1); st.Copies == 0 {
		t.Skipf("fixture carries no copies: %v", st.Err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	names := trailerCarriers(t, dir, 1, 1)
	if len(names) == 0 {
		t.Skip("fixture produced no carrier")
	}
	return dir, filepath.Join(dir, "0", names[0])
}

// TestFsckSeesADamagedTrailer: "this segment has no trailer" and "this segment's trailer
// will not read" are the same answer to a reader and opposite facts to anyone asking after
// the store's health. Reporting damage as absence is how a store that is one deletion from
// losing a dictionary looks identical to one that never carried a copy.
func TestFsckSeesADamagedTrailer(t *testing.T) {
	dir, segPath := carrierWithTrailer(t)
	// 9 bytes from the end is inside the foot, past the magic: the directory's checksum
	// no longer matches, so nothing in the trailer can be located.
	damageTrailer(t, segPath, 9)

	r, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tr := range r.Trailers {
		if tr.Path == segPath {
			found = true
			if tr.Why == "" {
				t.Errorf("reported the trailer as section damage; want the whole trailer unreadable")
			}
		}
	}
	if !found {
		t.Fatalf("Fsck did not report the damaged trailer: %+v", r.Trailers)
	}
	if out := r.String(); !strings.Contains(out, "DAMAGED trailer") ||
		!strings.Contains(out, "records are unaffected") {
		t.Errorf("the report does not say what the damage costs:\n%s", out)
	}
	// The records are genuinely unaffected: the trailer lives past them.
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 20})
	if err != nil {
		t.Fatalf("a damaged trailer stopped the store opening: %v", err)
	}
	defer c.Close()
	if n := c.Len(); n == 0 {
		t.Error("no records came back")
	}
}

// TestFsckSeesOneDamagedSection: when the directory still verifies the damage is
// divisible, and fsck says which section rather than condemning the trailer.
func TestFsckSeesOneDamagedSection(t *testing.T) {
	dir, segPath := carrierWithTrailer(t)
	tr, ok := readSegTrailerFile(segPath)
	if !ok {
		t.Fatal("setup: no trailer")
	}
	// Inside the first section's body, which is the whole trailer less its directory and
	// foot.
	damageTrailer(t, segPath, int64(tr.bytes)-4)

	r, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range r.Trailers {
		if got.Path != segPath {
			continue
		}
		if got.Why != "" {
			t.Fatalf("the whole trailer was condemned for one damaged body: %q", got.Why)
		}
		if len(got.Sections) != 1 || got.Sections[0] != segSecDictReplica {
			t.Errorf("Sections = %v; want the one replica section", got.Sections)
		}
		if out := r.String(); !strings.Contains(out, "rest of the trailer is intact") {
			t.Errorf("the report does not say the damage is contained:\n%s", out)
		}
		return
	}
	t.Fatalf("Fsck did not report the damaged section: %+v", r.Trailers)
}

// TestADamagedTrailerIsNeverWrittenOver: the length that says where a trailer begins lives
// in the foot that failed its checksum, so the arena's end is unknowable. Treating that as
// "no trailer" would put the arena's end at the FILE's end and append a second trailer
// after the first -- growing the file by one more trailer on every pass that tried, and
// folding the old one into what recovery maps as the arena.
func TestADamagedTrailerIsNeverWrittenOver(t *testing.T) {
	_, segPath := carrierWithTrailer(t)
	sizeBefore := damageTrailer(t, segPath, 9)

	if _, err := segArenaEnd(segPath); !errors.Is(err, ErrDamagedTrailer) {
		t.Fatalf("segArenaEnd gave %v; want ErrDamagedTrailer", err)
	}
	if err := addDictReplica(segPath, 64, 1, bytes.Repeat([]byte("d"), 64)); !errors.Is(err, ErrDamagedTrailer) {
		t.Fatalf("addDictReplica gave %v; want it to refuse", err)
	}
	st, err := os.Stat(segPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != sizeBefore {
		t.Errorf("the file grew from %d to %d bytes over a damaged trailer", sizeBefore, st.Size())
	}
}

// TestCensusSkipsASegmentWithADamagedTrailer: a segment that can no longer carry a copy
// must not stop the level being met -- there are other segments.
func TestCensusSkipsASegmentWithADamagedTrailer(t *testing.T) {
	dir, segPath := carrierWithTrailer(t)
	damageTrailer(t, segPath, 9)

	c := openCarrier(t, dir, 1)
	st := stateFor(t, c.EnsureDictReplicas(), 1)
	if st.Copies < dictReplicaTarget {
		t.Errorf("copies = %d (%v); want the level met from the other segments", st.Copies, st.Err)
	}
}
