package collections

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// buildTwoDictStore builds a persistent collection holding segments under two trained
// dictionaries, with a third (the current write codec) installed last. Dictionary 1 is
// therefore referenced by segments but is NOT the write codec -- the only shape in which
// pruning can consider it dead.
func buildTwoDictStore(t *testing.T) (dir, shardDir string) {
	t.Helper()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	sample := loadCorpus(t)
	dir = t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	put := func(lo, hi int) {
		for i := lo; i < hi; i++ {
			if err := c.Put([]byte(fmt.Sprintf("job%05d", i)), sample[i%len(sample)]); err != nil {
				t.Fatalf("put: %v", err)
			}
		}
	}
	put(0, 700)
	if _, err := c.RetrainDict(1500); err != nil { // dict 1
		t.Fatalf("retrain 1: %v", err)
	}
	put(700, 1400)                                 // segments under dict 1
	if _, err := c.RetrainDict(1500); err != nil { // dict 2, now the write codec
		t.Fatalf("retrain 2: %v", err)
	}
	put(1400, 2100) // segments under dict 2
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return dir, filepath.Join(dir, "0")
}

// segmentsNaming lists the segment files in shardDir whose name encodes dictID.
func segmentsNaming(t *testing.T, shardDir string, dictID uint32) []string {
	t.Helper()
	entries, err := os.ReadDir(shardDir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if id, ok := dictIDFromSegmentName(e.Name()); ok && id == dictID {
			out = append(out, e.Name())
		}
	}
	return out
}

func dictExists(dir string, id uint32) bool {
	_, err := os.Stat(filepath.Join(dir, "dicts", fmt.Sprintf("%d.zst", id)))
	return err == nil
}

func atticHas(dir string, id uint32) bool {
	_, err := os.Stat(filepath.Join(dir, "dicts", "attic", fmt.Sprintf("%d.zst", id)))
	return err == nil
}

// TestPruneKeepsDictionaryOfUnmappedSegmentFile is the whole point: a segment file whose
// name recovery does not recognise is not mapped, so the in-memory segment set says its
// dictionary is dead -- while the file sitting in the shard directory says otherwise, and
// the file is right. Retiring the dictionary there is what makes the data unrecoverable,
// because the id is recorded nowhere but in that file name.
func TestPruneKeepsDictionaryOfUnmappedSegmentFile(t *testing.T) {
	dir, shardDir := buildTwoDictStore(t)

	named := segmentsNaming(t, shardDir, 1)
	if len(named) == 0 {
		t.Fatal("fixture wrote no segment under dictionary 1")
	}
	// Rename every one of them to something recovery ignores: it matches
	// seg-<n>.d<dict>.dat exactly. The dictionary id is still in the name, which is the
	// only reason the file is recoverable at all.
	for _, name := range named {
		if err := os.Rename(filepath.Join(shardDir, name),
			filepath.Join(shardDir, "orphan-"+name)); err != nil {
			t.Fatal(err)
		}
		// Sidecars follow the data file, as a mangling restore would leave them.
		_ = os.Rename(filepath.Join(shardDir, name+".idx"),
			filepath.Join(shardDir, "orphan-"+name+".idx"))
	}

	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if cerr := c.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	if !dictExists(dir, 1) {
		if atticHas(dir, 1) {
			t.Fatal("dictionary 1 was retired to the attic although segment files on disk name it")
		}
		t.Fatal("dictionary 1 is gone although segment files on disk name it")
	}

	// And the point of keeping it: repairing the names makes the records readable again.
	for _, name := range named {
		if rerr := os.Rename(filepath.Join(shardDir, "orphan-"+name),
			filepath.Join(shardDir, name)); rerr != nil {
			t.Fatal(rerr)
		}
		_ = os.Rename(filepath.Join(shardDir, "orphan-"+name+".idx"),
			filepath.Join(shardDir, name+".idx"))
	}
	c2, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatalf("reopen after repairing the names: %v", err)
	}
	defer c2.Close()
	if n := c2.Len(); n == 0 {
		t.Error("no records came back after repairing the names")
	}
}

// TestPruneStillRetiresTrulyDeadDictionary is the control. Keeping a dictionary that
// nothing names would turn the registry into the unbounded leak pruning exists to stop,
// so the new rule must not simply keep everything.
func TestPruneStillRetiresTrulyDeadDictionary(t *testing.T) {
	dir, shardDir := buildTwoDictStore(t)

	named := segmentsNaming(t, shardDir, 1)
	if len(named) == 0 {
		t.Fatal("fixture wrote no segment under dictionary 1")
	}
	// Remove them outright, as a reap does. Now nothing on disk or in memory names
	// dictionary 1 and it is genuinely dead.
	for _, name := range named {
		if err := os.Remove(filepath.Join(shardDir, name)); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(filepath.Join(shardDir, name+".idx"))
	}
	_ = os.Remove(filepath.Join(shardDir, dirSnapName))

	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if cerr := c.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	if dictExists(dir, 1) {
		t.Error("dictionary 1 was kept although nothing names it")
	}
	if !atticHas(dir, 1) {
		t.Error("dictionary 1 was not retired to the attic")
	}
}

// TestDictIDFromSegmentName pins the parse, including the deliberate looseness: a rewrite
// staged under another prefix still names a dictionary that must not be retired.
func TestDictIDFromSegmentName(t *testing.T) {
	for _, tc := range []struct {
		name string
		want uint32
		ok   bool
	}{
		{"seg-12.d3.dat", 3, true},
		{"seg-0.d0.dat", 0, true},
		{"tmp-merge-9.d17.dat", 17, true},
		{"orphan-seg-12.d3.dat", 3, true},
		{"seg-12.d3.dat.idx", 0, false},
		{"seg-12.d3.dat.merging", 0, false},
		{"dir.snap", 0, false},
		{"seg-12.dat", 0, false},
		{"seg-12.d.dat", 0, false},
		{"seg-12.dx.dat", 0, false},
		{"seg-12.d4294967296.dat", 0, false}, // past uint32
	} {
		got, ok := dictIDFromSegmentName(tc.name)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("dictIDFromSegmentName(%q) = (%d, %v); want (%d, %v)",
				tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// TestCollectSegmentDictIDsIgnoresUnreadableDir pins the fallback: an unreadable shard
// directory contributes nothing rather than failing the pass, leaving the caller with the
// in-memory answer it had before.
func TestCollectSegmentDictIDsIgnoresUnreadableDir(t *testing.T) {
	into := map[uint32]bool{}
	collectSegmentDictIDs(filepath.Join(t.TempDir(), "does-not-exist"), into)
	if len(into) != 0 {
		t.Errorf("collected %v from a missing directory", into)
	}
}
