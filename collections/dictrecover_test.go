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

// replicatedStore builds the carrier fixture, brings every dictionary's copies up to the
// target, and closes. It returns the store directory and the bytes of dicts/1.zst.
func replicatedStore(t *testing.T) (dir string, dict1 []byte) {
	t.Helper()
	dir = buildCarrierStore(t)
	c := openCarrier(t, dir, 1)
	if st := stateFor(t, c.EnsureDictReplicas(), 1); st.Copies == 0 {
		t.Fatalf("fixture carries no copies: %v", st.Err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "dicts", "1.zst"))
	if err != nil {
		t.Fatal(err)
	}
	return dir, b
}

// TestOpenRestoresADictionaryFromASegment is what the whole mechanism is for. The
// dictionary file is gone, the store was not running when it went, and every record in
// every segment naming it would be unreadable -- except that the segments carry a copy, so
// the store simply opens.
func TestOpenRestoresADictionaryFromASegment(t *testing.T) {
	dir, want := replicatedStore(t)
	path := filepath.Join(dir, "dicts", "1.zst")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	before := DictsRecoveredFromSegments()

	var reported []error
	c, err := Open(Options{
		Dir: dir, Shards: 1, SegmentSize: 1 << 20,
		OnDictFault: func(e error) { reported = append(reported, e) },
	})
	if err != nil {
		t.Fatalf("Open refused a store whose segments carry the dictionary: %v", err)
	}
	defer c.Close()
	if n := c.Len(); n == 0 {
		t.Error("no records came back")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the dictionary was not written back: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("what was written back is not the dictionary that was lost")
	}
	if DictsRecoveredFromSegments() <= before {
		t.Error("the recovery was not counted")
	}
	if len(reported) != 1 {
		t.Fatalf("OnDictFault fired %d times; want 1", len(reported))
	}
	var df *DictFault
	if !errors.As(reported[0], &df) || !df.Repaired || df.From == "" {
		t.Fatalf("the report does not say a copy was restored from a segment: %v", reported[0])
	}
	// Restoring silently would be the same mistake the old code made: the primary copy
	// was lost while the store was down, and that is worth saying out loud.
	for _, want := range []string{path, df.From, "was not running"} {
		if !strings.Contains(reported[0].Error(), want) {
			t.Errorf("the message does not mention %q: %v", want, reported[0])
		}
	}
}

// TestOpenRestoresAfterTheWholeDirectoryGoes is the shape of the incident: not one file
// but dicts/ entire, taken while the store was down.
func TestOpenRestoresAfterTheWholeDirectoryGoes(t *testing.T) {
	dir, want := replicatedStore(t)
	if err := os.RemoveAll(filepath.Join(dir, "dicts")); err != nil {
		t.Fatal(err)
	}

	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 20})
	if err != nil {
		t.Fatalf("Open after the dictionary directory vanished: %v", err)
	}
	defer c.Close()
	if n := c.Len(); n == 0 {
		t.Error("no records came back")
	}
	got, err := os.ReadFile(filepath.Join(dir, "dicts", "1.zst"))
	if err != nil {
		t.Fatalf("the directory was not rebuilt: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("what was written back is not the dictionary that was lost")
	}
}

// TestOpenStillRefusesWhatNoSegmentCarries is the control. The sweep must not turn the
// refusal into a shrug: a dictionary nothing holds a copy of is still fatal, and must
// still name itself.
func TestOpenStillRefusesWhatNoSegmentCarries(t *testing.T) {
	dir, _ := replicatedStore(t)
	// Strip the copies, then the dictionary.
	for _, name := range trailerCarriers(t, dir, 1, 1) {
		p := filepath.Join(dir, "0", name)
		arenaEnd, err := segArenaEnd(p)
		if err != nil {
			t.Fatal(err)
		}
		if terr := os.Truncate(p, arenaEnd); terr != nil {
			t.Fatal(terr)
		}
	}
	if err := os.Remove(filepath.Join(dir, "dicts", "1.zst")); err != nil {
		t.Fatal(err)
	}

	_, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 20})
	if !errors.Is(err, ErrMissingDictionaries) {
		t.Fatalf("Open gave %v; want ErrMissingDictionaries", err)
	}
	var mde *MissingDictionaryError
	if !errors.As(err, &mde) || len(mde.IDs) != 1 || mde.IDs[0] != 1 {
		t.Errorf("the refusal does not name dictionary 1: %v", err)
	}
}

