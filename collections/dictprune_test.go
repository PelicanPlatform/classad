package collections

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// countDictFiles returns the number of <dir>/dicts/*.zst files.
func countDictFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "dicts"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".zst" {
			n++
		}
	}
	return n
}

// TestRetrainPrunesOldDicts pins the registry/disk lifecycle of trained dictionaries:
// after a retrain recompacts every segment to the new codec, the superseded
// dictionaries are dropped from the registry and their .zst files unlinked. Without
// this the registry grew by one zstd encoder/decoder pair per retrain per table for
// the life of the daemon -- the live-heap leak observed in production (~25 accumulated
// dictionaries per table) -- and a reopen reloaded the entire history.
func TestRetrainPrunesOldDicts(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	sample := loadCorpus(t)
	dir := t.TempDir()
	c, err := Open(Options{Shards: 2, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	const n = 2000
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), sample[i%len(sample)]); err != nil {
			t.Fatal(err)
		}
	}

	for round := 0; round < 3; round++ {
		if _, err := c.RetrainDict(1500); err != nil {
			t.Skipf("RetrainDict unavailable on this corpus: %v", err)
		}
	}

	// Only the current dictionary should survive: the registry holds base + current,
	// and the dicts dir holds one .zst.
	c.dicts.mu.Lock()
	regSize := len(c.dicts.byID)
	c.dicts.mu.Unlock()
	if regSize != 2 {
		t.Errorf("registry holds %d codecs after 3 retrains, want 2 (base + current)", regSize)
	}
	if files := countDictFiles(t, dir); files != 1 {
		t.Errorf("dicts dir holds %d .zst files after 3 retrains, want 1", files)
	}
	if c.Len() != n {
		t.Fatalf("Len=%d want %d after retrains", c.Len(), n)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: data intact under the surviving dictionary, registry stays pruned.
	c2, err := Open(Options{Shards: 2, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if c2.Len() != n {
		t.Fatalf("reopen Len=%d want %d", c2.Len(), n)
	}
	if _, ok := c2.Get([]byte("k42")); !ok {
		t.Error("k42 missing after reopen")
	}
	c2.dicts.mu.Lock()
	regSize2 := len(c2.dicts.byID)
	c2.dicts.mu.Unlock()
	if regSize2 != 2 {
		t.Errorf("reopened registry holds %d codecs, want 2", regSize2)
	}
}
