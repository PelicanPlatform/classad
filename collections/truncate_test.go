package collections

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestTruncatePersistent verifies Truncate empties the store, reclaims its segment
// files, and leaves it writable (a fresh segment allocates without colliding with a
// retired file, since file names use a monotonic counter).
func TestTruncatePersistent(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	c, err := Open(Options{Shards: 2, Dir: dir, SegmentSize: 1 << 13})
	if err != nil {
		t.Fatal(err)
	}
	const n = 300
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	if c.Len() != n {
		t.Fatalf("Len before = %d, want %d", c.Len(), n)
	}

	c.Truncate()

	if c.Len() != 0 {
		t.Fatalf("Len after Truncate = %d, want 0", c.Len())
	}
	got := 0
	for range c.Scan() {
		got++
	}
	if got != 0 {
		t.Fatalf("scan after Truncate yielded %d ads, want 0", got)
	}
	// Segment files are reclaimed across both shards.
	for s := 0; s < 2; s++ {
		if fn := countSegFiles(t, filepath.Join(dir, fmt.Sprintf("%d", s))); fn != 0 {
			t.Errorf("shard %d has %d seg files after Truncate, want 0", s, fn)
		}
	}

	// Still writable, and reopen recovers exactly the post-truncate writes.
	for i := 0; i < 10; i++ {
		if err := c.Put([]byte(fmt.Sprintf("new%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	if c.Len() != 10 {
		t.Fatalf("Len after re-put = %d, want 10", c.Len())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c2, err := Open(Options{Shards: 2, Dir: dir, SegmentSize: 1 << 13})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if c2.Len() != 10 {
		t.Fatalf("Len after reopen = %d, want 10 (only post-truncate writes survive)", c2.Len())
	}
	if _, ok := c2.Get([]byte("k0")); ok {
		t.Error("a pre-truncate key survived reopen")
	}
	if _, ok := c2.Get([]byte("new0")); !ok {
		t.Error("a post-truncate key is missing after reopen")
	}
}