// TestOpenRefusesAReplicaFiledUnderTheWrongID: a segment carrying a dictionary stamped 2
// under the label 1 is reporting a confusion, not holding a copy. Writing those bytes
// into dicts/1.zst would make the confusion permanent and silently skip every record of
// every segment naming 1 -- which is the failure mode stamping exists to end.
func TestOpenRefusesAReplicaFiledUnderTheWrongID(t *testing.T) {
	dir, _ := replicatedStore(t)
	other, err := os.ReadFile(filepath.Join(dir, "dicts", "1.zst"))
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := stampDictID(other, 2) // a real dictionary, but not dictionary 1
	if err != nil {
		t.Fatal(err)
	}
	seg := aSealedSegmentOf(t, dir, 1<<20)
	if aerr := addDictReplica(seg.path, seg.used, 1, wrong); aerr != nil {
		t.Fatal(aerr)
	}
	// Remove every other copy so the mislabelled one is the only candidate.
	for _, name := range trailerCarriers(t, dir, 1, 1) {
		p := filepath.Join(dir, "0", name)
		if p == seg.path {
			continue
		}
		arenaEnd, aerr := segArenaEnd(p)
		if aerr != nil {
			t.Fatal(aerr)
		}
		if terr := os.Truncate(p, arenaEnd); terr != nil {
			t.Fatal(terr)
		}
	}
	path := filepath.Join(dir, "dicts", "1.zst")
	if rerr := os.Remove(path); rerr != nil {
		t.Fatal(rerr)
	}

	if _, oerr := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 20}); !errors.Is(oerr, ErrMissingDictionaries) {
		t.Fatalf("Open gave %v; want it to refuse a copy filed under the wrong id", oerr)
	}
	if _, serr := os.Stat(path); serr == nil {
		t.Error("the mislabelled dictionary was written into dicts/")
	}
}

// TestQuarantineDictlessPrefersASegmentCopy: quarantine sets records aside, and a copy in
// a segment means they were never lost. Reopening alone restores it, so there is nothing
// to set aside.
func TestQuarantineDictlessPrefersASegmentCopy(t *testing.T) {
	dir, _ := replicatedStore(t)
	if err := os.Remove(filepath.Join(dir, "dicts", "1.zst")); err != nil {
		t.Fatal(err)
	}

	rep, err := QuarantineDictless(dir, DictlessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + rep.String())
	if len(rep.Moved) != 0 {
		t.Errorf("quarantined %d segments although a copy is carried in one", len(rep.Moved))
	}
	if len(rep.Recoverable) != 1 || rep.Recoverable[0] != 1 {
		t.Fatalf("Recoverable = %v; want [1]", rep.Recoverable)
	}
	if from := rep.RecoverableFrom[1]; !strings.Contains(from, ".dat") {
		t.Errorf("RecoverableFrom[1] = %q; want the segment carrying it", from)
	}
	if out := rep.String(); !strings.Contains(out, "REFUSED") || !strings.Contains(out, "reopening") {
		t.Errorf("the report does not point at the cheaper recovery:\n%s", out)
	}
}

