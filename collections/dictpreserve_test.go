package collections

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func backupFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "dicts", dictBackupDir))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// TestReplacedDictionaryIsPreserved is the hole this closes. The file at the dictionary's
// own path is no longer the dictionary; the real one exists in this process's memory and
// its open descriptor and nowhere else, and both go when the process does. Reporting that
// and stopping leaves the store serving happily until it restarts, which is the whole
// shape of the original incident.
func TestReplacedDictionaryIsPreserved(t *testing.T) {
	c, path, want, recorded := openPinned(t)
	preservedBefore := DictsPreserved()
	replacement := append(append([]byte(nil), want...), []byte("-not-the-same-file")...)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, replacement, 0o644); err != nil {
		t.Fatal(err)
	}

	fault := onlyFault(t, c.VerifyDictFiles())
	if fault.Kind != DictFaultReplaced {
		t.Fatalf("Kind = %v; want %v", fault.Kind, DictFaultReplaced)
	}
	if fault.Preserved == "" {
		t.Fatalf("the dictionary was not preserved anywhere: %v", fault.Err)
	}
	kept, err := os.ReadFile(fault.Preserved)
	if err != nil {
		t.Fatalf("reading the preserved copy: %v", err)
	}
	if !bytes.Equal(kept, want) {
		t.Error("what was preserved is not the dictionary that was loaded")
	}
	if DictsPreserved() <= preservedBefore {
		t.Error("preserving was not counted")
	}
	// The replacement is still untouched: it may be the restore an operator is making.
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, replacement) {
		t.Error("the replacement file was not left alone")
	}
	if len(*recorded) != 1 || !strings.Contains((*recorded)[0].Error(), fault.Preserved) {
		t.Errorf("the report does not say where the dictionary is kept: %v", *recorded)
	}
}

// TestChangedDictionaryIsPreserved: the same inode with different contents is the other
// way the file stops being the dictionary.
func TestChangedDictionaryIsPreserved(t *testing.T) {
	c, path, want, _ := openPinned(t)
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, werr := f.WriteAt([]byte{^want[64], ^want[65]}, 64); werr != nil {
		t.Fatal(werr)
	}
	if cerr := f.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	fault := onlyFault(t, c.VerifyDictFiles())
	if fault.Kind != DictFaultChanged || fault.Preserved == "" {
		t.Fatalf("kind=%v preserved=%q err=%v", fault.Kind, fault.Preserved, fault.Err)
	}
	kept, err := os.ReadFile(fault.Preserved)
	if err != nil || !bytes.Equal(kept, want) {
		t.Error("what was preserved is not the dictionary that was loaded")
	}
}

// TestPreservedCopyIsNotDuplicated: the check runs on every maintenance pass, and a fault
// an operator has not dealt with yet persists across all of them. Naming the backup after
// its own contents is what keeps that from leaving one file per pass.
func TestPreservedCopyIsNotDuplicated(t *testing.T) {
	c, path, want, _ := openPinned(t)
	dir := filepath.Dir(filepath.Dir(path))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(want, 'x'), 0o644); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if f := c.VerifyDictFiles(); len(f) != 1 || f[0].Preserved == "" {
			t.Fatalf("pass %d: %+v", i, f)
		}
	}
	if names := backupFiles(t, dir); len(names) != 1 {
		t.Errorf("three passes left %d backup file(s): %v", len(names), names)
	}
}

// TestPreserveUsesACopyAlreadyInASegment: a dictionary carried in three segment trailers
// is already preserved. Writing a fourth copy into a directory that whatever took dicts/
// could take just as easily would buy nothing.
func TestPreserveUsesACopyAlreadyInASegment(t *testing.T) {
	dir := buildCarrierStore(t)
	func() {
		c := openCarrier(t, dir, 1)
		if st := stateFor(t, c.EnsureDictReplicas(), 1); st.Copies == 0 {
			t.Fatalf("fixture carries no copies: %v", st.Err)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	path := filepath.Join(dir, "dicts", "1.zst")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if rmErr := os.Remove(path); rmErr != nil {
		t.Fatal(rmErr)
	}
	if werr := os.WriteFile(path, append(append([]byte(nil), want...), 'x'), 0o644); werr != nil {
		t.Fatal(werr)
	}

	fault := onlyFault(t, c.VerifyDictFiles())
	if !strings.HasSuffix(fault.Preserved, ".dat") {
		t.Errorf("Preserved = %q; want the segment already carrying it", fault.Preserved)
	}
	if names := backupFiles(t, dir); len(names) != 0 {
		t.Errorf("wrote %v although a segment already carries the dictionary", names)
	}
}

// TestPreserveUsesACopyAlreadyInTheAttic: likewise for a dictionary pruning retired. The
// search is cheapest-first, so this is found without opening a single segment.
func TestPreserveUsesACopyAlreadyInTheAttic(t *testing.T) {
	c, path, want, _ := openPinned(t)
	dir := filepath.Dir(filepath.Dir(path))
	attic := filepath.Join(dir, "dicts", "attic")
	if err := os.MkdirAll(attic, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attic, "1.zst"), want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(want, 'x'), 0o644); err != nil {
		t.Fatal(err)
	}

	fault := onlyFault(t, c.VerifyDictFiles())
	if fault.Preserved != filepath.Join(attic, "1.zst") {
		t.Errorf("Preserved = %q; want the copy already in the attic", fault.Preserved)
	}
	if names := backupFiles(t, dir); len(names) != 0 {
		t.Errorf("wrote %v although the attic already holds the dictionary", names)
	}
	// A copy that is NOT the same bytes does not count as having preserved it.
	if werr := os.WriteFile(filepath.Join(attic, "1.zst"), append(want, 'y'), 0o644); werr != nil {
		t.Fatal(werr)
	}
	fault = onlyFault(t, c.VerifyDictFiles())
	if !strings.Contains(fault.Preserved, dictBackupDir) {
		t.Errorf("Preserved = %q; an attic copy with different bytes must not count", fault.Preserved)
	}
}

// TestPreservingDoesNotLookLikeTheDictsDirectoryVanishing: writing the first backup
// creates dicts/backup/, and writeDictFile counts a missing parent as the dicts directory
// having gone at runtime -- a real and serious signal. Crying wolf on it would make the
// counter useless exactly when it matters.
func TestPreservingDoesNotLookLikeTheDictsDirectoryVanishing(t *testing.T) {
	c, path, want, _ := openPinned(t)
	before := DictsDirRecreated()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(want, 'x'), 0o644); err != nil {
		t.Fatal(err)
	}

	if f := onlyFault(t, c.VerifyDictFiles()); f.Preserved == "" {
		t.Fatalf("nothing was preserved: %v", f.Err)
	}
	if got := DictsDirRecreated(); got != before {
		t.Errorf("DictsDirRecreated moved %d -> %d for a new backup directory", before, got)
	}
}

// TestUnpreservableDictionaryStillReportsTheRealReason: when the primary could not be
// rewritten AND nothing can be preserved, the fault must still carry why the rewrite
// failed rather than being overwritten by the second failure.
func TestUnpreservableDictionaryStillReportsTheRealReason(t *testing.T) {
	c, path, _, _ := openPinned(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	writeFaultHook = func(string) error { return errors.New("injected: no space left on device") }
	defer func() { writeFaultHook = nil }()

	fault := onlyFault(t, c.VerifyDictFiles())
	if fault.Repaired {
		t.Fatal("reported a repair that was refused")
	}
	if fault.Err == nil || !strings.Contains(fault.Err.Error(), "no space left") {
		t.Errorf("the fault lost why the rewrite failed: %v", fault.Err)
	}
}
