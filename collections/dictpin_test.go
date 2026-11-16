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

// openPinned opens the two-dictionary fixture store with a fault recorder attached, and
// returns the collection, the path of dicts/1.zst, and the bytes that file holds.
func openPinned(t *testing.T) (c *Collection, path string, want []byte, faults *[]error) {
	t.Helper()
	dir, _ := buildTwoDictStore(t)
	rec := &[]error{}
	c, err := Open(Options{
		Dir: dir, Shards: 1, SegmentSize: 1 << 14,
		OnDictFault: func(e error) { *rec = append(*rec, e) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	path = filepath.Join(dir, "dicts", "1.zst")
	want, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return c, path, want, rec
}

func onlyFault(t *testing.T, faults []DictFault) DictFault {
	t.Helper()
	if len(faults) != 1 {
		t.Fatalf("want exactly one fault, got %d: %v", len(faults), faults)
	}
	return faults[0]
}

// TestDeletedDictionaryFileIsRestored is the whole point of holding the descriptor. The
// file is unlinked underneath a running store; the inode -- and so the bytes -- survive
// because this process still has it open, and the check puts the directory entry back
// before the restart that would have made the loss permanent.
func TestDeletedDictionaryFileIsRestored(t *testing.T) {
	c, path, want, recorded := openPinned(t)
	missingBefore, restoredBefore := DictFilesMissing(), DictFilesRestored()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	fault := onlyFault(t, c.VerifyDictFiles())
	if fault.Kind != DictFaultMissing {
		t.Errorf("Kind = %v; want %v", fault.Kind, DictFaultMissing)
	}
	if !fault.Repaired {
		t.Fatalf("a deleted dictionary file was not rewritten: %v", fault.Err)
	}
	if fault.ID != 1 {
		t.Errorf("ID = %d; want 1", fault.ID)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("dictionary file not put back: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("the rewritten dictionary is not the one that was loaded")
	}
	if DictFilesMissing() <= missingBefore || DictFilesRestored() <= restoredBefore {
		t.Error("counters did not move")
	}
	if len(*recorded) != 1 {
		t.Fatalf("OnDictFault fired %d times; want 1", len(*recorded))
	}
	if !strings.Contains((*recorded)[0].Error(), path) {
		t.Errorf("the reported error does not name the file: %v", (*recorded)[0])
	}
	var df *DictFault
	if !errors.As((*recorded)[0], &df) || df.Kind != DictFaultMissing {
		t.Errorf("OnDictFault did not deliver a *DictFault describing the loss: %v", (*recorded)[0])
	}

	// A second check finds nothing: the restore re-pinned to the file it just wrote, so
	// the new inode is not then reported as a replacement forever after.
	if f := c.VerifyDictFiles(); len(f) != 0 {
		t.Errorf("the restored file is still reported as faulty: %v", f)
	}

	// And the store opens on it, which is the outcome all of this is for.
	if cerr := c.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	c2, err := Open(Options{Dir: filepath.Dir(filepath.Dir(path)), Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatalf("reopen after the restore: %v", err)
	}
	defer c2.Close()
	if n := c2.Len(); n == 0 {
		t.Error("no records came back")
	}
}

// TestReplacedDictionaryFileIsReportedNotOverwritten: a different file at the same path
// is what an operator restoring from a backup looks like. Reporting it is right;
// overwriting it would destroy the thing being put back.
func TestReplacedDictionaryFileIsReportedNotOverwritten(t *testing.T) {
	c, path, want, _ := openPinned(t)
	replacement := append([]byte(nil), want...)
	replacement = append(replacement, []byte("-not-the-same-file")...)
	if err := os.Remove(path); err != nil { // remove+create: a new inode at the same path
		t.Fatal(err)
	}
	if err := os.WriteFile(path, replacement, 0o644); err != nil {
		t.Fatal(err)
	}

	fault := onlyFault(t, c.VerifyDictFiles())
	if fault.Kind != DictFaultReplaced {
		t.Fatalf("Kind = %v; want %v", fault.Kind, DictFaultReplaced)
	}
	if fault.Repaired {
		t.Error("a replaced dictionary file was overwritten")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, replacement) {
		t.Error("the replacement file was not left alone")
	}
}

// TestChangedDictionaryFileIsReported: the same inode, different contents. A ZSTD
// dictionary carries no checksum of its own, so without the CRC taken at load there is
// nothing to compare and this is invisible.
func TestChangedDictionaryFileIsReported(t *testing.T) {
	c, path, want, _ := openPinned(t)
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Past the magic and the id, so the file is still a dictionary and still the same
	// size: only the content check can see this.
	if _, werr := f.WriteAt([]byte{^want[64], ^want[65]}, 64); werr != nil {
		t.Fatal(werr)
	}
	if cerr := f.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	fault := onlyFault(t, c.VerifyDictFiles())
	if fault.Kind != DictFaultChanged {
		t.Fatalf("Kind = %v; want %v", fault.Kind, DictFaultChanged)
	}
	if fault.Repaired {
		t.Error("a changed dictionary file was overwritten")
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() != int64(len(want)) {
		t.Errorf("the file was not left as it was: %v size=%d", err, st.Size())
	}
}

// TestHealthyStoreHasNoDictFaults is the control: the check must not invent work on a
// store where nothing happened.
func TestHealthyStoreHasNoDictFaults(t *testing.T) {
	c, _, _, recorded := openPinned(t)
	if f := c.VerifyDictFiles(); len(f) != 0 {
		t.Errorf("healthy store reported %d fault(s): %v", len(f), f)
	}
	if len(*recorded) != 0 {
		t.Errorf("OnDictFault fired on a healthy store: %v", *recorded)
	}
}

// TestRetiredDictionaryIsNotRestored is the rule the restore must not break. Pruning
// RETIRES a dictionary to dicts/attic/ on purpose, so a pin left behind would read that
// departure as a deletion and put the file straight back -- undoing the retire on every
// pass and leaking a descriptor each time.
func TestRetiredDictionaryIsNotRestored(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dictsDir := filepath.Join(t.TempDir(), "dicts")
	reg := newDictReg(identityCodec{})
	reg.dir = dictsDir
	id, _ := registerTrained(t, reg, 1)
	if _, err := os.Stat(filepath.Join(dictsDir, fmt.Sprintf("%d.zst", id))); err != nil {
		t.Fatal(err)
	}

	reg.prune(func(uint32, Codec) bool { return false })

	if f := reg.verifyFiles(); len(f) != 0 {
		t.Fatalf("a retired dictionary was reported as a fault: %v", f)
	}
	if _, err := os.Stat(filepath.Join(dictsDir, fmt.Sprintf("%d.zst", id))); err == nil {
		t.Error("the retired dictionary was put back into dicts/")
	}
	if _, err := os.Stat(filepath.Join(dictsDir, "attic", fmt.Sprintf("%d.zst", id))); err != nil {
		t.Errorf("the retired dictionary is not in the attic: %v", err)
	}
	if n := len(reg.pins); n != 0 {
		t.Errorf("%d pin(s) left after pruning every dictionary", n)
	}
}

// TestVanishedDictsDirectoryIsRebuilt is the shape of the incident this exists for: not
// one file but the whole directory, taken while the store was running and healthy.
func TestVanishedDictsDirectoryIsRebuilt(t *testing.T) {
	c, path, want, _ := openPinned(t)
	dictsDir := filepath.Dir(path)
	recreatedBefore := DictsDirRecreated()
	if err := os.RemoveAll(dictsDir); err != nil {
		t.Fatal(err)
	}

	faults := c.VerifyDictFiles()
	if len(faults) == 0 {
		t.Fatal("the dictionary directory vanished and nothing was reported")
	}
	for _, f := range faults {
		if f.Kind != DictFaultMissing || !f.Repaired {
			t.Errorf("dictionary %d: kind=%v repaired=%v err=%v", f.ID, f.Kind, f.Repaired, f.Err)
		}
	}
	if DictsDirRecreated() <= recreatedBefore {
		t.Error("recreating the directory was not counted")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("dictionary not rewritten into the recreated directory: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("the rewritten dictionary is not the one that was loaded")
	}
}

// TestMaintenanceChecksTheDictionaryFiles proves the hook, not the check: an ordinary
// maintenance pass has to be what notices, because nothing else looks.
func TestMaintenanceChecksTheDictionaryFiles(t *testing.T) {
	c, path, want, recorded := openPinned(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	c.lastDictVerify.Store(0) // the pass is rate-limited; this one is due

	c.Compact()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("a maintenance pass did not restore the dictionary: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("the rewritten dictionary is not the one that was loaded")
	}
	if len(*recorded) == 0 {
		t.Error("a maintenance pass restored a dictionary without reporting it")
	}
}

// TestDictVerifyIsRateLimited: the check runs on every maintenance pass, and a Rotate
// runs on every seal, so the interval is what keeps a busy store from paying for the
// check more often than an idle one.
func TestDictVerifyIsRateLimited(t *testing.T) {
	c, path, _, _ := openPinned(t)
	c.VerifyDictFiles() // sets the clock
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	c.maybeVerifyDictFiles()
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the check ran again inside the interval")
	}

	c.lastDictVerify.Store(0)
	c.maybeVerifyDictFiles()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the check did not run once it was due: %v", err)
	}
}

// TestCloseReleasesDictionaryDescriptors: the descriptors are held for the life of the
// collection, and a daemon that opens and closes many tables must not accumulate them.
func TestCloseReleasesDictionaryDescriptors(t *testing.T) {
	c, _, _, _ := openPinned(t)
	if n := len(c.dicts.pins); n == 0 {
		t.Fatal("no dictionary files were pinned")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if n := len(c.dicts.pins); n != 0 {
		t.Errorf("%d dictionary descriptor(s) still held after Close", n)
	}
}

// TestDictFaultMessagesAreUsable: the callback is the loud half of the check, and what it
// hands over has to say which file, what happened to it, and what it costs -- without a
// second investigation. An operator reading a log line is the whole audience.
func TestDictFaultMessagesAreUsable(t *testing.T) {
	cause := errors.New("read-only file system")
	for _, tc := range []struct {
		name string
		f    DictFault
		want []string
	}{
		{"restored", DictFault{ID: 3, Path: "/d/3.zst", Kind: DictFaultMissing, Repaired: true},
			[]string{"/d/3.zst", "rewritten", "deleting"}},
		{"not restored", DictFault{ID: 3, Path: "/d/3.zst", Kind: DictFaultMissing, Err: cause},
			[]string{"/d/3.zst", "read-only file system", "unreadable"}},
		{"replaced", DictFault{ID: 4, Path: "/d/4.zst", Kind: DictFaultReplaced},
			[]string{"/d/4.zst", "replaced"}},
		{"changed", DictFault{ID: 5, Path: "/d/5.zst", Kind: DictFaultChanged},
			[]string{"/d/5.zst", "changed"}},
		{"check failed", DictFault{ID: 6, Path: "/d/6.zst", Kind: DictFaultChanged, Err: cause},
			[]string{"/d/6.zst", "changed", "read-only file system"}},
	} {
		msg := tc.f.Error()
		for _, want := range tc.want {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: message does not mention %q: %s", tc.name, want, msg)
			}
		}
	}
	// Unwrap is what lets a caller reach the cause behind a fault.
	f := &DictFault{Kind: DictFaultMissing, Err: cause}
	if !errors.Is(f, cause) {
		t.Error("the cause is not reachable through the fault")
	}
	if (&DictFault{Kind: DictFaultChanged}).Unwrap() != nil {
		t.Error("a fault with no cause unwrapped to something")
	}
	for kind, want := range map[DictFaultKind]string{
		DictFaultMissing: "missing", DictFaultReplaced: "replaced", DictFaultChanged: "changed",
	} {
		if got := kind.String(); got != want {
			t.Errorf("DictFaultKind(%d).String() = %q; want %q", int(kind), got, want)
		}
	}
	if got := DictFaultKind(99).String(); !strings.Contains(got, "99") {
		t.Errorf("an unknown kind renders as %q", got)
	}
}

// TestTruncatedDictionaryFileIsReported: the same inode, fewer bytes. Caught on size
// before anything is read, which is the cheap half of the content check.
func TestTruncatedDictionaryFileIsReported(t *testing.T) {
	c, path, want, _ := openPinned(t)
	changedBefore := DictFilesChanged()
	if err := os.Truncate(path, int64(len(want)/2)); err != nil {
		t.Fatal(err)
	}

	fault := onlyFault(t, c.VerifyDictFiles())
	if fault.Kind != DictFaultChanged || fault.Repaired {
		t.Fatalf("kind=%v repaired=%v; want changed, untouched", fault.Kind, fault.Repaired)
	}
	if DictFilesChanged() <= changedBefore {
		t.Error("DictFilesChanged did not move")
	}
}

// TestReplacedDictionaryIsCounted pins the counter to its own kind: three counters that
// all moved together would say nothing an operator could act on.
func TestReplacedDictionaryIsCounted(t *testing.T) {
	c, path, want, _ := openPinned(t)
	replacedBefore, missingBefore := DictFilesReplaced(), DictFilesMissing()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(want, 'x'), 0o644); err != nil {
		t.Fatal(err)
	}

	c.VerifyDictFiles()
	if DictFilesReplaced() != replacedBefore+1 {
		t.Errorf("DictFilesReplaced %d -> %d; want +1", replacedBefore, DictFilesReplaced())
	}
	if DictFilesMissing() != missingBefore {
		t.Errorf("a replacement was also counted as missing (%d -> %d)", missingBefore, DictFilesMissing())
	}
}

// TestUnrestorableDictionaryFileIsReportedAnyway: holding the bytes is not the same as
// being able to write them back -- a full or read-only filesystem is exactly when this
// fires. The fault must still be reported, and must say the repair did not happen, since
// that is the difference between "handled" and "you have until the next restart".
func TestUnrestorableDictionaryFileIsReportedAnyway(t *testing.T) {
	c, path, _, recorded := openPinned(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	writeFaultHook = func(p string) error {
		if p == path {
			return errors.New("injected: no space left on device")
		}
		return nil
	}
	defer func() { writeFaultHook = nil }()

	fault := onlyFault(t, c.VerifyDictFiles())
	if fault.Kind != DictFaultMissing {
		t.Fatalf("Kind = %v; want %v", fault.Kind, DictFaultMissing)
	}
	if fault.Repaired {
		t.Fatal("reported a repair that was refused")
	}
	if fault.Err == nil || !strings.Contains(fault.Err.Error(), "no space left") {
		t.Errorf("the fault does not carry why the repair failed: %v", fault.Err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the file came back despite the injected failure")
	}
	if len(*recorded) != 1 || !strings.Contains((*recorded)[0].Error(), "unreadable") {
		t.Errorf("the reported error does not say what it costs: %v", *recorded)
	}
}

// TestPinDictFileRefusesAMissingFile is the control for everything above: a pin is only
// ever taken on a file that is there, so the registry never holds one that vouches for
// nothing.
func TestPinDictFileRefusesAMissingFile(t *testing.T) {
	if _, _, err := pinDictFile(filepath.Join(t.TempDir(), "absent.zst")); err == nil {
		t.Error("pinned a file that does not exist")
	}
}
