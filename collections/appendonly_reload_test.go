package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestAppendOnlyReload verifies a persistent append-only collection survives close+reopen with
// every record (including intentional duplicate keys) still live -- the reload must not apply
// the mutable store's single-current-version dedup.
func TestAppendOnlyReload(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{AppendOnly: true, Dir: dir, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatal(err)
	}
	// 50 appends, all under the SAME key (a log with heavy key collision): every one must persist.
	for i := 0; i < 50; i++ {
		ad, perr := classad.Parse(fmt.Sprintf(`[ N = %d; Owner = "u%d" ]`, i, i%3))
		if perr != nil {
			t.Fatal(perr)
		}
		if err := c.Put([]byte("dup"), ad); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2, err := Open(Options{AppendOnly: true, Dir: dir, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	n := 0
	seen := map[int64]bool{}
	for ad := range c2.Scan() {
		n++
		if v, ok := ad.EvaluateAttrInt("N"); ok {
			seen[v] = true
		}
	}
	if n != 50 {
		t.Fatalf("after reload scan = %d records, want 50 (no dedup on an append log)", n)
	}
	if len(seen) != 50 {
		t.Errorf("distinct N values after reload = %d, want 50 (each append preserved)", len(seen))
	}
	// The reopened log keeps appending correctly.
	ad, _ := classad.Parse(`[ N = 999 ]`)
	if err := c2.Put([]byte("dup"), ad); err != nil {
		t.Fatal(err)
	}
	n = 0
	for range c2.Scan() {
		n++
	}
	if n != 51 {
		t.Errorf("after post-reload append scan = %d, want 51", n)
	}
}
