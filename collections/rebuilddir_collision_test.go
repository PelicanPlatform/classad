package collections

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// collidingHasher forces many distinct keys onto a handful of hashes, so recovery's
// hash-keyed winner map has to tell a repeat of one key apart from a different key that
// merely hashes the same. The real hasher makes that path effectively unreachable -- one
// collision in a store of millions -- which is exactly why it needs forcing here: merging
// two keys does not fail loudly, it drops one of them.
type collidingHasher struct{ buckets uint64 }

func (c collidingHasher) Hash(key []byte) uint64 {
	var h uint64 = 14695981039346656037
	for _, b := range key {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return h % c.buckets
}

// TestRebuildDirSurvivesHashCollisions writes keys that collide heavily, forces the record
// walk by removing the directory snapshot, and requires every surviving key to come back
// with its last value -- and every deleted one to stay gone.
//
// NOT parallel: it installs onDirSnapLoad, which is a package global, so a concurrent test
// that also reopens a store would be counted here.
func TestRebuildDirSurvivesHashCollisions(t *testing.T) {
	const keys = 60
	dir := t.TempDir()
	opts := Options{Dir: dir, Shards: 1, SegmentSize: 1 << 12, Hasher: collidingHasher{buckets: 3}}

	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	// Three versions of every key, so the winner map sees each hash many times over.
	for v := 1; v <= 3; v++ {
		for i := range keys {
			ad, err := classad.Parse(fmt.Sprintf(`[ Owner="u%d"; Version=%d ]`, i, v))
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Put(fmt.Appendf(nil, "k%02d", i), ad); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := 0; i < keys; i += 5 { // delete a slice of them, colliding ones included
		if !c.Delete(fmt.Appendf(nil, "k%02d", i)) {
			t.Fatalf("k%02d: delete reported nothing removed", i)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Drop the snapshot so the reopen takes the record walk (rebuildDir), not the fast path.
	if err := os.Remove(filepath.Join(dir, "0", dirSnapName)); err != nil {
		t.Fatal(err)
	}
	var scanned int
	onDirSnapLoad = func(ok bool) {
		if !ok {
			scanned++
		}
	}
	defer func() { onDirSnapLoad = nil }()

	c2, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if scanned != 1 {
		t.Fatalf("expected the shard to rebuild by walking, got scanned=%d", scanned)
	}

	want := 0
	for i := range keys {
		key := fmt.Appendf(nil, "k%02d", i)
		ad, ok := c2.Get(key)
		if i%5 == 0 {
			if ok {
				t.Fatalf("k%02d was deleted but came back after recovery", i)
			}
			continue
		}
		want++
		if !ok {
			t.Fatalf("k%02d is missing after recovery: a colliding key was dropped", i)
		}
		owner, _ := ad.EvaluateAttrString("Owner")
		if owner != fmt.Sprintf("u%d", i) {
			t.Fatalf("k%02d came back as %q: a colliding key overwrote another", i, owner)
		}
		if v, _ := ad.EvaluateAttrInt("Version"); v != 3 {
			t.Fatalf("k%02d came back at Version=%d, want the last one (3)", i, v)
		}
	}
	if got := c2.Len(); got != want {
		t.Fatalf("Len()=%d after recovery, want %d", got, want)
	}
}
