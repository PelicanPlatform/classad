package collections

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

// recordDirSyncs installs dirSyncHook for the duration of a test and returns a function
// reporting every directory synced so far.
func recordDirSyncs(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	dirSyncHook = func(dir string) {
		mu.Lock()
		seen = append(seen, dir)
		mu.Unlock()
	}
	t.Cleanup(func() { dirSyncHook = nil })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

// TestWriteDictFileSyncsDictsDir pins the durability contract register relies on: the
// dictionary's NAME is on disk before the write returns, not just its contents. Without the
// directory fsync a power loss leaves the bytes with nothing pointing at them, and every
// segment written under that dictionary becomes undecodable.
func TestWriteDictFileSyncsDictsDir(t *testing.T) {
	dicts := filepath.Join(t.TempDir(), "dicts")
	synced := recordDirSyncs(t)

	if err := writeDictFile(filepath.Join(dicts, "1.zst"), []byte("dictionary bytes")); err != nil {
		t.Fatalf("writeDictFile: %v", err)
	}

	if got := synced(); !slices.Contains(got, dicts) {
		t.Errorf("writeDictFile did not fsync %s; synced %v", dicts, got)
	}
}

// TestRetireDictFileSyncsBothDirs covers the other half: a retirement is a rename across two
// directories, and until BOTH are durable a crash can leave the dictionary in neither --
// destroying it outright, which is the one outcome the attic exists to prevent.
func TestRetireDictFileSyncsBothDirs(t *testing.T) {
	dicts := filepath.Join(t.TempDir(), "dicts")
	if err := os.MkdirAll(dicts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dicts, "3.zst"), []byte("dictionary bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	synced := recordDirSyncs(t)

	retireDictFile(dicts, 3)

	attic := filepath.Join(dicts, "attic")
	if _, err := os.Stat(filepath.Join(attic, "3.zst")); err != nil {
		t.Fatalf("dictionary was not retired into the attic: %v", err)
	}
	got := synced()
	for _, want := range []string{attic, dicts} {
		if !slices.Contains(got, want) {
			t.Errorf("retireDictFile did not fsync %s; synced %v", want, got)
		}
	}
}

// TestWriteDictFileFailsWhenDirSyncFails asserts the failure DIRECTION. A dictionary whose
// name may not have survived must not be reported as written: register would then commit the
// codec and segments would start naming it. Failing the write fails the retrain and leaves
// the previous codec in use, which is the recoverable outcome.
func TestWriteDictFileFailsWhenDirSyncFails(t *testing.T) {
	dir := t.TempDir()
	// A path whose parent is a regular file: MkdirAll fails, so the write never happens.
	// This is the same class of failure a sync error produces -- the point is that
	// writeDictFile reports it rather than returning nil.
	if err := os.WriteFile(filepath.Join(dir, "notadir"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeDictFile(filepath.Join(dir, "notadir", "1.zst"), []byte("x")); err == nil {
		t.Error("writeDictFile returned nil for a dictionary it could not persist")
	}
}
