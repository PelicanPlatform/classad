package collections

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestPersistentBasic exercises a persistent (mmap-backed) collection end to end
// while open: writes land in segment files on disk, and Get/Scan/Query work.
// (Recovery on reopen is a later milestone.)
func TestPersistentBasic(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	c, err := Open(Options{Shards: 4, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if c.dir != dir {
		t.Fatalf("dir = %q, want %q", c.dir, dir)
	}

	const n = 2000
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAd(t, fmt.Sprintf(`[Id=%d; Cpus=%d; Owner=%q]`, i, i%8, []string{"alice", "bob"}[i%2]))); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if c.Len() != n {
		t.Fatalf("Len = %d, want %d", c.Len(), n)
	}

	// Segment files exist on disk under the shard subdirs.
	segFiles := 0
	err = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Ext(p) == ".dat" {
			segFiles++
			if info.Size() == 0 {
				t.Errorf("empty segment file %s", p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if segFiles == 0 {
		t.Fatal("no segment files created on disk")
	}
	t.Logf("%d ads across %d on-disk segment files", n, segFiles)

	// Reads work against the mmap-backed store.
	for i := 0; i < n; i++ {
		ad, ok := c.Get([]byte(fmt.Sprintf("k%d", i)))
		if !ok {
			t.Fatalf("k%d missing", i)
		}
		if id, _ := ad.EvaluateAttrInt("Id"); id != int64(i) {
			t.Fatalf("k%d Id=%d", i, id)
		}
	}
	seen := 0
	for range c.Scan() {
		seen++
	}
	if seen != n {
		t.Fatalf("scan yielded %d, want %d", seen, n)
	}
	q, _ := vm.Parse(`Cpus >= 4 && Owner == "alice"`)
	got, want := 0, 0
	for i := 0; i < n; i++ {
		if i%8 >= 4 && i%2 == 0 {
			want++
		}
	}
	for range c.Query(q) {
		got++
	}
	if got != want {
		t.Fatalf("query matched %d, want %d", got, want)
	}

	// Close flushes and unmaps.
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestPersistentEmptyDirIsInMemory verifies Open with no Dir behaves like New.
func TestPersistentEmptyDirIsInMemory(t *testing.T) {
	c, err := Open(Options{Shards: 2})
	if err != nil {
		t.Fatal(err)
	}
	if c.dir != "" {
		t.Fatal("expected in-memory collection")
	}
	if err := c.Put([]byte("k"), mustAd(t, `[A=1]`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get([]byte("k")); !ok {
		t.Fatal("k missing")
	}
	if err := c.Close(); err != nil { // no-op for in-memory
		t.Fatalf("close: %v", err)
	}
}
