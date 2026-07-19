package collections

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestDirSnapshotFastReopen proves the clean-shutdown fast reopen: after a clean
// Close, a reopen restores every shard's directory from its snapshot without the
// full record scan (rebuildDir), and the recovered state -- including deletes -- is
// exactly correct.
func TestDirSnapshotFastReopen(t *testing.T) {
	var loaded, scanned int
	onDirSnapLoad = func(ok bool) {
		if ok {
			loaded++
		} else {
			scanned++
		}
	}
	defer func() { onDirSnapLoad = nil }()

	dir := t.TempDir()
	openIt := func() *Collection {
		c, err := Open(Options{Dir: dir, Shards: 4, SegmentSize: 4096})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	c := openIt()
	for i := 0; i < 400; i++ {
		ad, err := classad.Parse(fmt.Sprintf(`[ Owner="u%d"; Seq=%d ]`, i%7, i))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 60; i++ { // delete a chunk so the fast path must honor tombstones
		c.Delete([]byte(fmt.Sprintf("k%d", i)))
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// --- Clean reopen: every shard takes the snapshot fast path (no scan). ---
	loaded, scanned = 0, 0
	c2 := openIt()
	nShards := len(c2.shards)
	if scanned != 0 || loaded != nShards {
		t.Fatalf("clean reopen: loaded=%d scanned=%d, want loaded=%d scanned=0", loaded, scanned, nShards)
	}
	if got := c2.Len(); got != 340 {
		t.Fatalf("reopened Len=%d, want 340 (400 put, 60 deleted)", got)
	}
	for i := 60; i < 400; i++ {
		ad, ok := c2.Get([]byte(fmt.Sprintf("k%d", i)))
		if !ok {
			t.Fatalf("k%d missing after fast reopen", i)
		}
		if v, _ := ad.EvaluateAttrInt("Seq"); v != int64(i) {
			t.Fatalf("k%d Seq=%d, want %d", i, v, i)
		}
	}
	for i := 0; i < 60; i++ {
		if _, ok := c2.Get([]byte(fmt.Sprintf("k%d", i))); ok {
			t.Fatalf("deleted k%d resurfaced after fast reopen", i)
		}
	}
	if err := c2.Close(); err != nil {
		t.Fatal(err)
	}

	// --- Corrupt one shard's snapshot: that shard must fall back to the scan and
	// still recover correctly; the others still fast-load. ---
	snap := filepath.Join(dir, "0", dirSnapName)
	if err := os.WriteFile(snap, []byte("garbage-not-a-valid-snapshot"), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, scanned = 0, 0
	c3 := openIt()
	defer c3.Close()
	if scanned < 1 {
		t.Fatalf("corrupted shard should have fallen back to a scan; scanned=%d", scanned)
	}
	if loaded+scanned != nShards {
		t.Fatalf("each shard should load-or-scan exactly once: loaded=%d scanned=%d", loaded, scanned)
	}
	if got := c3.Len(); got != 340 {
		t.Fatalf("after corrupt-snapshot fallback, Len=%d, want 340", got)
	}
	// Spot-check a key that hashes into the corrupted shard 0 recovered correctly.
	for i := 60; i < 400; i++ {
		if _, ok := c3.Get([]byte(fmt.Sprintf("k%d", i))); !ok {
			t.Fatalf("k%d lost after corrupt-snapshot fallback", i)
		}
	}
}