// TestFsckCountsSegmentCopies: the number of copies is the answer to "how close is this
// store to losing a dictionary for good", and fsck is where an operator asks.
func TestFsckCountsSegmentCopies(t *testing.T) {
	dir, _ := replicatedStore(t)

	r, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.DictReplicas[1]; got != dictReplicaTarget {
		t.Errorf("DictReplicas[1] = %d; want %d", got, dictReplicaTarget)
	}
	if len(r.DictsMissing) != 0 {
		t.Errorf("a healthy store reports missing dictionaries: %v", r.DictsMissing)
	}

	// With the file gone, the copies become the recovery, and fsck must say so rather
	// than reporting an unreadable store.
	if rerr := os.Remove(filepath.Join(dir, "dicts", "1.zst")); rerr != nil {
		t.Fatal(rerr)
	}
	r2, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.DictsInSegments) != 1 || r2.DictsInSegments[0] != 1 {
		t.Fatalf("DictsInSegments = %v; want [1]", r2.DictsInSegments)
	}
	out := r2.String()
	if !strings.Contains(out, "RECOVERABLE dictionary 1") || !strings.Contains(out, "reopening") {
		t.Errorf("the report does not offer the recovery:\n%s", out)
	}
}

// TestFsckReportsAThinDictionary: a dictionary present in dicts/ and carried nowhere is
// one deletion away from taking its segments with it, and that is exactly the state the
// whole mechanism exists to get out of.
func TestFsckReportsAThinDictionary(t *testing.T) {
	dir := buildCarrierStore(t) // built, never replicated

	r, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := r.DictReplicas[1]; n != 0 {
		t.Errorf("DictReplicas[1] = %d; want 0 on a store that never ran a census", n)
	}
	if out := r.String(); !strings.Contains(out, "THIN dictionary 1") {
		t.Errorf("the report does not flag the dictionary as thin:\n%s", out)
	}
}

// TestOpenRefusesWhenOnlySomeDictionariesCanBeRecovered is the case a partial recovery
// gets wrong. Two dictionaries are missing and only one is carried anywhere; restoring
// that one must NOT be read as the store being whole. Opening here would map segments
// whose dictionary is gone and then silently skip every record in them -- the failure the
// coverage check exists to prevent, reintroduced by the fix for it.
func TestOpenRefusesWhenOnlySomeDictionariesCanBeRecovered(t *testing.T) {
	dir, _ := replicatedStore(t)
	// Make a second dictionary go missing by renaming a segment onto an id that was never
	// trained: nothing has its file and nothing carries a copy, which is exactly the state
	// of a dictionary genuinely lost.
	entries, err := os.ReadDir(filepath.Join(dir, "0"))
	if err != nil {
		t.Fatal(err)
	}
	renamed := ""
	for _, e := range entries {
		var n uint64
		var d uint32
		if _, serr := fmt.Sscanf(e.Name(), "seg-%d.d%d.dat", &n, &d); serr != nil || d != 1 {
			continue
		}
		renamed = fmt.Sprintf("seg-%d.d9.dat", n)
		if rerr := os.Rename(filepath.Join(dir, "0", e.Name()), filepath.Join(dir, "0", renamed)); rerr != nil {
			t.Fatal(rerr)
		}
		_ = os.Rename(filepath.Join(dir, "0", e.Name()+".idx"), filepath.Join(dir, "0", renamed+".idx"))
		break
	}
	if renamed == "" {
		t.Skip("fixture has no segment under dictionary 1")
	}
	_ = os.Remove(filepath.Join(dir, "0", dirSnapName))
	if rerr := os.Remove(filepath.Join(dir, "dicts", "1.zst")); rerr != nil {
		t.Fatal(rerr)
	}

	_, err = Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 20})
	if !errors.Is(err, ErrMissingDictionaries) {
		t.Fatalf("Open gave %v; want it to still refuse over the dictionary nothing carries", err)
	}
	var mde *MissingDictionaryError
	if !errors.As(err, &mde) {
		t.Fatalf("not a *MissingDictionaryError: %v", err)
	}
	if len(mde.IDs) != 1 || mde.IDs[0] != 9 {
		t.Errorf("the refusal names %v; want only dictionary 9, since 1 was recovered", mde.IDs)
	}
	// And the one that WAS recoverable is back on disk, so restoring the other is all
	// that stands between the operator and a store that opens.
	if _, serr := os.Stat(filepath.Join(dir, "dicts", "1.zst")); serr != nil {
		t.Errorf("dictionary 1 was not restored even though a segment carried it: %v", serr)
	}
}
