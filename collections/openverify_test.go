package collections

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenRefusesWhenDictionaryIsMissing: a dictionary is the only thing that can decode
// the segments naming it and nothing else on disk holds a copy, so a store missing one
// must be refused whole -- before any segment is mapped and before anything writes to it
// believing those segments are gone.
func TestOpenRefusesWhenDictionaryIsMissing(t *testing.T) {
	dir, shardDir := buildTwoDictStore(t)
	named := segmentsNaming(t, shardDir, 1)
	if len(named) == 0 {
		t.Fatal("fixture wrote no segment under dictionary 1")
	}
	if err := os.Remove(filepath.Join(dir, "dicts", "1.zst")); err != nil {
		t.Fatal(err)
	}

	_, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err == nil {
		t.Fatal("Open succeeded over a missing dictionary")
	}
	if !errors.Is(err, ErrMissingDictionaries) {
		t.Fatalf("error does not unwrap to ErrMissingDictionaries: %v", err)
	}
	var mde *MissingDictionaryError
	if !errors.As(err, &mde) {
		t.Fatalf("error is not a *MissingDictionaryError: %v", err)
	}
	if len(mde.IDs) != 1 || mde.IDs[0] != 1 {
		t.Errorf("IDs = %v; want [1]", mde.IDs)
	}
	if mde.Segments != len(named) {
		t.Errorf("Segments = %d; want %d", mde.Segments, len(named))
	}
	// The message has to be actionable without a second investigation.
	msg := err.Error()
	for _, want := range []string{"dicts/1.zst", ".d1.dat"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not mention %q: %s", want, msg)
		}
	}
}

// TestOpenNamesEveryMissingDictionary: failing at whichever segment recovery reached first
// reported one id and left the operator to rediscover the rest one restart at a time.
func TestOpenNamesEveryMissingDictionary(t *testing.T) {
	dir, shardDir := buildTwoDictStore(t)
	if len(segmentsNaming(t, shardDir, 1)) == 0 || len(segmentsNaming(t, shardDir, 2)) == 0 {
		t.Fatal("fixture did not write segments under both dictionaries")
	}
	for _, id := range []uint32{1, 2} {
		if err := os.Remove(filepath.Join(dir, "dicts", fmt.Sprintf("%d.zst", id))); err != nil {
			t.Fatal(err)
		}
	}

	_, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	var mde *MissingDictionaryError
	if !errors.As(err, &mde) {
		t.Fatalf("want *MissingDictionaryError, got %v", err)
	}
	if len(mde.IDs) != 2 || mde.IDs[0] != 1 || mde.IDs[1] != 2 {
		t.Errorf("IDs = %v; want [1 2]", mde.IDs)
	}
	for _, id := range mde.IDs {
		if mde.Example[id] == "" {
			t.Errorf("no example segment given for dictionary %d", id)
		}
	}
	// The message renders a SET when several are missing, and names each one's example.
	msg := err.Error()
	for _, want := range []string{"dicts/{1,2}.zst", ".d1.dat", ".d2.dat"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not mention %q: %s", want, msg)
		}
	}
}

// TestOpenIgnoresDirectoriesInAShard: a shard directory holding a subdirectory -- an
// operator's own scratch, a half-finished move -- must not be mistaken for a segment by
// either the Open cross-check or the dictionary prune.
func TestOpenIgnoresDirectoriesInAShard(t *testing.T) {
	dir, shardDir := buildTwoDictStore(t)
	// Named so that, if it were treated as a file, it would parse as a segment naming a
	// dictionary that does not exist.
	if err := os.MkdirAll(filepath.Join(shardDir, "seg-99999.d77.dat"), 0o755); err != nil {
		t.Fatal(err)
	}

	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatalf("Open refused over a directory in the shard: %v", err)
	}
	defer c.Close()
	if n := c.Len(); n == 0 {
		t.Error("no records came back")
	}
}

// TestOpenSucceedsOnceDictionaryIsRestored is the point of refusing rather than opening
// degraded: the records were never damaged, and putting the file back brings them all
// back. A store that had been written to meanwhile could not promise that.
func TestOpenSucceedsOnceDictionaryIsRestored(t *testing.T) {
	dir, _ := buildTwoDictStore(t)
	path := filepath.Join(dir, "dicts", "1.zst")
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if rmErr := os.Remove(path); rmErr != nil {
		t.Fatal(rmErr)
	}
	if _, openErr := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14}); openErr == nil {
		t.Fatal("control: Open succeeded over a missing dictionary")
	}

	if werr := os.WriteFile(path, saved, 0o644); werr != nil {
		t.Fatal(werr)
	}
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatalf("Open after restoring the dictionary: %v", err)
	}
	defer c.Close()
	if n := c.Len(); n == 0 {
		t.Error("no records came back after restoring the dictionary")
	}
}

// TestOpenAcceptsStoreNeedingNoDictionary is the control. A fresh store, and one whose
// segments are all under the base codec, have no dicts directory and must open: id 0 is
// reconstructed from Options and is never a file.
func TestOpenAcceptsStoreNeedingNoDictionary(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatalf("fresh store: %v", err)
	}
	sample := loadCorpus(t)
	for i := 0; i < 200; i++ {
		if perr := c.Put([]byte(fmt.Sprintf("k%d", i)), sample[i%len(sample)]); perr != nil {
			t.Fatal(perr)
		}
	}
	if cerr := c.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	// Remove the (empty) dicts directory entirely: nothing names a trained dictionary, so
	// there is nothing to refuse.
	if rerr := os.RemoveAll(filepath.Join(dir, "dicts")); rerr != nil {
		t.Fatal(rerr)
	}
	c2, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatalf("store using only the base codec: %v", err)
	}
	defer c2.Close()
	if n := c2.Len(); n == 0 {
		t.Error("no records came back")
	}
}

// TestDictsDirRecreatedIsCounted: nothing in this package removes the dicts directory, so
// a write that has to recreate it means something outside the process did -- taking every
// dictionary in it, and dicts/attic/ with it, since the attic lives inside. The store runs
// on its in-memory codecs and fails at the NEXT open, so this counter is the only warning
// available in between.
func TestDictsDirRecreatedIsCounted(t *testing.T) {
	dicts := filepath.Join(t.TempDir(), "dicts")
	if err := os.MkdirAll(dicts, 0o755); err != nil {
		t.Fatal(err)
	}
	before := DictsDirRecreated()
	if err := writeDictFile(filepath.Join(dicts, "1.zst"), []byte("dictionary bytes")); err != nil {
		t.Fatal(err)
	}
	if got := DictsDirRecreated(); got != before {
		t.Errorf("counter moved %d -> %d for a write into an existing directory", before, got)
	}

	if err := os.RemoveAll(dicts); err != nil {
		t.Fatal(err)
	}
	if err := writeDictFile(filepath.Join(dicts, "2.zst"), []byte("dictionary bytes")); err != nil {
		t.Fatal(err)
	}
	if got := DictsDirRecreated(); got != before+1 {
		t.Errorf("counter = %d; want %d after recreating a vanished dicts directory", got, before+1)
	}
}

// TestOpenIgnoresStrayDatNamingAMissingDictionary guards the asymmetry between keeping a
// dictionary and refusing to open. Recovery loads only seg-<n>.d<dict>.dat; anything else
// in a shard directory is a stray it has always ignored, so a store holding one opens
// today and must keep opening. Refusing on a leftover nothing reads would turn a harmless
// file into a daemon that will not start.
//
// pruneDicts is deliberately the other way round: it protects the dictionary of any file
// that looks like it might need one, because being wrong there costs tens of KB.
func TestOpenIgnoresStrayDatNamingAMissingDictionary(t *testing.T) {
	dir, shardDir := buildTwoDictStore(t)
	named := segmentsNaming(t, shardDir, 1)
	if len(named) == 0 {
		t.Fatal("fixture wrote no segment under dictionary 1")
	}
	// Rename every segment under dictionary 1 to a name recovery ignores, then remove the
	// dictionary. Nothing recovery loads needs it any more.
	for _, name := range named {
		if err := os.Rename(filepath.Join(shardDir, name),
			filepath.Join(shardDir, "orphan-"+name)); err != nil {
			t.Fatal(err)
		}
		_ = os.Rename(filepath.Join(shardDir, name+".idx"),
			filepath.Join(shardDir, "orphan-"+name+".idx"))
	}
	if err := os.Remove(filepath.Join(dir, "dicts", "1.zst")); err != nil {
		t.Fatal(err)
	}

	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatalf("Open refused over a stray .dat recovery never reads: %v", err)
	}
	defer c.Close()

	// The control: restore one to a name recovery DOES read, and the refusal must fire.
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(shardDir, "orphan-"+named[0]),
		filepath.Join(shardDir, named[0])); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14}); !errors.Is(err, ErrMissingDictionaries) {
		t.Errorf("Open gave %v; want ErrMissingDictionaries once a loadable segment names it", err)
	}
}

// TestRecoveredSegmentDictID pins the strict parse against loadShard's rule.
func TestRecoveredSegmentDictID(t *testing.T) {
	for _, tc := range []struct {
		name string
		want uint32
		ok   bool
	}{
		{"seg-12.d3.dat", 3, true},
		{"seg-0.d0.dat", 0, true},
		{"orphan-seg-12.d3.dat", 0, false}, // a stray; recovery ignores it
		{"tmp-merge-9.d17.dat", 0, false},  // staged output; recovery ignores it
		{"seg-12.d3.dat.idx", 0, false},
		{"dir.snap", 0, false},
	} {
		got, ok := recoveredSegmentDictID(tc.name)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("recoveredSegmentDictID(%q) = (%d, %v); want (%d, %v)",
				tc.name, got, ok, tc.want, tc.ok)
		}
	}
}
